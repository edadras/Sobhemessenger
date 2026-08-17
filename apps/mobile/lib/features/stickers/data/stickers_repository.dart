import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One image in a set.
class Sticker {
  const Sticker({
    required this.id,
    required this.mediaId,
    this.emoji = '',
    this.position = 0,
  });

  factory Sticker.fromJson(Map<String, dynamic> json) => Sticker(
        id: json['id'] as String,
        mediaId: json['media_id'] as String,
        emoji: json['emoji'] as String? ?? '',
        position: (json['position'] as num?)?.toInt() ?? 0,
      );

  final String id;
  final String mediaId;
  final String emoji;
  final int position;
}

/// A sticker pack (§12).
class StickerSet {
  const StickerSet({
    required this.id,
    required this.slug,
    required this.title,
    this.kind = 'static',
    this.isOfficial = false,
    this.isAdded = false,
    this.stickers = const <Sticker>[],
  });

  factory StickerSet.fromJson(Map<String, dynamic> json) => StickerSet(
        id: json['id'] as String,
        slug: json['slug'] as String? ?? '',
        title: json['title'] as String? ?? '',
        kind: json['kind'] as String? ?? 'static',
        isOfficial: json['is_official'] as bool? ?? false,
        isAdded: json['is_added'] as bool? ?? false,
        stickers: <Sticker>[
          for (final dynamic entry
              in json['stickers'] as List<dynamic>? ?? const <dynamic>[])
            Sticker.fromJson(entry as Map<String, dynamic>),
        ],
      );

  final String id;
  final String slug;
  final String title;
  final String kind;
  final bool isOfficial;
  final bool isAdded;
  final List<Sticker> stickers;
}

/// An unfurled link, as shown under a message.
class LinkPreview {
  const LinkPreview({
    required this.url,
    this.siteName = '',
    this.title = '',
    this.description = '',
    this.imageMediaId,
  });

  factory LinkPreview.fromJson(Map<String, dynamic> json) => LinkPreview(
        url: json['url'] as String? ?? '',
        siteName: json['site_name'] as String? ?? '',
        title: json['title'] as String? ?? '',
        description: json['description'] as String? ?? '',
        imageMediaId: json['image_media_id'] as String?,
      );

  final String url;
  final String siteName;
  final String title;
  final String description;
  final String? imageMediaId;
}

/// Sticker sets and link previews (§12).
class StickersRepository {
  StickersRepository(this._api);

  final ApiClient _api;

  Future<List<StickerSet>> added() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/stickers');
    return _sets(data['sets']);
  }

  Future<List<StickerSet>> search(String query) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/stickers/search',
      query: <String, dynamic>{'q': query},
    );
    return _sets(data['sets']);
  }

  Future<StickerSet> bySlug(String slug) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/stickers/$slug');
    return StickerSet.fromJson(data);
  }

  Future<void> add(String setId) => _api.put<dynamic>('/stickers/$setId/added');

  Future<void> remove(String setId) =>
      _api.delete<dynamic>('/stickers/$setId/added');

  /// Unfurls a link.
  ///
  /// Returns null rather than throwing when there is nothing to show: a link
  /// with no preview is the ordinary case, not an error worth surfacing.
  Future<LinkPreview?> preview(String url) async {
    try {
      final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
        '/stickers/link-preview',
        query: <String, dynamic>{'url': url},
      );
      return LinkPreview.fromJson(data);
    } on Object {
      return null;
    }
  }

  static List<StickerSet> _sets(dynamic raw) => <StickerSet>[
        for (final dynamic entry in raw as List<dynamic>? ?? const <dynamic>[])
          StickerSet.fromJson(entry as Map<String, dynamic>),
      ];
}

final Provider<StickersRepository> stickersRepositoryProvider =
    Provider<StickersRepository>(
  (Ref ref) => StickersRepository(ref.watch(apiClientProvider)),
);

/// The sets the caller has installed, in their own order.
final FutureProvider<List<StickerSet>> installedStickerSetsProvider =
    FutureProvider<List<StickerSet>>(
  (Ref ref) => ref.watch(stickersRepositoryProvider).added(),
);

/// A preview for one URL, cached per URL for the life of the screen so the
/// same link in a scrolling list is unfurled once.
final FutureProviderFamily<LinkPreview?, String> linkPreviewProvider =
    FutureProvider.family<LinkPreview?, String>(
  (Ref ref, String url) => ref.watch(stickersRepositoryProvider).preview(url),
);
