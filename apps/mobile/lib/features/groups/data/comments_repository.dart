import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// Where the comments on one channel post live (§15).
///
/// Comments are ordinary replies to the post's mirrored copy in the linked
/// discussion group, so everything that applies to a message applies to a
/// comment — which is why this carries chat and message ids rather than a
/// comment id of its own.
class CommentThread {
  const CommentThread({
    required this.postMessageId,
    required this.discussionChatId,
    required this.rootMessageId,
    this.commentCount = 0,
  });

  factory CommentThread.fromJson(Map<String, dynamic> json) => CommentThread(
        postMessageId: json['post_message_id'] as String,
        discussionChatId: json['discussion_chat_id'] as String,
        rootMessageId: json['root_message_id'] as String,
        commentCount: (json['comment_count'] as num?)?.toInt() ?? 0,
      );

  final String postMessageId;
  final String discussionChatId;
  final String rootMessageId;
  final int commentCount;
}

/// One comment.
///
/// A comment is an ordinary message in the discussion group, but it is not
/// stored in the local database: comments are read on demand under a post
/// rather than synced like a conversation, so this is a plain view model
/// rather than a row.
class Comment {
  const Comment({
    required this.id,
    required this.seq,
    required this.content,
    required this.createdAt,
    this.senderId,
    this.deleted = false,
  });

  factory Comment.fromJson(Map<String, dynamic> json) => Comment(
        id: json['id'] as String,
        seq: (json['seq'] as num?)?.toInt() ?? 0,
        content: json['content'] as String? ?? '',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        senderId: json['sender_id'] as String?,
        deleted: json['deleted_at'] != null,
      );

  final String id;
  final int seq;
  final String content;
  final DateTime createdAt;
  final String? senderId;
  final bool deleted;
}

/// A thread and the page of comments read with it.
class CommentPage {
  const CommentPage({required this.thread, required this.comments});

  final CommentThread thread;
  final List<Comment> comments;
}

/// Channel comments and the linked discussion group (§15).
class CommentsRepository {
  CommentsRepository(this._api);

  final ApiClient _api;

  /// Attaches a group to a channel. The caller has to be entitled to edit
  /// both: linking hands the channel's posts to the group and the group's
  /// members the channel's audience.
  Future<void> linkDiscussion(String channelId, String groupChatId) =>
      _api.post<dynamic>(
        '/chats/$channelId/discussion',
        body: <String, dynamic>{'group_chat_id': groupChatId},
      );

  Future<void> unlinkDiscussion(String channelId) =>
      _api.delete<dynamic>('/chats/$channelId/discussion');

  Future<CommentPage> comments(
    String channelId,
    String postId, {
    int? beforeSeq,
    int limit = 50,
  }) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats/$channelId/messages/$postId/comments',
      query: <String, String>{
        'limit': '$limit',
        if (beforeSeq != null) 'before_seq': '$beforeSeq',
      },
    );
    return CommentPage(
      thread: CommentThread.fromJson(data['thread'] as Map<String, dynamic>),
      comments: <Comment>[
        for (final dynamic entry
            in data['comments'] as List<dynamic>? ?? const <dynamic>[])
          Comment.fromJson(entry as Map<String, dynamic>),
      ],
    );
  }

  /// Posts a comment. A reader who is not yet in the discussion group is
  /// joined by commenting — the audience for a channel is its subscribers, and
  /// making them find a second chat first would leave the comment box on a
  /// post they cannot use.
  Future<Comment> comment({
    required String channelId,
    required String postId,
    required String clientMessageId,
    required String content,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$channelId/messages/$postId/comments',
      body: <String, dynamic>{
        'client_message_id': clientMessageId,
        'type': 'text',
        'content': content,
      },
    );
    return Comment.fromJson(data['message'] as Map<String, dynamic>);
  }
}

final Provider<CommentsRepository> commentsRepositoryProvider =
    Provider<CommentsRepository>(
  (Ref ref) => CommentsRepository(ref.watch(apiClientProvider)),
);
