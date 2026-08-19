import 'package:collection/collection.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// How the feed is ordered (§25).
enum FeedMode {
  latest('latest'),
  popular('popular'),
  following('following'),
  breaking('breaking');

  const FeedMode(this.wire);

  final String wire;
}

/// A news article.
class Article {
  const Article({
    required this.id,
    required this.slug,
    required this.title,
    required this.locale,
    required this.readingMinutes,
    required this.viewCount,
    this.subtitle = '',
    this.lead = '',
    this.body = '',
    this.bodyFormat = 'markdown',
    this.categoryName = '',
    this.authorName = '',
    this.isBreaking = false,
    this.isBookmarked = false,
    this.coverMediaId,
    this.publishedAt,
  });

  factory Article.fromJson(Map<String, dynamic> json) => Article(
        id: json['id'] as String,
        slug: json['slug'] as String? ?? '',
        title: json['title'] as String? ?? '',
        locale: json['locale'] as String? ?? 'fa',
        readingMinutes: (json['reading_minutes'] as num?)?.toInt() ?? 0,
        viewCount: (json['view_count'] as num?)?.toInt() ?? 0,
        subtitle: json['subtitle'] as String? ?? '',
        lead: json['lead'] as String? ?? '',
        body: json['body'] as String? ?? '',
        bodyFormat: json['body_format'] as String? ?? 'markdown',
        categoryName: json['category_name'] as String? ?? '',
        authorName: json['author_name'] as String? ?? '',
        isBreaking: json['is_breaking'] as bool? ?? false,
        isBookmarked: json['is_bookmarked'] as bool? ?? false,
        coverMediaId: json['cover_media_id'] as String?,
        publishedAt: json['published_at'] == null
            ? null
            : DateTime.parse(json['published_at'] as String).toLocal(),
      );

  final String id;
  final String slug;
  final String title;
  final String locale;
  final int readingMinutes;
  final int viewCount;
  final String subtitle;
  final String lead;
  final String body;
  final String bodyFormat;
  final String categoryName;
  final String authorName;
  final bool isBreaking;
  final bool isBookmarked;
  final String? coverMediaId;
  final DateTime? publishedAt;
}

/// A news category, already resolved to the reader's language.
class NewsCategory {
  const NewsCategory({
    required this.id,
    required this.slug,
    required this.name,
  });

  factory NewsCategory.fromJson(Map<String, dynamic> json) => NewsCategory(
        id: json['id'] as String,
        slug: json['slug'] as String? ?? '',
        name: json['name'] as String? ??
            (json['names'] as Map<String, dynamic>?)?.values.firstOrNull
                as String? ??
            '',
      );

  final String id;
  final String slug;
  final String name;
}

/// The news feed (§25, §26).
///
/// The feed itself is readable without an account; bookmarks and follows are
/// not, which is why they live under a different prefix.
class NewsRepository {
  NewsRepository(this._api);

  final ApiClient _api;

  Future<List<Article>> feed({
    required FeedMode mode,
    required String locale,
    String? categoryId,
  }) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/news',
      query: <String, dynamic>{
        'mode': mode.wire,
        'locale': locale,
        if (categoryId != null) 'category_id': categoryId,
      },
      // OptionalAuth: a signed-in reader also gets their bookmark state back.
      authenticated: true,
    );
    return _articles(data['articles']);
  }

  Future<List<NewsCategory>> categories(String locale) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/news/categories',
      query: <String, dynamic>{'locale': locale},
    );
    return <NewsCategory>[
      for (final dynamic entry
          in data['categories'] as List<dynamic>? ?? const <dynamic>[])
        NewsCategory.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<Article> article(String slug, String locale) async => Article.fromJson(
        await _api.get<Map<String, dynamic>>(
          '/news/articles/$slug',
          query: <String, dynamic>{'locale': locale},
        ),
      );

  /// The one story currently marked as breaking, or null when there is none.
  ///
  /// Different from the `breaking` feed mode, which is a *list* of everything
  /// ever flagged. This is the single item a banner shows, so the feed can say
  /// "this is happening now" above the ordinary list rather than making the
  /// reader spot it among fifty others.
  Future<Article?> breaking(String locale) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/news/breaking?locale=$locale',
    );
    final Object? article = data['article'];
    return article is Map<String, dynamic> ? Article.fromJson(article) : null;
  }

  Future<List<Article>> bookmarks(String locale) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/news-reader/bookmarks',
      query: <String, dynamic>{'locale': locale},
    );
    return _articles(data['articles']);
  }

  Future<void> setBookmark(String articleId, bool bookmarked) => bookmarked
      ? _api.put<Map<String, dynamic>>('/news-reader/bookmarks/$articleId')
      : _api.delete<Map<String, dynamic>>('/news-reader/bookmarks/$articleId');

  Future<void> setFollow({
    String? categoryId,
    String? authorId,
    required bool following,
  }) =>
      _api.put<Map<String, dynamic>>(
        '/news-reader/follows',
        body: <String, dynamic>{
          if (categoryId != null) 'category_id': categoryId,
          if (authorId != null) 'author_id': authorId,
          'following': following,
        },
      );

  static List<Article> _articles(dynamic raw) => <Article>[
        for (final dynamic entry in raw as List<dynamic>? ?? const <dynamic>[])
          Article.fromJson(entry as Map<String, dynamic>),
      ];
}

/// What the feed screen is currently showing.
class FeedQuery {
  const FeedQuery({required this.mode, required this.locale, this.categoryId});

  final FeedMode mode;
  final String locale;
  final String? categoryId;

  @override
  bool operator ==(Object other) =>
      other is FeedQuery &&
      other.mode == mode &&
      other.locale == locale &&
      other.categoryId == categoryId;

  @override
  int get hashCode => Object.hash(mode, locale, categoryId);
}

final Provider<NewsRepository> newsRepositoryProvider =
    Provider<NewsRepository>(
  (Ref ref) => NewsRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<List<Article>, FeedQuery> newsFeedProvider =
    FutureProvider.family<List<Article>, FeedQuery>(
  (Ref ref, FeedQuery query) => ref.watch(newsRepositoryProvider).feed(
        mode: query.mode,
        locale: query.locale,
        categoryId: query.categoryId,
      ),
);

final FutureProviderFamily<List<NewsCategory>, String> newsCategoriesProvider =
    FutureProvider.family<List<NewsCategory>, String>(
  (Ref ref, String locale) =>
      ref.watch(newsRepositoryProvider).categories(locale),
);

final FutureProviderFamily<List<Article>, String> newsBookmarksProvider =
    FutureProvider.family<List<Article>, String>(
  (Ref ref, String locale) =>
      ref.watch(newsRepositoryProvider).bookmarks(locale),
);

final FutureProviderFamily<Article?, String> breakingArticleProvider =
    FutureProvider.family<Article?, String>(
  (Ref ref, String locale) =>
      ref.watch(newsRepositoryProvider).breaking(locale),
);
