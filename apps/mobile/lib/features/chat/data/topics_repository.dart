import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One thread inside a forum (§14).
///
/// A topic is not a chat: membership, permissions and moderation all stay at
/// the chat level. What it adds is a partition of the messages and a read
/// cursor per partition, because a forum member follows some topics and
/// ignores others.
class ForumTopic {
  const ForumTopic({
    required this.id,
    required this.chatId,
    required this.title,
    this.iconEmoji = '',
    this.iconColor = 0,
    this.createdBy,
    this.isGeneral = false,
    this.isClosed = false,
    this.isHidden = false,
    this.isPinned = false,
    this.messageCount = 0,
    this.unreadCount = 0,
    required this.lastMessageAt,
  });

  factory ForumTopic.fromJson(Map<String, dynamic> json) => ForumTopic(
        id: json['id'] as String,
        chatId: json['chat_id'] as String,
        title: json['title'] as String? ?? '',
        iconEmoji: json['icon_emoji'] as String? ?? '',
        iconColor: (json['icon_color'] as num?)?.toInt() ?? 0,
        createdBy: json['created_by'] as String?,
        isGeneral: json['is_general'] as bool? ?? false,
        isClosed: json['is_closed'] as bool? ?? false,
        isHidden: json['is_hidden'] as bool? ?? false,
        isPinned: json['is_pinned'] as bool? ?? false,
        messageCount: (json['message_count'] as num?)?.toInt() ?? 0,
        unreadCount: (json['unread_count'] as num?)?.toInt() ?? 0,
        lastMessageAt:
            DateTime.parse(json['last_message_at'] as String).toLocal(),
      );

  final String id;
  final String chatId;
  final String title;
  final String iconEmoji;
  final int iconColor;
  final String? createdBy;

  /// The topic a converted group's history is filed under. Exactly one per
  /// forum, and it cannot be deleted.
  final bool isGeneral;
  final bool isClosed;
  final bool isHidden;
  final bool isPinned;
  final int messageCount;

  /// The caller's own, from their per-topic cursor.
  final int unreadCount;
  final DateTime lastMessageAt;
}

/// Forum topics (§14).
class TopicsRepository {
  TopicsRepository(this._api);

  final ApiClient _api;
  static const Uuid _uuid = Uuid();

  Future<List<ForumTopic>> list(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/topics');
    return <ForumTopic>[
      for (final dynamic entry
          in data['topics'] as List<dynamic>? ?? const <dynamic>[])
        ForumTopic.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Turns a group into a forum, returning the General topic its existing
  /// history has been filed under.
  Future<ForumTopic> enableForum(String chatId) async {
    final Map<String, dynamic> data =
        await _api.post<Map<String, dynamic>>('/chats/$chatId/forum');
    return ForumTopic.fromJson(data['general_topic'] as Map<String, dynamic>);
  }

  Future<void> disableForum(String chatId) =>
      _api.delete<dynamic>('/chats/$chatId/forum');

  Future<ForumTopic> create({
    required String chatId,
    required String title,
    String iconEmoji = '',
    int iconColor = 0,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$chatId/topics',
      body: <String, dynamic>{
        'title': title,
        'icon_emoji': iconEmoji,
        'icon_color': iconColor,
      },
    );
    return ForumTopic.fromJson(data['topic'] as Map<String, dynamic>);
  }

  /// Only the fields supplied are changed. A whole-object update would let a
  /// rename silently reopen a topic somebody had closed.
  Future<ForumTopic> update(
    String chatId,
    String topicId, {
    String? title,
    String? iconEmoji,
    int? iconColor,
    bool? isClosed,
    bool? isHidden,
    bool? isPinned,
  }) async {
    final Map<String, dynamic> data = await _api.patch<Map<String, dynamic>>(
      '/chats/$chatId/topics/$topicId',
      body: <String, dynamic>{
        if (title != null) 'title': title,
        if (iconEmoji != null) 'icon_emoji': iconEmoji,
        if (iconColor != null) 'icon_color': iconColor,
        if (isClosed != null) 'is_closed': isClosed,
        if (isHidden != null) 'is_hidden': isHidden,
        if (isPinned != null) 'is_pinned': isPinned,
      },
    );
    return ForumTopic.fromJson(data['topic'] as Map<String, dynamic>);
  }

  Future<void> delete(String chatId, String topicId) =>
      _api.delete<dynamic>('/chats/$chatId/topics/$topicId');

  /// One topic's messages, newest last.
  ///
  /// Fetched rather than read from the local store, which has no notion of a
  /// topic: the offline cache is per chat, and a forum's history is filed
  /// under General plus whatever topics exist. Caching it per topic as well
  /// would be a second copy of the same rows to keep in step, for a screen
  /// people open deliberately rather than live in.
  Future<List<TopicMessage>> messages(
    String chatId,
    String topicId, {
    int? beforeSeq,
    int limit = 50,
  }) async {
    final String query = <String>[
      'limit=$limit',
      if (beforeSeq != null) 'before_seq=$beforeSeq',
    ].join('&');
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats/$chatId/topics/$topicId/messages?$query',
    );
    return <TopicMessage>[
      for (final dynamic entry
          in data['messages'] as List<dynamic>? ?? const <dynamic>[])
        TopicMessage.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Posts into a topic. The topic id is what files it there; without one the
  /// message lands in General, which is what the server does for a forum.
  Future<void> post({
    required String chatId,
    required String topicId,
    required String content,
  }) =>
      _api.post<dynamic>(
        '/chats/$chatId/messages',
        body: <String, dynamic>{
          'client_message_id': _uuid.v4(),
          'type': 'text',
          'content': content,
          'topic_id': topicId,
        },
      );

  Future<void> markRead(String chatId, String topicId, int seq) =>
      _api.post<dynamic>(
        '/chats/$chatId/topics/$topicId/read',
        body: <String, dynamic>{'seq': seq},
      );
}

final Provider<TopicsRepository> topicsRepositoryProvider =
    Provider<TopicsRepository>(
  (Ref ref) => TopicsRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<List<ForumTopic>, String> forumTopicsProvider =
    FutureProvider.family<List<ForumTopic>, String>(
  (Ref ref, String chatId) => ref.watch(topicsRepositoryProvider).list(chatId),
);

/// One message inside a topic.
///
/// Deliberately thinner than the chat screen's row: this is a fetched view of
/// a filed conversation, not the offline-first store, so it carries what the
/// list draws and nothing more.
class TopicMessage {
  const TopicMessage({
    required this.id,
    required this.seq,
    required this.content,
    required this.createdAt,
    this.senderId,
    this.senderName = '',
    this.isDeleted = false,
  });

  factory TopicMessage.fromJson(Map<String, dynamic> json) => TopicMessage(
        id: json['id'] as String,
        seq: (json['seq'] as num?)?.toInt() ?? 0,
        content: json['content'] as String? ?? '',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        senderId: json['sender_id'] as String?,
        senderName: json['sender_name'] as String? ?? '',
        isDeleted: json['deleted_at'] != null,
      );

  final String id;
  final int seq;
  final String content;
  final DateTime createdAt;
  final String? senderId;
  final String senderName;
  final bool isDeleted;
}

final FutureProviderFamily<List<TopicMessage>, (String, String)>
    topicMessagesProvider =
    FutureProvider.family<List<TopicMessage>, (String, String)>(
  (Ref ref, (String, String) key) =>
      ref.watch(topicsRepositoryProvider).messages(key.$1, key.$2),
);
