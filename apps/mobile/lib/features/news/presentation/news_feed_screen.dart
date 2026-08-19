import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/settings/settings_controller.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/news_repository.dart';
import 'article_screen.dart';

/// The news feed (§25).
class NewsFeedScreen extends ConsumerStatefulWidget {
  const NewsFeedScreen({super.key});

  @override
  ConsumerState<NewsFeedScreen> createState() => _NewsFeedScreenState();
}

class _NewsFeedScreenState extends ConsumerState<NewsFeedScreen> {
  FeedMode _mode = FeedMode.latest;
  String? _categoryId;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final String locale =
        ref.watch(settingsControllerProvider).locale.languageCode;
    final FeedQuery query =
        FeedQuery(mode: _mode, locale: locale, categoryId: _categoryId);
    final AsyncValue<List<Article>> feed = ref.watch(newsFeedProvider(query));

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.newsTitle),
        actions: <Widget>[
          IconButton(
            tooltip: l10n.newsBookmarks,
            icon: const Icon(Icons.bookmark_border),
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const BookmarksScreen()),
            ),
          ),
        ],
        bottom: PreferredSize(
          preferredSize:
              const Size.fromHeight(SobhSpacing.xxl + SobhSpacing.lg),
          child: _FeedControls(
            mode: _mode,
            categoryId: _categoryId,
            locale: locale,
            onModeChanged: (FeedMode mode) => setState(() => _mode = mode),
            onCategoryChanged: (String? id) => setState(() => _categoryId = id),
          ),
        ),
      ),
      body: feed.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(newsFeedProvider(query)),
        ),
        data: (List<Article> articles) {
          if (articles.isEmpty) {
            return SobhEmptyState(
              icon: Icons.article_outlined,
              title: l10n.newsEmpty,
            );
          }
          return RefreshIndicator(
            onRefresh: () async {
              ref
                ..invalidate(newsFeedProvider(query))
                ..invalidate(breakingArticleProvider(locale));
            },
            child: CustomScrollView(
              slivers: <Widget>[
                // Above the list rather than in it. A story flagged as
                // breaking is buried among fifty others if it is only sorted
                // differently, and the reader has to know to look for it.
                // Only on the ordinary feed: the breaking *mode* is already a
                // list of these, and a banner over its own contents is noise.
                if (_mode != FeedMode.breaking)
                  SliverToBoxAdapter(child: _BreakingBanner(locale: locale)),
                SliverPadding(
                  padding:
                      const EdgeInsets.symmetric(vertical: SobhSpacing.sm),
                  sliver: SliverList.separated(
                    itemCount: articles.length,
                    separatorBuilder: (_, __) => const Divider(height: 1),
                    itemBuilder: (BuildContext context, int index) =>
                        ArticleTile(article: articles[index]),
                  ),
                ),
              ],
            ),
          );
        },
      ),
    );
  }
}

class _FeedControls extends ConsumerWidget {
  const _FeedControls({
    required this.mode,
    required this.categoryId,
    required this.locale,
    required this.onModeChanged,
    required this.onCategoryChanged,
  });

  final FeedMode mode;
  final String? categoryId;
  final String locale;
  final ValueChanged<FeedMode> onModeChanged;
  final ValueChanged<String?> onCategoryChanged;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<NewsCategory>> categories =
        ref.watch(newsCategoriesProvider(locale));

    String label(FeedMode value) => switch (value) {
          FeedMode.latest => l10n.newsLatest,
          FeedMode.popular => l10n.newsPopular,
          FeedMode.following => l10n.newsFollowing,
          FeedMode.breaking => l10n.newsBreaking,
        };

    return SingleChildScrollView(
      scrollDirection: Axis.horizontal,
      padding: const EdgeInsets.symmetric(
        horizontal: SobhSpacing.lg,
        vertical: SobhSpacing.sm,
      ),
      child: Row(
        children: <Widget>[
          for (final FeedMode value in FeedMode.values) ...<Widget>[
            ChoiceChip(
              label: Text(label(value)),
              selected: mode == value,
              onSelected: (_) => onModeChanged(value),
            ),
            const SizedBox(width: SobhSpacing.sm),
          ],
          // Categories only appear once they have loaded; a failure here must
          // not hide the feed itself.
          ...categories.maybeWhen(
            orElse: () => const <Widget>[],
            data: (List<NewsCategory> rows) => <Widget>[
              const SizedBox(width: SobhSpacing.sm),
              for (final NewsCategory category in rows) ...<Widget>[
                // Long-pressing follows the category, which is what the
                // `following` feed mode reads. Following was in the
                // repository and reachable from nowhere, so that mode was a
                // list that could only ever be empty.
                GestureDetector(
                  onLongPress: () => _toggleFollow(context, ref, category),
                  child: FilterChip(
                    label: Text(category.name),
                    selected: categoryId == category.id,
                    onSelected: (bool selected) =>
                        onCategoryChanged(selected ? category.id : null),
                  ),
                ),
                const SizedBox(width: SobhSpacing.sm),
              ],
            ],
          ),
        ],
      ),
    );
  }
}

/// One article in a list. Shared by the feed and the bookmarks screen.
class ArticleTile extends ConsumerWidget {
  const ArticleTile({super.key, required this.article});

  final Article article;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;

