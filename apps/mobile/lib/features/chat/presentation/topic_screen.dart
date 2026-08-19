import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../data/topics_repository.dart';

/// One forum topic's conversation (§14).
///
/// Per-topic reading was the point of forums and the app never used it: the
/// topic list showed how many messages each held and tapping one did nothing,
/// so a forum was a chat with labels on it.
///
/// The history is fetched rather than cached. The offline store is per chat,
/// and keeping a second copy filed per topic would be the same rows tracked
/// twice — for a screen people open deliberately rather than live in.
class TopicScreen extends ConsumerStatefulWidget {
  const TopicScreen({
    super.key,
    required this.chatId,
    required this.topic,
  });

  final String chatId;
  final ForumTopic topic;

  @override
  ConsumerState<TopicScreen> createState() => _TopicScreenState();
}

class _TopicScreenState extends ConsumerState<TopicScreen> {
  final TextEditingController _composer = TextEditingController();
  bool _sending = false;

  /// How far this topic has been acknowledged, so scrolling does not send the
  /// same read repeatedly.
  int _markedReadUpTo = 0;

  @override
  void dispose() {
    _composer.dispose();
    super.dispose();
  }

  (String, String) get _key => (widget.chatId, widget.topic.id);

  Future<void> _send() async {
    final String text = _composer.text.trim();
    if (text.isEmpty || _sending) {
      return;
    }

    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    setState(() => _sending = true);
    _composer.clear();

    try {
      await ref.read(topicsRepositoryProvider).post(
            chatId: widget.chatId,
            topicId: widget.topic.id,
            content: text,
          );
      ref.invalidate(topicMessagesProvider(_key));
    } on ApiException catch (error) {
      // Put the text back: it is not stored anywhere else, and losing what
      // somebody just wrote is worse than showing them the refusal.
      _composer.text = text;
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    } finally {
      if (mounted) {
        setState(() => _sending = false);
      }
    }
  }

  /// Acknowledges the topic up to the furthest message on screen.
  ///
  /// A topic keeps its own cursor, which is what makes an unread badge per
  /// topic mean anything — reading one does not clear the others.
  Future<void> _markRead(List<TopicMessage> messages) async {
    final int highest = messages.fold<int>(
      0,
      (int furthest, TopicMessage m) => m.seq > furthest ? m.seq : furthest,
    );
    if (highest <= _markedReadUpTo) {
      return;
    }
    _markedReadUpTo = highest;

    try {
      await ref
          .read(topicsRepositoryProvider)
          .markRead(widget.chatId, widget.topic.id, highest);
    } on ApiException {
      // Offline. The badge clears next time; an error over a topic somebody is
      // reading would be noise.
      _markedReadUpTo = 0;
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final String? me = ref.watch(sessionControllerProvider).userId;
    final AsyncValue<List<TopicMessage>> messages =
        ref.watch(topicMessagesProvider(_key));

    messages.whenData((List<TopicMessage> rows) {
      if (rows.isNotEmpty) {
        WidgetsBinding.instance.addPostFrameCallback((_) {
          if (mounted) {
            _markRead(rows);
          }
        });
      }
    });

    return Scaffold(
      backgroundColor: palette.chatBackground,
      appBar: AppBar(
        title: Row(
          children: <Widget>[
            Text(widget.topic.iconEmoji.isEmpty ? '💬' : widget.topic.iconEmoji),
            const SizedBox(width: SobhSpacing.sm),
            Flexible(
              child: Text(
                widget.topic.isGeneral
                    ? l10n.topicsGeneral
                    : widget.topic.title,
                overflow: TextOverflow.ellipsis,
              ),
            ),
          ],
        ),
      ),
      body: Column(
        children: <Widget>[
          Expanded(
            child: messages.when(
              loading: () => const SobhLoading(),
              error: (Object error, StackTrace _) => SobhErrorState(
                error: error,
                onRetry: () => ref.invalidate(topicMessagesProvider(_key)),
              ),
              data: (List<TopicMessage> rows) {
                if (rows.isEmpty) {
                  return SobhEmptyState(
                    icon: Icons.forum_outlined,
                    title: l10n.topicEmptyTitle,
                    body: l10n.topicEmptyBody,
                  );
                }
                return ListView.builder(
                  reverse: true,
                  padding: const EdgeInsets.all(SobhSpacing.md),
                  itemCount: rows.length,
                  itemBuilder: (BuildContext context, int index) {
                    final TopicMessage message = rows[rows.length - 1 - index];
                    return _TopicBubble(
                      message: message,
                      isOutgoing: message.senderId == me,
                    );
                  },
                );
              },
            ),
          ),
          // A closed topic is read-only, which is what closing one means.
          // Showing a compose box that would be refused is worse than not
          // showing one.
          if (widget.topic.isClosed)
            Padding(
              padding: const EdgeInsets.all(SobhSpacing.lg),
              child: Text(
                l10n.topicsClosedNotice,
                textAlign: TextAlign.center,
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.textSecondary),
              ),
            )
          else
            SafeArea(
              child: Padding(
                padding: const EdgeInsets.all(SobhSpacing.sm),
                child: Row(
                  children: <Widget>[
                    Expanded(
                      child: TextField(
                        controller: _composer,
                        enabled: !_sending,
                        minLines: 1,
                        maxLines: 5,
                        decoration: InputDecoration(
                          hintText: l10n.chatMessageHint,
                          border: InputBorder.none,
                        ),
                        onSubmitted: (_) => _send(),
                      ),
                    ),
                    IconButton(
                      onPressed: _sending ? null : _send,
                      icon: const Icon(Icons.send),
                    ),
                  ],
                ),
              ),
            ),
        ],
      ),
    );
  }
}

class _TopicBubble extends StatelessWidget {
  const _TopicBubble({required this.message, required this.isOutgoing});

  final TopicMessage message;
  final bool isOutgoing;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Align(
      alignment:
          isOutgoing ? AlignmentDirectional.centerEnd : AlignmentDirectional.centerStart,
      child: Container(
        margin: const EdgeInsets.symmetric(vertical: SobhSpacing.xxs),
        padding: const EdgeInsets.symmetric(
          horizontal: SobhSpacing.md,
          vertical: SobhSpacing.sm,
        ),
        decoration: BoxDecoration(
          color: isOutgoing ? palette.bubbleOutgoing : palette.bubbleIncoming,
          borderRadius: BorderRadius.circular(SobhRadius.lg),
          border: Border.all(color: palette.outline),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            if (!isOutgoing && message.senderName.isNotEmpty)
              Text(
                message.senderName,
                style: Theme.of(context)
                    .textTheme
                    .labelSmall
                    ?.copyWith(color: palette.primary),
              ),
            Text(
              message.isDeleted ? l10n.chatMessageDeleted : message.content,
              style: TextStyle(
                color: isOutgoing
                    ? palette.bubbleOutgoingText
                    : palette.bubbleIncomingText,
                fontStyle:
                    message.isDeleted ? FontStyle.italic : FontStyle.normal,
              ),
            ),
            Text(
              DateFormat.Hm().format(message.createdAt),
              style: Theme.of(context)
                  .textTheme
                  .labelSmall
                  ?.copyWith(color: palette.textSecondary),
            ),
          ],
        ),
      ),
    );
  }
}
