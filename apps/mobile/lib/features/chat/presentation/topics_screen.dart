import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/topics_repository.dart';

/// The topics inside a forum (§14).
///
/// A topic is not a chat: membership, permissions and moderation stay at the
/// group level. What this screen offers is the partition — which thread a
/// message belongs to, and which threads have something new in them.
class TopicsScreen extends ConsumerWidget {
  const TopicsScreen({
    required this.chatId,
    this.canModerate = false,
    super.key,
  });

  final String chatId;

  /// Closing, hiding, pinning and deleting are moderation; opening a topic is
  /// not. The caller knows the viewer's role, so it says so rather than this
  /// screen guessing from a failed request.
  final bool canModerate;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ForumTopic>> topics =
        ref.watch(forumTopicsProvider(chatId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.topicsTitle)),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => _create(context, ref),
        icon: const Icon(Icons.add_comment_outlined),
        label: Text(l10n.topicsNew),
      ),
      body: topics.when(
        loading: () => const Center(child: CircularProgressIndicator()),
        error: (Object error, StackTrace stack) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(forumTopicsProvider(chatId)),
        ),
        data: (List<ForumTopic> rows) {
          if (rows.isEmpty) {
            return Center(child: Text(l10n.topicsEmpty));
          }
          return ListView.separated(
            itemCount: rows.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) => _TopicTile(
              chatId: chatId,
              topic: rows[index],
              canModerate: canModerate,
            ),
          );
        },
      ),
    );
  }

  Future<void> _create(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TextEditingController controller = TextEditingController();

    final String? title = await showDialog<String>(
      context: context,
      builder: (BuildContext context) => AlertDialog(
        title: Text(l10n.topicsNew),
        content: TextField(
          controller: controller,
          autofocus: true,
          decoration: InputDecoration(labelText: l10n.topicsName),
        ),
        actions: <Widget>[
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: Text(l10n.commonCancel),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(controller.text.trim()),
            child: Text(l10n.commonSave),
          ),
        ],
      ),
    );
    controller.dispose();
    if (title == null || title.isEmpty) {
      return;
    }

    try {
      await ref
          .read(topicsRepositoryProvider)
          .create(chatId: chatId, title: title);
      ref.invalidate(forumTopicsProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }
}

class _TopicTile extends ConsumerWidget {
  const _TopicTile({
    required this.chatId,
    required this.topic,
    required this.canModerate,
  });

  final String chatId;
  final ForumTopic topic;
  final bool canModerate;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return ListTile(
      leading: Text(
        topic.iconEmoji.isEmpty ? '💬' : topic.iconEmoji,
        style: const TextStyle(fontSize: SobhSizes.iconMedium),
      ),
      title: Row(
        children: <Widget>[
          Flexible(
            child: Text(
              topic.isGeneral ? l10n.topicsGeneral : topic.title,
              overflow: TextOverflow.ellipsis,
            ),
          ),
          if (topic.isPinned)
            const Padding(
              padding: EdgeInsets.only(left: SobhSpacing.xs),
              child: Icon(Icons.push_pin, size: SobhSizes.iconSmall),
            ),
          if (topic.isClosed)
            Padding(
              padding: const EdgeInsets.only(left: SobhSpacing.xs),
              child: Text(
                '· ${l10n.topicsClosed}',
                style: Theme.of(context).textTheme.labelSmall,
              ),
            ),
        ],
      ),
      subtitle: Text(l10n.topicsMessageCount(topic.messageCount)),
      trailing: _trailing(context, ref, l10n),
    );
  }

  Widget? _trailing(
    BuildContext context,
    WidgetRef ref,
    AppLocalizations l10n,
  ) {
    final Widget? badge = topic.unreadCount > 0
        ? Badge(label: Text('${topic.unreadCount}'))
        : null;
    if (!canModerate) {
      return badge;
    }

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        if (badge != null) badge,
        PopupMenuButton<String>(
          onSelected: (String action) => _act(context, ref, action),
          itemBuilder: (BuildContext context) => <PopupMenuEntry<String>>[
            PopupMenuItem<String>(
              value: topic.isClosed ? 'reopen' : 'close',
              child:
                  Text(topic.isClosed ? l10n.topicsReopen : l10n.topicsClose),
            ),
            PopupMenuItem<String>(
              value: topic.isPinned ? 'unpin' : 'pin',
              child: Text(topic.isPinned ? l10n.topicsUnpin : l10n.topicsPin),
            ),
            PopupMenuItem<String>(
              value: 'hide',
              child: Text(l10n.topicsHide),
            ),
            // The General topic holds the group's converted history, so it is
            // the one thing here that cannot be deleted.
            if (!topic.isGeneral)
              PopupMenuItem<String>(
                value: 'delete',
                child: Text(l10n.commonDelete),
              ),
          ],
        ),
      ],
    );
  }

  Future<void> _act(
    BuildContext context,
    WidgetRef ref,
    String action,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TopicsRepository repository = ref.read(topicsRepositoryProvider);

    try {
      switch (action) {
        case 'close':
        case 'reopen':
          await repository.update(
            chatId,
            topic.id,
            isClosed: action == 'close',
          );
        case 'pin':
        case 'unpin':
          await repository.update(chatId, topic.id, isPinned: action == 'pin');
        case 'hide':
          await repository.update(chatId, topic.id, isHidden: true);
        case 'delete':
          final bool? confirmed = await showDialog<bool>(
            context: context,
            builder: (BuildContext context) => AlertDialog(
              title: Text(topic.title),
              content: Text(l10n.topicsDeleteConfirm),
              actions: <Widget>[
                TextButton(
                  onPressed: () => Navigator.of(context).pop(false),
                  child: Text(l10n.commonCancel),
                ),
                FilledButton(
                  onPressed: () => Navigator.of(context).pop(true),
                  child: Text(l10n.commonDelete),
                ),
              ],
            ),
          );
          if (confirmed != true) {
            return;
          }
          await repository.delete(chatId, topic.id);
      }
      ref.invalidate(forumTopicsProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }
}
