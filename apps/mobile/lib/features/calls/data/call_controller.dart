import 'dart:async';

import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_webrtc/flutter_webrtc.dart';

import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';
import 'calls_repository.dart';

/// Where a call is in its lifecycle, from this device's point of view.
enum CallPhase {
  /// Ringing, waiting for the other side to accept.
  dialling,

  /// Someone is calling us and we have not answered.
  ringing,

  /// Accepted; the peer connection is negotiating.
  connecting,

  /// Media is flowing.
  connected,

  /// The ICE connection dropped and is being re-established.
  reconnecting,

  /// Over, for any reason.
  ended,
}

/// Everything the call screen renders.
class CallSession {
  const CallSession({
    required this.callId,
    required this.phase,
    required this.isVideo,
    required this.isOutgoing,
    required this.peerName,
    this.muted = false,
    this.cameraOn = false,
    this.speakerOn = false,
    this.startedAt,
    this.connectedAt,
    this.endReason = '',
  });

  final String callId;
  final CallPhase phase;
  final bool isVideo;
  final bool isOutgoing;
  final String peerName;
  final bool muted;
  final bool cameraOn;
  final bool speakerOn;
  final DateTime? startedAt;
  final DateTime? connectedAt;
  final String endReason;

  Duration get elapsed => connectedAt == null
      ? Duration.zero
      : DateTime.now().difference(connectedAt!);

  CallSession copyWith({
    CallPhase? phase,
    bool? muted,
    bool? cameraOn,
    bool? speakerOn,
    DateTime? connectedAt,
    String? endReason,
  }) =>
      CallSession(
        callId: callId,
        phase: phase ?? this.phase,
        isVideo: isVideo,
        isOutgoing: isOutgoing,
        peerName: peerName,
        muted: muted ?? this.muted,
        cameraOn: cameraOn ?? this.cameraOn,
        speakerOn: speakerOn ?? this.speakerOn,
        startedAt: startedAt,
        connectedAt: connectedAt ?? this.connectedAt,
        endReason: endReason ?? this.endReason,
      );
}

/// Drives one WebRTC call (§18).
///
/// The division of work mirrors the server's: the server relays SDP and ICE
/// without parsing them, and this class is the only place that produces or
/// consumes them. Nothing above it knows what an offer is.
///
/// Perfect negotiation is not needed here because the roles are fixed: the
/// caller always offers and the callee always answers, so there is no glare to
/// resolve.
class CallController extends StateNotifier<CallSession?> {
  CallController({
    required CallsRepository calls,
    required SocketClient socket,
    required String selfUserId,
  })  : _calls = calls,
        _socket = socket,
        _selfUserId = selfUserId,
        super(null) {
    _events = _socket.events.listen(_onSocketEvent);
  }

  final CallsRepository _calls;
  final SocketClient _socket;
  final String _selfUserId;

  late final StreamSubscription<SocketFrame> _events;

  RTCPeerConnection? _peer;
  MediaStream? _localStream;
  MediaStream? _remoteStream;
  String? _peerUserId;

  /// ICE candidates that arrived before the remote description was set.
  ///
  /// addCandidate throws if it is called before the remote description exists,
  /// and signalling gives no ordering guarantee, so they are held here.
  final List<RTCIceCandidate> _pendingCandidates = <RTCIceCandidate>[];
  bool _remoteDescriptionSet = false;

  final StreamController<MediaStream?> _localStreamController =
      StreamController<MediaStream?>.broadcast();
  final StreamController<MediaStream?> _remoteStreamController =
      StreamController<MediaStream?>.broadcast();

  Stream<MediaStream?> get localStream => _localStreamController.stream;
  Stream<MediaStream?> get remoteStream => _remoteStreamController.stream;

  MediaStream? get currentLocalStream => _localStream;
  MediaStream? get currentRemoteStream => _remoteStream;

  /// Places a call and starts ringing the other side.
  Future<void> place({
    required String chatId,
    required String peerName,
    required bool video,
  }) async {
    final Call call = await _calls.start(chatId: chatId, video: video);
    _peerUserId = call.participants
        .map((CallParticipant p) => p.userId)
        .firstWhere((String id) => id != _selfUserId, orElse: () => '');

    state = CallSession(
      callId: call.id,
      phase: CallPhase.dialling,
      isVideo: video,
      isOutgoing: true,
      peerName: peerName,
      cameraOn: video,
      startedAt: call.startedAt,
    );

    await _prepareMedia(video: video);
  }

