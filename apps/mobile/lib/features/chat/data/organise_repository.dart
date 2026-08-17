import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One post the caller has queued in a chat (§12).
///
/// A queued post is not a message: it has no sequence number and nobody else
/// can see it until it publishes, which is why it carries its own id rather
/// than a message id.
class ScheduledMessage {
  const ScheduledMessage({
    required this.id,
    required this.chatId,
    required this.content,
    required this.scheduledAt,
    this.type = 'text',
    this.attempts = 0,
    this.lastError = '',
  });

  factory ScheduledMessage.fromJson(Map<String, dynamic> json) =>
      ScheduledMessage(
        id: json['id'] as String,
        chatId: json['chat_id'] as String,
        content: json['content'] as String? ?? '',
        scheduledAt: DateTime.parse(json['scheduled_at'] as String).toLocal(),
        type: json['type'] as String? ?? 'text',
        attempts: (json['attempts'] as num?)?.toInt() ?? 0,
        lastError: json['last_error'] as String? ?? '',
      );

  final String id;
  final String chatId;
  final String content;
  final DateTime scheduledAt;
  final String type;

  /// How often publication has been tried, and why it last failed. Both are
  /// shown so a post that never went out can be explained rather than just
  /// disappearing from the queue.
  final int attempts;
  final String lastError;

  bool get hasFailed => lastError.isNotEmpty;
}

/// Forwarding, pinning, scheduling and filing a conversation (§12).
///
/// These are separate from [ChatRepository] because none of them go through
/// the offline outbox: each is an action on the server's copy of a
/// conversation, so queueing one locally would only hide that it had not
/// happened.
class OrganiseRepository {
  OrganiseRepository(this._api);

  final ApiClient _api;

  /// Copies messages into another chat.
  ///
  /// The originals keep their author: forwarding a forward still credits
  /// whoever wrote the message, not whoever passed it on. [dropAuthor] is
  /// "forward without quoting" — the text goes, the name does not.
  Future<int> forward({
    required String fromChatId,
    required String toChatId,
    required List<String> messageIds,
    bool dropAuthor = false,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$fromChatId/forward',
      body: <String, dynamic>{
        'to_chat_id': toChatId,
        'message_ids': messageIds,
        'drop_author': dropAuthor,
      },
    );
    return (data['messages'] as List<dynamic>? ?? const <dynamic>[]).length;
  }

  Future<void> setPinned(String messageId, bool pinned) => _api.put<dynamic>(
        '/messages/$messageId/pin',
        body: <String, dynamic>{'pinned': pinned},
      );

  Future<List<String>> pinnedMessageIds(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/pinned');
    return <String>[
      for (final dynamic entry
          in data['messages'] as List<dynamic>? ?? const <dynamic>[])
        (entry as Map<String, dynamic>)['id'] as String,
    ];
  }

  /// Files a conversation for the caller alone.
  ///
  /// A null field is left alone by the server, so unarchiving is not also an
  /// instruction to unpin.
  Future<void> setFlags(String chatId, {bool? pinned, bool? archived}) =>
      _api.put<dynamic>(
        '/chats/$chatId/flags',
        body: <String, dynamic>{
          if (pinned != null) 'pinned': pinned,
          if (archived != null) 'archived': archived,
        },
      );

  /// Mutes until a time; null unmutes.
  Future<void> setMuted(String chatId, DateTime? until) => _api.put<dynamic>(
        '/chats/$chatId/mute',
        body: <String, dynamic>{
          'muted_until': until?.toUtc().toIso8601String(),
        },
      );

  Future<List<ScheduledMessage>> scheduled(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/scheduled');
    return <ScheduledMessage>[
      for (final dynamic entry
          in data['messages'] as List<dynamic>? ?? const <dynamic>[])
        ScheduledMessage.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Queues a post for later.
  ///
  /// [clientMessageId] is required and is carried through to the published
  /// message, which is what makes both this call and the publication itself
  /// safe to retry: re-sending edits the queued post rather than adding a
  /// second one.
  Future<ScheduledMessage> schedule({
    required String chatId,
    required String clientMessageId,
    required String content,
    required DateTime at,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$chatId/scheduled',
      body: <String, dynamic>{
        'client_message_id': clientMessageId,
        'type': 'text',
        'content': content,
        'scheduled_at': at.toUtc().toIso8601String(),
      },
    );
    return ScheduledMessage.fromJson(data);
  }

  Future<void> cancelScheduled(String chatId, String scheduledId) =>
      _api.delete<dynamic>('/chats/$chatId/scheduled/$scheduledId');
}

final Provider<OrganiseRepository> organiseRepositoryProvider =
    Provider<OrganiseRepository>(
  (Ref ref) => OrganiseRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<List<ScheduledMessage>, String>
    scheduledMessagesProvider =
    FutureProvider.family<List<ScheduledMessage>, String>(
  (Ref ref, String chatId) =>
      ref.watch(organiseRepositoryProvider).scheduled(chatId),
);

final FutureProviderFamily<List<String>, String> pinnedMessagesProvider =
    FutureProvider.family<List<String>, String>(
  (Ref ref, String chatId) =>
      ref.watch(organiseRepositoryProvider).pinnedMessageIds(chatId),
);
