import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A call and its participants (§18).
class Call {
  const Call({
    required this.id,
    required this.initiatorId,
    required this.type,
    required this.scope,
    required this.state,
    required this.startedAt,
    this.chatId,
    this.endReason = '',
    this.durationSeconds,
    this.participants = const <CallParticipant>[],
  });

  factory Call.fromJson(Map<String, dynamic> json) => Call(
        id: json['id'] as String,
        initiatorId: json['initiator_id'] as String,
        type: json['type'] as String? ?? 'audio',
        scope: json['scope'] as String? ?? 'private',
        state: json['state'] as String? ?? 'ringing',
        startedAt: DateTime.parse(json['started_at'] as String).toLocal(),
        chatId: json['chat_id'] as String?,
        endReason: json['end_reason'] as String? ?? '',
        durationSeconds: (json['duration_seconds'] as num?)?.toInt(),
        participants: <CallParticipant>[
          for (final dynamic entry
              in json['participants'] as List<dynamic>? ?? const <dynamic>[])
            CallParticipant.fromJson(entry as Map<String, dynamic>),
        ],
      );

  final String id;
  final String initiatorId;
  final String type;
  final String scope;
  final String state;
  final DateTime startedAt;
  final String? chatId;
  final String endReason;
  final int? durationSeconds;
  final List<CallParticipant> participants;

  bool get isVideo => type == 'video';
  bool get isOver => state == 'ended';

  /// A call that ended without ever connecting is a missed call — which is
  /// what the history list needs to distinguish, not the raw state.
  bool get wasMissed => isOver && durationSeconds == null;
}

class CallParticipant {
  const CallParticipant({
    required this.userId,
    required this.state,
    required this.displayName,
    required this.isMuted,
    required this.videoEnabled,
  });

  factory CallParticipant.fromJson(Map<String, dynamic> json) =>
      CallParticipant(
        userId: json['user_id'] as String,
        state: json['state'] as String? ?? 'invited',
        displayName: json['display_name'] as String? ?? '',
        isMuted: json['is_muted'] as bool? ?? false,
        videoEnabled: json['video_enabled'] as bool? ?? false,
      );

  final String userId;
  final String state;
  final String displayName;
  final bool isMuted;
  final bool videoEnabled;
}

/// A TURN or STUN server, with credentials that expire.
class IceServer {
  const IceServer({
    required this.urls,
    this.username = '',
    this.credential = '',
  });

  factory IceServer.fromJson(Map<String, dynamic> json) => IceServer(
        urls: <String>[
          for (final dynamic url
              in json['urls'] as List<dynamic>? ?? const <dynamic>[])
            url as String,
        ],
        username: json['username'] as String? ?? '',
        credential: json['credential'] as String? ?? '',
      );

  final List<String> urls;
  final String username;
  final String credential;

  Map<String, dynamic> toConfiguration() => <String, dynamic>{
        'urls': urls,
        if (username.isNotEmpty) 'username': username,
        if (credential.isNotEmpty) 'credential': credential,
      };
}

/// Call signalling (§18).
///
/// The server relays SDP and ICE without parsing them, so this repository is
/// deliberately thin: it starts, answers and ends calls, and passes signalling
/// payloads through untouched.
class CallsRepository {
  CallsRepository(this._api);

  final ApiClient _api;

  Future<List<Call>> history() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/calls');
    return <Call>[
      for (final dynamic entry
          in data['calls'] as List<dynamic>? ?? const <dynamic>[])
        Call.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<Call> start({required String chatId, required bool video}) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/calls',
      body: <String, dynamic>{
        'chat_id': chatId,
        'type': video ? 'video' : 'audio',
      },
    );
    return Call.fromJson(data);
  }

  Future<Call> get(String callId) async =>
      Call.fromJson(await _api.get<Map<String, dynamic>>('/calls/$callId'));

  Future<Call> accept(String callId) async => Call.fromJson(
        await _api.post<Map<String, dynamic>>('/calls/$callId/accept'),
      );

  Future<void> reject(String callId, {String reason = 'declined'}) =>
      _api.post<Map<String, dynamic>>(
        '/calls/$callId/reject',
        body: <String, dynamic>{'reason': reason},
      );

  Future<void> end(String callId, {String reason = 'hangup'}) =>
      _api.post<Map<String, dynamic>>(
        '/calls/$callId/end',
        body: <String, dynamic>{'reason': reason},
      );

  /// Relays one SDP or ICE payload to another participant. The body is opaque
  /// to both this method and the server.
  Future<void> signal({
    required String callId,
    required String to,
    required String type,
    required Object payload,
  }) =>
      _api.post<Map<String, dynamic>>(
        '/calls/$callId/signal',
        body: <String, dynamic>{'to': to, 'type': type, 'payload': payload},
      );

  Future<void> setMedia(
    String callId, {
    bool? muted,
    bool? video,
    bool? screenSharing,
  }) =>
      _api.put<Map<String, dynamic>>(
        '/calls/$callId/media',
        body: <String, dynamic>{
          if (muted != null) 'is_muted': muted,
          if (video != null) 'video_enabled': video,
          if (screenSharing != null) 'screen_sharing': screenSharing,
        },
      );

  /// Fetches ICE servers. The TURN credentials are short-lived HMACs, so they
  /// are requested per call rather than cached.
  Future<List<IceServer>> iceServers() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/calls/ice-servers');
    return <IceServer>[
      for (final dynamic entry
          in data['ice_servers'] as List<dynamic>? ?? const <dynamic>[])
        IceServer.fromJson(entry as Map<String, dynamic>),
    ];
  }
}

final Provider<CallsRepository> callsRepositoryProvider =
    Provider<CallsRepository>(
  (Ref ref) => CallsRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<Call>> callHistoryProvider =
    FutureProvider<List<Call>>(
  (Ref ref) => ref.watch(callsRepositoryProvider).history(),
);
