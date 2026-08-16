import 'dart:async';
import 'dart:convert';
import 'dart:math';

import 'package:web_socket_channel/web_socket_channel.dart';

import '../config/app_config.dart';
import '../storage/token_store.dart';

/// Connection state surfaced to the UI (§8).
enum SocketStatus {
  disconnected,
  connecting,
  connected,
  syncing,
  waitingForNetwork
}

/// One frame of the SOBH WebSocket protocol.
class SocketFrame {
  const SocketFrame({
    required this.event,
    this.id,
    this.payload = const <String, dynamic>{},
    this.syncSeq,
    this.errorCode,
    this.errorMessage,
  });

  factory SocketFrame.fromJson(Map<String, dynamic> json) => SocketFrame(
        id: json['id'] as String?,
        event: json['event'] as String? ?? '',
        payload: (json['payload'] as Map<String, dynamic>?) ??
            const <String, dynamic>{},
        syncSeq: (json['sync_seq'] as num?)?.toInt(),
        errorCode: (json['error'] as Map<String, dynamic>?)?['code'] as String?,
        errorMessage:
            (json['error'] as Map<String, dynamic>?)?['message'] as String?,
      );

  final String? id;
  final String event;
  final Map<String, dynamic> payload;
  final int? syncSeq;
  final String? errorCode;
  final String? errorMessage;

  bool get isError => errorCode != null;

  Map<String, dynamic> toJson() => <String, dynamic>{
        if (id != null) 'id': id,
        'event': event,
        'payload': payload,
      };
}

/// Raised when the server rejects a request frame.
class SocketException implements Exception {
  const SocketException(this.code, this.message);

  final String code;
  final String message;

  @override
  String toString() => 'SocketException($code): $message';
}

/// The realtime connection to SOBH.
///
/// Owns reconnection with exponential backoff, request/response correlation,
/// and the resume-from-cursor handshake. It deliberately knows nothing about
/// chats or messages: it moves frames, and the repositories interpret them.
class SocketClient {
  SocketClient({
    required AppConfig config,
    required TokenStore tokenStore,
  })  : _config = config,
        _tokenStore = tokenStore;

  final AppConfig _config;
  final TokenStore _tokenStore;

  WebSocketChannel? _channel;
  StreamSubscription<dynamic>? _subscription;
  Timer? _reconnectTimer;
  Timer? _heartbeatTimer;

  int _reconnectAttempt = 0;
  bool _disposed = false;
  bool _intentionallyClosed = false;

  /// Correlates a sent frame with its acknowledgement.
  final Map<String, Completer<SocketFrame>> _pending =
      <String, Completer<SocketFrame>>{};
  int _requestCounter = 0;

  final StreamController<SocketFrame> _events =
      StreamController<SocketFrame>.broadcast();
  final StreamController<SocketStatus> _status =
      StreamController<SocketStatus>.broadcast();

  /// Server-initiated events. Acknowledgements are routed to their caller and
  /// never appear here.
  Stream<SocketFrame> get events => _events.stream;
  Stream<SocketStatus> get status => _status.stream;

  SocketStatus _currentStatus = SocketStatus.disconnected;
  SocketStatus get currentStatus => _currentStatus;

  /// The device's position in its event log, from the `connected` frame.
  int syncCursor = 0;
  int serverSeq = 0;

  /// Backoff bounds (§8). Jitter stops every client reconnecting in lockstep
  /// after a server restart.
  static const Duration _minBackoff = Duration(seconds: 1);
  static const Duration _maxBackoff = Duration(seconds: 60);
  static const Duration _requestTimeout = Duration(seconds: 20);
  static const Duration _appHeartbeat = Duration(seconds: 30);

  Future<void> connect() async {
    if (_disposed || _currentStatus == SocketStatus.connecting) {
      return;
    }
    _intentionallyClosed = false;
    _setStatus(SocketStatus.connecting);

    final String? token = await _tokenStore.readAccessToken();
    if (token == null) {
      _setStatus(SocketStatus.disconnected);
      return;
    }

    final Uri uri = Uri.parse(_config.wsUrl).replace(
      queryParameters: <String, String>{
        'token': token,
        'protocol_version': '${AppConfig.protocolVersion}',
      },
    );

    try {
      final WebSocketChannel channel = WebSocketChannel.connect(uri);
      await channel.ready;
      _channel = channel;

      _subscription = channel.stream.listen(
        _onData,
        onError: (Object error) => _onDisconnected(),
        onDone: _onDisconnected,
        cancelOnError: true,
      );

      _reconnectAttempt = 0;
      _startHeartbeat();
    } on Object {
      // Any failure here is a connection failure; the retry loop handles it.
      _scheduleReconnect();
    }
  }

