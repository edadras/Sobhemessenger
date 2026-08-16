import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/settings/settings_controller.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/news_repository.dart';

/// One article (§26).
class ArticleScreen extends ConsumerStatefulWidget {
  const ArticleScreen({super.key, required this.slug});

  final String slug;

  @override
  ConsumerState<ArticleScreen> createState() => _ArticleScreenState();
}

class _ArticleScreenState extends ConsumerState<ArticleScreen> {
  Future<Article>? _request;
  bool? _bookmarkedOverride;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final String locale =
        ref.watch(settingsControllerProvider).locale.languageCode;
    _request ??= ref.read(newsRepositoryProvider).article(widget.slug, locale);

    return Scaffold(
      body: FutureBuilder<Article>(
        future: _request,
        builder: (BuildContext context, AsyncSnapshot<Article> snapshot) {
          if (snapshot.connectionState != ConnectionState.done) {
            return const Scaffold(body: SobhLoading());
          }
          if (snapshot.hasError) {
            return Scaffold(
              appBar: AppBar(),
              body: SobhErrorState(
                error: snapshot.error!,
                onRetry: () => setState(() {
                  _request = ref
                      .read(newsRepositoryProvider)
                      .article(widget.slug, locale);
                }),
              ),
            );
          }

          final Article article = snapshot.data!;
          final bool bookmarked = _bookmarkedOverride ?? article.isBookmarked;

          return CustomScrollView(
            slivers: <Widget>[
              SliverAppBar(
                pinned: true,
                title: Text(article.categoryName),
                actions: <Widget>[
                  IconButton(
                    tooltip: l10n.newsBookmarks,
                    icon: Icon(
                      bookmarked ? Icons.bookmark : Icons.bookmark_border,
                    ),
                    onPressed: () async {
                      // Resolved before the await: this context belongs to the
                      // builder, so State.mounted does not vouch for it.
                      final ScaffoldMessengerState messenger =
                          ScaffoldMessenger.of(context);

                      // The icon flips immediately and reverts if the server
                      // refuses: a bookmark is not worth a spinner.
                      setState(() => _bookmarkedOverride = !bookmarked);
                      try {
                        await ref
                            .read(newsRepositoryProvider)
                            .setBookmark(article.id, !bookmarked);
                        ref.invalidate(newsBookmarksProvider(locale));
                      } on Exception {
                        if (mounted) {
                          setState(() => _bookmarkedOverride = bookmarked);
                          messenger.showSnackBar(
                            SnackBar(content: Text(l10n.errorGeneric)),
                          );
                        }
                      }
                    },
                  ),
                ],
              ),
              SliverPadding(
                padding: const EdgeInsets.all(SobhSpacing.lg),
                sliver: SliverList.list(
                  children: <Widget>[
                    Text(
                      article.title,
                      style: Theme.of(context).textTheme.headlineSmall,
                    ),
                    if (article.subtitle.isNotEmpty) ...<Widget>[
                      const SizedBox(height: SobhSpacing.sm),
                      Text(
                        article.subtitle,
                        style: Theme.of(context).textTheme.titleMedium,
                      ),
                    ],
                    const SizedBox(height: SobhSpacing.md),
                    _Byline(article: article),
                    const SizedBox(height: SobhSpacing.lg),
                    if (article.lead.isNotEmpty) ...<Widget>[
                      Text(
                        article.lead,
                        style: Theme.of(context).textTheme.bodyLarge?.copyWith(
                              fontWeight: FontWeight.w600,
                            ),
                      ),
                      const SizedBox(height: SobhSpacing.lg),
                    ],
                    Text(
                      article.body,
                      style: Theme.of(context).textTheme.bodyLarge,
                    ),
                    const SizedBox(height: SobhSpacing.xxl),
                  ],
                ),
              ),
            ],
          );
        },
      ),
    );
  }
}

class _Byline extends StatelessWidget {
  const _Byline({required this.article});

  final Article article;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Text(
      <String>[
        if (article.authorName.isNotEmpty) article.authorName,
        if (article.readingMinutes > 0)
          l10n.newsReadingTime(article.readingMinutes),
      ].join(' · '),
      style: Theme.of(context)
          .textTheme
          .bodySmall
          ?.copyWith(color: palette.textSecondary),
    );
  }
}
