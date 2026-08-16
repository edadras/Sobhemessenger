import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/stories_repository.dart';
import 'story_viewer_screen.dart';

/// The horizontal strip of stories above the chat list (§17).
///
/// It collapses to nothing when there are no stories: an empty strip would
/// take vertical space away from the conversation list for no reason.
class StoriesTray extends ConsumerWidget {
  const StoriesTray({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AsyncValue<List<Story>> stories = ref.watch(storyFeedProvider);

    return stories.maybeWhen(
      orElse: SizedBox.shrink,
      data: (List<Story> rows) {
        if (rows.isEmpty) {
          return const SizedBox.shrink();
        }
        return SizedBox(
          height: SobhSizes.avatarLarge,
          child: ListView.separated(
            scrollDirection: Axis.horizontal,
            padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.lg),
            itemCount: rows.length,
            separatorBuilder: (_, __) => const SizedBox(width: SobhSpacing.md),
            itemBuilder: (BuildContext context, int index) => _StoryBubble(
              story: rows[index],
              onTap: () => Navigator.of(context).push(
                MaterialPageRoute<void>(
                  builder: (_) =>
                      StoryViewerScreen(stories: rows, initialIndex: index),
                ),
              ),
            ),
          ),
        );
      },
    );
  }
}

class _StoryBubble extends StatelessWidget {
  const _StoryBubble({required this.story, required this.onTap});

  final Story story;
  final VoidCallback onTap;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return InkWell(
      onTap: onTap,
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Container(
            padding: const EdgeInsets.all(SobhSpacing.xxs),
            decoration: BoxDecoration(
              shape: BoxShape.circle,
              // An unseen story gets a ring; a seen one does not. That is the
              // whole affordance, so it must not be subtle.
              border: Border.all(
                color: story.seenByMe ? palette.outline : palette.primary,
                width: story.seenByMe ? 1 : 2,
              ),
            ),
            child: SobhAvatar(
              name: story.authorName,
              radius: SobhSizes.avatarSmall / 2,
            ),
          ),
          const SizedBox(height: SobhSpacing.xs),
          SizedBox(
            width: SobhSizes.avatarMedium + SobhSpacing.md,
            child: Text(
              story.authorName,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              textAlign: TextAlign.center,
              style: Theme.of(context).textTheme.labelSmall,
            ),
          ),
        ],
      ),
    );
  }
}

/// The stories screen reached from navigation, as opposed to the tray.
class StoriesScreen extends ConsumerWidget {
  const StoriesScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Story>> stories = ref.watch(storyFeedProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.storiesTitle)),
      body: stories.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(storyFeedProvider),
        ),
        data: (List<Story> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.auto_stories_outlined,
                title: l10n.storiesEmpty,
              )
            : ListView.separated(
                itemCount: rows.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Story story = rows[index];
                  return ListTile(
                    leading: SobhAvatar(name: story.authorName),
                    title: Text(story.authorName),
                    subtitle: Text(l10n.storiesViewers(story.viewCount)),
                    onTap: () => Navigator.of(context).push(
                      MaterialPageRoute<void>(
                        builder: (_) => StoryViewerScreen(
                          stories: rows,
                          initialIndex: index,
                        ),
                      ),
                    ),
                  );
                },
              ),
      ),
    );
  }
}