  void _onData(dynamic raw) {
    if (raw is! String) {
      return;
    }

    final SocketFrame frame;
    try {
      frame = SocketFrame.fromJson(jsonDecode(raw) as Map<String, dynamic>);
    } on FormatException {
      return;
    }

    if (frame.event == 'connected') {
      syncCursor = (frame.payload['sync_cursor'] as num?)?.toInt() ?? 0;
      serverSeq = (frame.payload['server_seq'] as num?)?.toInt() ?? 0;
      _setStatus(
        syncCursor < serverSeq ? SocketStatus.syncing : SocketStatus.connected,
      );
      _events.add(frame);
      return;
    }

    // Route acknowledgements back to whoever is waiting for them.
    final String? id = frame.id;
    if (id != null) {
      final Completer<SocketFrame>? completer = _pending.remove(id);
      if (completer != null && !completer.isCompleted) {
        if (frame.isError) {
          completer.completeError(
            SocketException(
              frame.errorCode!,
              frame.errorMessage ?? 'Request failed',
            ),
          );
        } else {
          completer.complete(frame);
        }
        return;
      }
    }

    _events.add(frame);
  }

  /// Sends a frame and waits for its acknowledgement.
  Future<SocketFrame> request(String event, Map<String, dynamic> payload) {
    final WebSocketChannel? channel = _channel;
    if (channel == null || _currentStatus == SocketStatus.disconnected) {
      return Future<SocketFrame>.error(
        const SocketException(
          'NOT_CONNECTED',
          'The realtime connection is not open',
        ),
      );
    }

    final String id =
        '${DateTime.now().microsecondsSinceEpoch}-${_requestCounter++}';
    final Completer<SocketFrame> completer = Completer<SocketFrame>();
    _pending[id] = completer;

    channel.sink.add(
      jsonEncode(
        SocketFrame(id: id, event: event, payload: payload).toJson(),
      ),
    );

    return completer.future.timeout(
      _requestTimeout,
      onTimeout: () {
        _pending.remove(id);
        throw const SocketException(
          'TIMEOUT',
          'The server did not acknowledge in time',
        );
      },
    );
  }

  /// Sends a frame without waiting — typing indicators and sync acks, where a
  /// lost frame costs nothing.
  void send(String event, Map<String, dynamic> payload) {
    _channel?.sink
        .add(jsonEncode(SocketFrame(event: event, payload: payload).toJson()));
  }

  /// Requests missed events after [cursor] (§9).
  Future<SocketFrame> requestSync(int cursor, {int limit = 200}) => request(
        'sync.request',
        <String, dynamic>{'cursor': cursor, 'limit': limit},
      );

  /// Confirms events up to [cursor] have been applied. Until this is sent the
  /// server will redeliver them, which is what makes a crash mid-apply safe.
  void acknowledgeSync(int cursor) {
    syncCursor = cursor;
    send('sync.ack', <String, dynamic>{'cursor': cursor});
  }

  /// Subscribes to a large chat's live feed (§8).
  Future<void> subscribeToChat(String chatId) =>
      request('chat.subscribe', <String, dynamic>{'chat_id': chatId});

  Future<void> unsubscribeFromChat(String chatId) =>
      request('chat.unsubscribe', <String, dynamic>{'chat_id': chatId});

  void _startHeartbeat() {
    _heartbeatTimer?.cancel();
    // The server sends control-frame pings, but mobile platforms suspend timers
    // in the background; an application-level ping detects a dead socket that
    // the OS has not yet reported.
    _heartbeatTimer = Timer.periodic(_appHeartbeat, (_) {
      if (_currentStatus == SocketStatus.connected) {
        send('ping', const <String, dynamic>{});
      }
    });
  }

  void _onDisconnected() {
    _heartbeatTimer?.cancel();
    _channel = null;

    // Anything still waiting will never be answered on this connection.
    for (final Completer<SocketFrame> completer in _pending.values) {
      if (!completer.isCompleted) {
        completer.completeError(
          const SocketException(
            'DISCONNECTED',
            'The connection closed before a reply arrived',
          ),
        );
      }
    }
    _pending.clear();

    if (_intentionallyClosed || _disposed) {
      _setStatus(SocketStatus.disconnected);
      return;
    }
    _scheduleReconnect();
  }

  void _scheduleReconnect() {
    _setStatus(SocketStatus.waitingForNetwork);
    _reconnectTimer?.cancel();

    final Duration delay = _backoffDelay(_reconnectAttempt);
    _reconnectAttempt++;

    _reconnectTimer = Timer(delay, () {
      if (!_disposed && !_intentionallyClosed) {
        unawaited(connect());
      }
    });
  }

  /// `min(60s, 1s * 2^attempt)` with ±50% jitter.
  Duration _backoffDelay(int attempt) {
    final int exponential =
        _minBackoff.inMilliseconds * (1 << attempt.clamp(0, 6));
    final int capped = min(exponential, _maxBackoff.inMilliseconds);
    final double jitter = 0.5 + Random().nextDouble();
    return Duration(milliseconds: (capped * jitter).round());
  }

  void _setStatus(SocketStatus status) {
    if (_currentStatus == status) {
      return;
    }
    _currentStatus = status;
    if (!_status.isClosed) {
      _status.add(status);
    }
  }

  /// Marks the initial synchronisation complete.
  void markSynced() => _setStatus(SocketStatus.connected);

  Future<void> disconnect() async {
    _intentionallyClosed = true;
    _reconnectTimer?.cancel();
    _heartbeatTimer?.cancel();
    await _subscription?.cancel();
    await _channel?.sink.close();
    _channel = null;
    _setStatus(SocketStatus.disconnected);
  }

  Future<void> dispose() async {
    _disposed = true;
    await disconnect();
    await _events.close();
    await _status.close();
  }
}
