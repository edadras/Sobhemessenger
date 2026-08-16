import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A 24-hour story (§17).
class Story {
  const Story({
    required this.id,
    required this.authorId,
    required this.authorName,
    required this.type,
    required this.createdAt,
    required this.expiresAt,
    required this.viewCount,
    required this.seenByMe,
    this.caption = '',
    this.background = '',
    this.mediaId,
    this.myReaction,
  });

  factory Story.fromJson(Map<String, dynamic> json) => Story(
        id: json['id'] as String,
        authorId: json['author_id'] as String,
        authorName: json['author_name'] as String? ?? '',
        type: json['type'] as String? ?? 'text',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        expiresAt: DateTime.parse(json['expires_at'] as String).toLocal(),
        viewCount: (json['view_count'] as num?)?.toInt() ?? 0,
        seenByMe: json['seen_by_me'] as bool? ?? false,
        caption: json['caption'] as String? ?? '',
        background: json['background'] as String? ?? '',
        mediaId: json['media_id'] as String?,
        myReaction: json['my_reaction'] as String?,
      );

  final String id;
  final String authorId;
  final String authorName;
  final String type;
  final DateTime createdAt;
  final DateTime expiresAt;
  final int viewCount;
  final bool seenByMe;
  final String caption;
  final String background;
  final String? mediaId;
  final String? myReaction;

  Duration get remaining => expiresAt.difference(DateTime.now());
}

/// Someone who watched a story.
class StoryViewer {
  const StoryViewer({
    required this.userId,
    required this.displayName,
    this.reaction,
  });

  factory StoryViewer.fromJson(Map<String, dynamic> json) => StoryViewer(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        reaction: json['reaction'] as String?,
      );

  final String userId;
  final String displayName;
  final String? reaction;
}

class StoriesRepository {
  StoriesRepository(this._api);

  final ApiClient _api;

  Future<List<Story>> feed() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/stories');
    return <Story>[
      for (final dynamic entry
          in data['stories'] as List<dynamic>? ?? const <dynamic>[])
        Story.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<Story> post({
    required String type,
    String caption = '',
    String background = '',
    String? mediaId,
    String privacy = 'contacts',
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/stories',
      body: <String, dynamic>{
        'type': type,
        'caption': caption,
        'background': background,
        'privacy': privacy,
        if (mediaId != null) 'media_id': mediaId,
      },
    );
    return Story.fromJson(data);
  }

  /// Records a view. Repeated calls are harmless — the server counts a viewer
  /// once, so re-opening a story does not inflate its count.
  Future<void> markViewed(String storyId, {String? reaction}) =>
      _api.post<Map<String, dynamic>>(
        '/stories/$storyId/view',
        body: <String, dynamic>{if (reaction != null) 'reaction': reaction},
      );

  Future<List<StoryViewer>> viewers(String storyId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/stories/$storyId/viewers');
    return <StoryViewer>[
      for (final dynamic entry
          in data['viewers'] as List<dynamic>? ?? const <dynamic>[])
        StoryViewer.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<void> delete(String storyId) =>
      _api.delete<Map<String, dynamic>>('/stories/$storyId');
}

final Provider<StoriesRepository> storiesRepositoryProvider =
    Provider<StoriesRepository>(
  (Ref ref) => StoriesRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<Story>> storyFeedProvider =
    FutureProvider<List<Story>>(
  (Ref ref) => ref.watch(storiesRepositoryProvider).feed(),
);

final FutureProviderFamily<List<StoryViewer>, String> storyViewersProvider =
    FutureProvider.family<List<StoryViewer>, String>(
  (Ref ref, String storyId) =>
      ref.watch(storiesRepositoryProvider).viewers(storyId),
);
