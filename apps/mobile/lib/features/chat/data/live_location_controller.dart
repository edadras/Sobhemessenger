import 'dart:async';
import 'dart:convert';

import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:geolocator/geolocator.dart';

import '../../../core/network/api_client.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../auth/session_controller.dart';

/// Keeps shared live locations moving (§12).
///
/// Sending a live location is only the beginning of it: the message carries a
/// deadline, and until that deadline passes the sender is promising the other
/// side a current position. Without something pushing updates, the pin sits
/// where it was first shared while the app claims it is live — which is worse
/// than not offering the feature, because the recipient believes it.
///
/// The set of active shares is read from the local database rather than held
/// in memory, so a share survives the app being closed and reopened, and a
/// message still waiting in the outbox is picked up on a later pass once the
/// server has given it an id.
class LiveLocationController {
  LiveLocationController({
    required LocalDatabase database,
    required ApiClient api,
  })  : _db = database,
        _api = api;

  final LocalDatabase _db;
  final ApiClient _api;

  StreamSubscription<Position>? _positions;
  Timer? _heartbeat;
  bool _pushing = false;
  bool _started = false;

  /// A stationary sender still has to report in. If the pin only moved when
  /// the device did, a recipient could not tell "standing still" from "the
  /// phone is off", which is the difference that matters when someone is
  /// waiting for you.
  static const Duration _heartbeatInterval = Duration(seconds: 45);

  /// Metres of movement before a new fix is delivered. Small enough to follow
  /// someone walking, large enough that GPS jitter does not spend the battery
  /// reporting a stationary phone.
  static const int _distanceFilterMetres = 15;

  /// Begins following any share that is already running.
  ///
  /// Safe to call repeatedly — at launch, and again whenever a share starts.
  Future<void> start() async {
    if (_started) {
      await _refresh();
      return;
    }
    _started = true;
    _heartbeat = Timer.periodic(_heartbeatInterval, (_) => unawaited(_push()));
    await _refresh();
  }

  /// Ends one share, both on the server and in the local copy.
  ///
  /// The server refusing because the share has already finished is not a
  /// failure to report: the outcome the caller asked for is the outcome they
  /// have.
  Future<void> stopSharing(String messageId) async {
    try {
      await _api.delete<dynamic>('/messages/$messageId/live-location');
    } on ApiException catch (error) {
      if (!_isFinished(error)) {
        rethrow;
      }
    }
    await _db.endLiveLocation(messageId);
    await _refresh();
  }

  /// Pushes a fix now — used when a share has just started, so the first
  /// update does not wait for the heartbeat.
  Future<void> refresh() => _refresh();

  Future<void> _refresh() async {
    final List<String> active = await _activeShareIds();
    if (active.isEmpty) {
      // Nothing to follow: stop draining the battery until there is.
      await _positions?.cancel();
      _positions = null;
      return;
    }
    _positions ??= Geolocator.getPositionStream(
      locationSettings: const LocationSettings(
        accuracy: LocationAccuracy.high,
        distanceFilter: _distanceFilterMetres,
      ),
    ).listen(
      (Position _) => unawaited(_push()),
      // A location stream can end because permission was withdrawn while the
      // app was in the background. That stops the updates, not the share: the
      // server expires it on its own deadline, and the heartbeat will try
      // again if permission comes back.
      onError: (Object _) {
        _positions?.cancel();
        _positions = null;
      },
    );
    await _push();
  }

  /// Sends the current position to every share that is still running.
  Future<void> _push() async {
    // Overlapping passes would send the same fix twice and, worse, race each
    // other's local writes when a share turns out to be over.
    if (_pushing) {
      return;
    }
    _pushing = true;
    try {
      final List<String> active = await _activeShareIds();
      if (active.isEmpty) {
        await _positions?.cancel();
        _positions = null;
        return;
      }

      final Position? position = await _currentPosition();
      if (position == null) {
        return;
      }

      for (final String messageId in active) {
        await _send(messageId, position);
      }
    } finally {
      _pushing = false;
    }
  }

  Future<Position?> _currentPosition() async {
    try {
      return await Geolocator.getCurrentPosition(
        locationSettings: const LocationSettings(
          accuracy: LocationAccuracy.high,
        ),
      );
    } on Exception {
      // No fix available this time. The share is still valid and the next
      // heartbeat may do better; giving up on it here would end a share the
      // user did not end.
      return null;
    }
  }

  Future<void> _send(String messageId, Position position) async {
    try {
      await _api.put<dynamic>(
        '/messages/$messageId/live-location',
        body: <String, dynamic>{
          'latitude': position.latitude,
          'longitude': position.longitude,
          'horizontal_accuracy': position.accuracy,
          'heading': position.heading,
          'speed': position.speed,
        },
      );
    } on ApiException catch (error) {
      if (_isFinished(error)) {
        // The server has the last word on when a share ends — it expired, or
        // another device stopped it. Record that locally so this device stops
        // asking and stops drawing it as live.
        await _db.endLiveLocation(messageId);
        return;
      }
      if (!error.isRetryable) {
        rethrow;
      }
      // A network failure is ordinary while moving. The next fix carries a
      // better position than this one would have, so nothing is queued.
    }
  }

  /// Server ids of the shares this device should still be moving.
  Future<List<String>> _activeShareIds() async {
    final List<MessageRow> rows = await _db.liveLocationMessages();
    final DateTime now = DateTime.now();

    return <String>[
      for (final MessageRow row in rows)
        if (_liveUntil(row.payloadJson)?.isAfter(now) ?? false)
          // A row still carrying its client id has not been acknowledged yet,
          // so there is nothing on the server to move.
          if (row.id != row.clientMessageId) row.id,
    ];
  }

  static DateTime? _liveUntil(String? payloadJson) {
    if (payloadJson == null) {
      return null;
    }
    final Object? decoded = jsonDecode(payloadJson);
    if (decoded is! Map<String, dynamic>) {
      return null;
    }
    final Object? until = decoded['live_until'];
    if (until is! String) {
      return null;
    }
    return DateTime.tryParse(until)?.toLocal();
  }

  /// Whether the server's refusal means the share is over rather than that
  /// something went wrong.
  static bool _isFinished(ApiException error) =>
      error.statusCode == 404 || error.statusCode == 409;

  void dispose() {
    _heartbeat?.cancel();
    _heartbeat = null;
    unawaited(_positions?.cancel());
    _positions = null;
    _started = false;
  }
}

final Provider<LiveLocationController> liveLocationControllerProvider =
    Provider<LiveLocationController>((Ref ref) {
  final LiveLocationController controller = LiveLocationController(
    database: ref.watch(localDatabaseProvider),
    api: ref.watch(apiClientProvider),
  );
  ref.onDispose(controller.dispose);
  return controller;
});