    return ListTile(
      onTap: () => Navigator.of(context).push(
        MaterialPageRoute<void>(
          builder: (_) => ArticleScreen(slug: article.slug),
        ),
      ),
      title: Row(
        children: <Widget>[
          if (article.isBreaking) ...<Widget>[
            Container(
              padding: const EdgeInsets.symmetric(
                horizontal: SobhSpacing.sm,
                vertical: SobhSpacing.xxs,
              ),
              decoration: BoxDecoration(
                color: palette.error,
                borderRadius: BorderRadius.circular(SobhRadius.sm),
              ),
              child: Text(
                l10n.newsBreaking,
                style: text.labelSmall?.copyWith(color: palette.onPrimary),
              ),
            ),
            const SizedBox(width: SobhSpacing.sm),
          ],
          Expanded(
            child: Text(
              article.title,
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
            ),
          ),
        ],
      ),
      subtitle: Padding(
        padding: const EdgeInsets.only(top: SobhSpacing.xs),
        child: Text(
          <String>[
            if (article.categoryName.isNotEmpty) article.categoryName,
            if (article.readingMinutes > 0)
              l10n.newsReadingTime(article.readingMinutes),
          ].join(' · '),
          style: text.bodySmall?.copyWith(color: palette.textSecondary),
        ),
      ),
    );
  }
}

/// Saved articles (§26).
class BookmarksScreen extends ConsumerWidget {
  const BookmarksScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final String locale =
        ref.watch(settingsControllerProvider).locale.languageCode;
    final AsyncValue<List<Article>> bookmarks =
        ref.watch(newsBookmarksProvider(locale));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.newsBookmarks)),
      body: bookmarks.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(newsBookmarksProvider(locale)),
        ),
        data: (List<Article> articles) => articles.isEmpty
            ? SobhEmptyState(
                icon: Icons.bookmark_border,
                title: l10n.newsBookmarksEmpty,
              )
            : ListView.separated(
                itemCount: articles.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) =>
                    ArticleTile(article: articles[index]),
              ),
      ),
    );
  }
}

/// Follows or unfollows a category, and says which it did.
///
/// The server holds one row per follow, so there is nothing to read back
/// before deciding — the switch is in the confirmation, not in the chip, which
/// keeps the chip meaning "filter by this" and nothing else.
Future<void> _toggleFollow(
  BuildContext context,
  WidgetRef ref,
  NewsCategory category,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final bool? follow = await showModalBottomSheet<bool>(
    context: context,
    builder: (BuildContext context) => SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          ListTile(
            leading: const Icon(Icons.notifications_active_outlined),
            title: Text(l10n.newsFollowCategory(category.name)),
            onTap: () => Navigator.of(context).pop(true),
          ),
          ListTile(
            leading: const Icon(Icons.notifications_off_outlined),
            title: Text(l10n.newsUnfollowCategory(category.name)),
            onTap: () => Navigator.of(context).pop(false),
          ),
        ],
      ),
    ),
  );
  if (follow == null || !context.mounted) {
    return;
  }

  final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
  try {
    await ref
        .read(newsRepositoryProvider)
        .setFollow(categoryId: category.id, following: follow);
    messenger.showSnackBar(
      SnackBar(
        content: Text(follow ? l10n.newsFollowed : l10n.newsUnfollowed),
      ),
    );
  } on ApiException catch (error) {
    messenger.showSnackBar(
      SnackBar(
        content: Text(error.isOffline ? l10n.errorNetwork : error.message),
      ),
    );
  }
}

/// The single story currently marked as breaking.
///
/// Renders nothing at all when there is none, and nothing when the request
/// fails: a banner that cannot load is not worth an error over a feed that
/// loaded perfectly well, and an empty box where news used to be would be
/// read as news having stopped.
class _BreakingBanner extends ConsumerWidget {
  const _BreakingBanner({required this.locale});

  final String locale;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final Article? article =
        ref.watch(breakingArticleProvider(locale)).valueOrNull;
    if (article == null) {
      return const SizedBox.shrink();
    }

    return Padding(
      padding: const EdgeInsets.fromLTRB(
        SobhSpacing.md,
        SobhSpacing.md,
        SobhSpacing.md,
        0,
      ),
      child: Material(
        color: palette.error.withValues(alpha: 0.08),
        borderRadius: BorderRadius.circular(SobhRadius.md),
        child: InkWell(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          onTap: () => Navigator.of(context).push(
            MaterialPageRoute<void>(
              builder: (_) => ArticleScreen(slug: article.slug),
            ),
          ),
          child: Padding(
            padding: const EdgeInsets.all(SobhSpacing.md),
            child: Row(
              children: <Widget>[
                Icon(Icons.bolt, color: palette.error),
                const SizedBox(width: SobhSpacing.md),
                Expanded(
                  child: Column(
                    crossAxisAlignment: CrossAxisAlignment.start,
                    mainAxisSize: MainAxisSize.min,
                    children: <Widget>[
                      Text(
                        l10n.newsBreaking,
                        style: Theme.of(context)
                            .textTheme
                            .labelSmall
                            ?.copyWith(color: palette.error),
                      ),
                      Text(
                        article.title,
                        maxLines: 2,
                        overflow: TextOverflow.ellipsis,
                        style: Theme.of(context).textTheme.titleSmall,
                      ),
                    ],
                  ),
                ),
              ],
            ),
          ),
        ),
      ),
    );
  }
}
