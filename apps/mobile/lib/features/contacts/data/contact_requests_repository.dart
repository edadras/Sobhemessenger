import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One contact request (§54).
///
/// Adding a contact is one-sided and silent: it writes a row in your own
/// address book and tells the other person nothing. A request asks them to
/// know you back, and acceptance writes both entries at once.
class ContactRequest {
  const ContactRequest({
    required this.id,
    required this.requesterId,
    required this.targetId,
    required this.status,
    required this.createdAt,
    this.message = '',
    this.displayName = '',
    this.username,
    this.avatarMediaId,
    this.resolvedAt,
  });

  factory ContactRequest.fromJson(Map<String, dynamic> json) => ContactRequest(
        id: json['id'] as String,
        requesterId: json['requester_id'] as String,
        targetId: json['target_id'] as String,
        status: json['status'] as String? ?? 'pending',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        message: json['message'] as String? ?? '',
        displayName: json['display_name'] as String? ?? '',
        username: json['username'] as String?,
        avatarMediaId: json['avatar_media_id'] as String?,
        resolvedAt: json['resolved_at'] == null
            ? null
            : DateTime.parse(json['resolved_at'] as String).toLocal(),
      );

  final String id;
  final String requesterId;
  final String targetId;

  /// `pending`, `accepted`, `rejected` or `cancelled`.
  final String status;
  final DateTime createdAt;
  final String message;

  /// The other party, so a list renders without a lookup per row.
  final String displayName;
  final String? username;
  final String? avatarMediaId;
  final DateTime? resolvedAt;

  bool get isPending => status == 'pending';
}

/// Contact requests (§54).
class ContactRequestsRepository {
  ContactRequestsRepository(this._api);

  final ApiClient _api;

  /// [direction] is `incoming` or `outgoing`. Incoming lists only what is
  /// still pending; outgoing keeps its history, so the sender can see that
  /// what they asked for was declined rather than wondering whether it ever
  /// arrived.
  Future<List<ContactRequest>> list({String direction = 'incoming'}) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/contacts/requests',
      query: <String, String>{'direction': direction},
    );
    return <ContactRequest>[
      for (final dynamic entry
          in data['requests'] as List<dynamic>? ?? const <dynamic>[])
        ContactRequest.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Sending the same request twice returns the one already open rather than
  /// creating a second.
  Future<ContactRequest> send(String userId, {String message = ''}) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/contacts/requests',
      body: <String, dynamic>{'user_id': userId, 'message': message},
    );
    return ContactRequest.fromJson(data['request'] as Map<String, dynamic>);
  }

  Future<void> accept(String requestId) =>
      _api.post<dynamic>('/contacts/requests/$requestId/accept');

  Future<void> reject(String requestId) =>
      _api.post<dynamic>('/contacts/requests/$requestId/reject');

  /// Withdraws a request the caller sent.
  Future<void> cancel(String requestId) =>
      _api.delete<dynamic>('/contacts/requests/$requestId');
}

final Provider<ContactRequestsRepository> contactRequestsRepositoryProvider =
    Provider<ContactRequestsRepository>(
  (Ref ref) => ContactRequestsRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<List<ContactRequest>, String>
    contactRequestsProvider =
    FutureProvider.family<List<ContactRequest>, String>(
  (Ref ref, String direction) =>
      ref.watch(contactRequestsRepositoryProvider).list(direction: direction),
);