  /// Presents an incoming call the socket announced.
  Future<void> receive({
    required String callId,
    required String fromUserId,
    required String peerName,
    required bool video,
  }) async {
    _peerUserId = fromUserId;
    state = CallSession(
      callId: callId,
      phase: CallPhase.ringing,
      isVideo: video,
      isOutgoing: false,
      peerName: peerName,
      cameraOn: video,
      startedAt: DateTime.now(),
    );
  }

  /// Answers a ringing call.
  Future<void> answer() async {
    final CallSession? session = state;
    if (session == null || session.phase != CallPhase.ringing) {
      return;
    }

    state = session.copyWith(phase: CallPhase.connecting);
    await _calls.accept(session.callId);
    await _prepareMedia(video: session.isVideo);
    // The callee does not offer: accepting tells the caller to, and the offer
    // arrives as a signal.
  }

  Future<void> decline() async {
    final CallSession? session = state;
    if (session == null) {
      return;
    }
    await _calls.reject(session.callId);
    await _teardown(reason: 'declined');
  }

  Future<void> hangUp() async {
    final CallSession? session = state;
    if (session == null) {
      return;
    }
    await _calls.end(session.callId);
    await _teardown(reason: 'hangup');
  }

  Future<void> toggleMute() async {
    final CallSession? session = state;
    if (session == null) {
      return;
    }
    final bool muted = !session.muted;
    for (final MediaStreamTrack track
        in _localStream?.getAudioTracks() ?? <MediaStreamTrack>[]) {
      track.enabled = !muted;
    }
    state = session.copyWith(muted: muted);
    await _calls.setMedia(session.callId, muted: muted);
  }

  Future<void> toggleCamera() async {
    final CallSession? session = state;
    if (session == null) {
      return;
    }
    final bool on = !session.cameraOn;
    for (final MediaStreamTrack track
        in _localStream?.getVideoTracks() ?? <MediaStreamTrack>[]) {
      track.enabled = on;
    }
    state = session.copyWith(cameraOn: on);
    await _calls.setMedia(session.callId, video: on);
  }

  Future<void> switchCamera() async {
    final List<MediaStreamTrack> video =
        _localStream?.getVideoTracks() ?? <MediaStreamTrack>[];
    if (video.isNotEmpty) {
      await Helper.switchCamera(video.first);
    }
  }

  Future<void> toggleSpeaker() async {
    final CallSession? session = state;
    if (session == null) {
      return;
    }
    final bool on = !session.speakerOn;
    await Helper.setSpeakerphoneOn(on);
    state = session.copyWith(speakerOn: on);
  }

  /// Opens the microphone and camera and builds the peer connection.
  Future<void> _prepareMedia({required bool video}) async {
    // TURN credentials are short-lived HMACs, so they are fetched per call
    // rather than cached.
    final List<IceServer> servers = await _calls.iceServers();

    _peer = await createPeerConnection(<String, dynamic>{
      'iceServers': <Map<String, dynamic>>[
        for (final IceServer server in servers) server.toConfiguration(),
      ],
      // Unified plan is the only spec-compliant semantics; plan-b is removed
      // from modern browsers.
      'sdpSemantics': 'unified-plan',
    });

    _localStream = await navigator.mediaDevices.getUserMedia(<String, dynamic>{
      'audio': true,
      'video': video
          ? <String, dynamic>{
              'facingMode': 'user',
              'width': <String, dynamic>{'ideal': 1280},
              'height': <String, dynamic>{'ideal': 720},
            }
          : false,
    });
    _localStreamController.add(_localStream);

    for (final MediaStreamTrack track in _localStream!.getTracks()) {
      await _peer!.addTrack(track, _localStream!);
    }

    _peer!.onIceCandidate = (RTCIceCandidate candidate) {
      final CallSession? session = state;
      if (session == null ||
          _peerUserId == null ||
          candidate.candidate == null) {
        return;
      }
      // Fire and forget: a lost candidate costs one connectivity path, and
      // failing the call over it would be worse than trying the rest.
      unawaited(
        _calls
            .signal(
              callId: session.callId,
              to: _peerUserId!,
              type: 'ice',
              payload: candidate.toMap() as Object,
            )
            .catchError((Object _) {}),
      );
    };

    _peer!.onTrack = (RTCTrackEvent event) {
      if (event.streams.isNotEmpty) {
        _remoteStream = event.streams.first;
        _remoteStreamController.add(_remoteStream);
      }
    };

    _peer!.onConnectionState = (RTCPeerConnectionState connectionState) {
      final CallSession? session = state;
      if (session == null) {
        return;
      }
      switch (connectionState) {
        case RTCPeerConnectionState.RTCPeerConnectionStateConnected:
          state = session.copyWith(
            phase: CallPhase.connected,
            connectedAt: session.connectedAt ?? DateTime.now(),
          );
        case RTCPeerConnectionState.RTCPeerConnectionStateDisconnected:
          state = session.copyWith(phase: CallPhase.reconnecting);
        case RTCPeerConnectionState.RTCPeerConnectionStateFailed:
        case RTCPeerConnectionState.RTCPeerConnectionStateClosed:
          unawaited(_teardown(reason: 'connection_lost'));
        case RTCPeerConnectionState.RTCPeerConnectionStateNew:
        case RTCPeerConnectionState.RTCPeerConnectionStateConnecting:
          break;
      }
    };
  }

