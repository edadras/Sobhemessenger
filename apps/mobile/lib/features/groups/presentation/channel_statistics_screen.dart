import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../chat/data/chat_repository.dart';
import '../data/groups_repository.dart';

/// Reach figures for recent channel posts (§15).
///
/// The server answers per message rather than for the channel as a whole, so
/// this asks about the posts this device is holding. That is deliberate: the
/// numbers line up with posts the author can actually see and scroll to,
/// rather than being a total with nothing behind it.
class ChannelStatisticsScreen extends ConsumerWidget {
  const ChannelStatisticsScreen({super.key, required this.chatId});

  final String chatId;

  /// How many recent posts to ask about. Every id goes into the query string,
  /// so this is bounded rather than "everything the device has".
  static const int _postLimit = 30;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<MessageRow>> messages =
        ref.watch(chatMessagesProvider(chatId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.channelStatisticsTitle)),
      body: messages.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(chatMessagesProvider(chatId)),
        ),
        data: (List<MessageRow> rows) {
          final List<MessageRow> posts = rows.reversed
              .where(
                (MessageRow row) =>
                    row.deletedAt == null &&
                    row.status == MessageStatus.sent &&
                    row.id != row.clientMessageId,
              )
              .take(_postLimit)
              .toList();

          if (posts.isEmpty) {
            return SobhEmptyState(
              icon: Icons.query_stats,
              title: l10n.channelStatisticsEmpty,
            );
          }

          return _Figures(chatId: chatId, posts: posts);
        },
      ),
    );
  }
}

class _Figures extends ConsumerWidget {
  const _Figures({required this.chatId, required this.posts});

  final String chatId;
  final List<MessageRow> posts;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return FutureBuilder<List<PostStatistics>>(
      future: ref.read(groupsRepositoryProvider).postStatistics(
            chatId,
            posts.map((MessageRow row) => row.id).toList(growable: false),
          ),
      builder: (
        BuildContext context,
        AsyncSnapshot<List<PostStatistics>> snapshot,
      ) {
        if (snapshot.connectionState == ConnectionState.waiting) {
          return const SobhLoading();
        }
        if (snapshot.hasError) {
          return SobhErrorState(error: snapshot.error!);
        }

        // A post nobody has opened yet has no row of its own; showing zeros is
        // the honest answer, and dropping it would make the list disagree with
        // the channel.
        final Map<String, PostStatistics> byId = <String, PostStatistics>{
          for (final PostStatistics entry
              in snapshot.data ?? const <PostStatistics>[])
            entry.messageId: entry,
        };

        return ListView.separated(
          itemCount: posts.length,
          separatorBuilder: (_, __) => const Divider(height: 1),
          itemBuilder: (BuildContext context, int index) {
            final MessageRow post = posts[index];
            final PostStatistics figures = byId[post.id] ??
                PostStatistics(
                  messageId: post.id,
                  viewCount: 0,
                  forwardCount: 0,
                  reactionCount: 0,
                  commentCount: 0,
                );

            return ListTile(
              title: Text(
                post.content.isEmpty
                    ? l10n.channelStatisticsUntitledPost
                    : post.content,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
              ),
              subtitle: Padding(
                padding: const EdgeInsets.only(top: SobhSpacing.xs),
                child: Wrap(
                  spacing: SobhSpacing.md,
                  children: <Widget>[
                    _Figure(
                      icon: Icons.visibility_outlined,
                      label: l10n.channelStatisticsViews,
                      value: figures.viewCount,
                    ),
                    _Figure(
                      icon: Icons.forward_outlined,
                      label: l10n.channelStatisticsForwards,
                      value: figures.forwardCount,
                    ),
                    _Figure(
                      icon: Icons.emoji_emotions_outlined,
                      label: l10n.channelStatisticsReactions,
                      value: figures.reactionCount,
                    ),
                    _Figure(
                      icon: Icons.mode_comment_outlined,
                      label: l10n.channelStatisticsComments,
                      value: figures.commentCount,
                    ),
                  ],
                ),
              ),
              trailing: Text(
                DateFormat.Md().format(post.createdAt.toLocal()),
                style: TextStyle(color: palette.textSecondary),
              ),
            );
          },
        );
      },
    );
  }
}

class _Figure extends StatelessWidget {
  const _Figure({
    required this.icon,
    required this.label,
    required this.value,
  });

  final IconData icon;
  final String label;
  final int value;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    return Tooltip(
      message: label,
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Icon(icon, size: SobhSizes.iconSmall, color: palette.textSecondary),
          const SizedBox(width: SobhSpacing.xs),
          Text(
            NumberFormat.decimalPattern().format(value),
            style: Theme.of(context)
                .textTheme
                .bodySmall
                ?.copyWith(color: palette.textSecondary),
          ),
        ],
      ),
    );
  }
}