  /// Sends the offer. The caller does this once the callee has accepted.
  Future<void> _offer() async {
    final CallSession? session = state;
    if (_peer == null || session == null || _peerUserId == null) {
      return;
    }

    final RTCSessionDescription offer = await _peer!.createOffer();
    await _peer!.setLocalDescription(offer);
    await _calls.signal(
      callId: session.callId,
      to: _peerUserId!,
      type: 'offer',
      payload: offer.toMap() as Object,
    );
  }

  Future<void> _onSocketEvent(SocketFrame frame) async {
    final Map<String, dynamic> payload = frame.payload;

    switch (frame.event) {
      case 'call.accepted':
        // The callee is ready; the caller now offers.
        if (state?.isOutgoing ?? false) {
          state = state!.copyWith(phase: CallPhase.connecting);
          await _offer();
        }

      case 'call.signal':
        await _onSignal(payload);

      case 'call.rejected':
        await _teardown(reason: 'rejected');

      case 'call.ended':
        await _teardown(reason: payload['reason'] as String? ?? 'hangup');
    }
  }

  Future<void> _onSignal(Map<String, dynamic> payload) async {
    if (_peer == null || payload['call_id'] != state?.callId) {
      return;
    }
    final Map<String, dynamic> body =
        payload['payload'] as Map<String, dynamic>? ??
            const <String, dynamic>{};

    switch (payload['type'] as String?) {
      case 'offer':
        await _peer!.setRemoteDescription(
          RTCSessionDescription(
            body['sdp'] as String?,
            body['type'] as String?,
          ),
        );
        _remoteDescriptionSet = true;
        await _drainCandidates();

        final RTCSessionDescription answer = await _peer!.createAnswer();
        await _peer!.setLocalDescription(answer);
        await _calls.signal(
          callId: state!.callId,
          to: _peerUserId!,
          type: 'answer',
          payload: answer.toMap() as Object,
        );

      case 'answer':
        await _peer!.setRemoteDescription(
          RTCSessionDescription(
            body['sdp'] as String?,
            body['type'] as String?,
          ),
        );
        _remoteDescriptionSet = true;
        await _drainCandidates();

      case 'ice':
        final RTCIceCandidate candidate = RTCIceCandidate(
          body['candidate'] as String?,
          body['sdpMid'] as String?,
          (body['sdpMLineIndex'] as num?)?.toInt(),
        );
        if (_remoteDescriptionSet) {
          await _peer!.addCandidate(candidate);
        } else {
          _pendingCandidates.add(candidate);
        }
    }
  }

  Future<void> _drainCandidates() async {
    for (final RTCIceCandidate candidate in _pendingCandidates) {
      await _peer!.addCandidate(candidate);
    }
    _pendingCandidates.clear();
  }

  Future<void> _teardown({required String reason}) async {
    final CallSession? session = state;
    if (session != null) {
      state = session.copyWith(phase: CallPhase.ended, endReason: reason);
    }

    for (final MediaStreamTrack track
        in _localStream?.getTracks() ?? <MediaStreamTrack>[]) {
      await track.stop();
    }
    await _localStream?.dispose();
    await _peer?.close();

    _localStream = null;
    _remoteStream = null;
    _peer = null;
    _remoteDescriptionSet = false;
    _pendingCandidates.clear();

    _localStreamController.add(null);
    _remoteStreamController.add(null);
  }

  /// Clears the session so the screen can close and the next call starts fresh.
  void clear() => state = null;

  @override
  void dispose() {
    unawaited(_events.cancel());
    unawaited(_teardown(reason: 'disposed'));
    unawaited(_localStreamController.close());
    unawaited(_remoteStreamController.close());
    super.dispose();
  }
}

final StateNotifierProvider<CallController, CallSession?>
    callControllerProvider =
    StateNotifierProvider<CallController, CallSession?>(
  (Ref ref) => CallController(
    calls: ref.watch(callsRepositoryProvider),
    socket: ref.watch(socketClientProvider),
    selfUserId: ref.watch(sessionControllerProvider).userId ?? '',
  ),
);
