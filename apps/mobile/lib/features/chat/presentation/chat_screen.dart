import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../auth/session_controller.dart';
import '../data/chat_repository.dart';

/// A single conversation (§49).
class ChatScreen extends ConsumerStatefulWidget {
  const ChatScreen({required this.chatId, super.key});

  final String chatId;

  @override
  ConsumerState<ChatScreen> createState() => _ChatScreenState();
}

class _ChatScreenState extends ConsumerState<ChatScreen> {
  final TextEditingController _composer = TextEditingController();
  final ScrollController _scrollController = ScrollController();

  @override
  void dispose() {
    _composer.dispose();
    _scrollController.dispose();
    super.dispose();
  }

  Future<void> _send() async {
    final String text = _composer.text.trim();
    if (text.isEmpty) {
      return;
    }
    // Clear immediately: the message is already stored locally, so there is
    // nothing to roll back if the network is down.
    _composer.clear();
    await ref
        .read(chatRepositoryProvider)
        .sendText(chatId: widget.chatId, content: text);
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<MessageRow>> messages =
        ref.watch(chatMessagesProvider(widget.chatId));
    final String? currentUserId = ref.watch(sessionControllerProvider).userId;

    return Scaffold(
      backgroundColor: palette.chatBackground,
      appBar: AppBar(
        title: Text(l10n.navChats),
        actions: <Widget>[
          IconButton(
            icon: const Icon(Icons.info_outline),
            tooltip: l10n.groupsInfoTitle,
            onPressed: () => context.push('/chats/${widget.chatId}/info'),
          ),
        ],
      ),
      body: Column(
        children: <Widget>[
          Expanded(
            child: messages.when(
              loading: () => const Center(child: CircularProgressIndicator()),
              error: (Object error, StackTrace stack) =>
                  Center(child: Text(l10n.errorGeneric)),
              data: (List<MessageRow> rows) => ListView.builder(
                controller: _scrollController,
                reverse: true,
                padding: const EdgeInsets.symmetric(
                  horizontal: SobhSpacing.md,
                  vertical: SobhSpacing.sm,
                ),
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) {
                  final MessageRow message = rows[rows.length - 1 - index];
                  return _MessageBubble(
                    message: message,
                    isOutgoing: message.senderId == null ||
                        message.senderId == currentUserId,
                  );
                },
              ),
            ),
          ),
          _Composer(
            controller: _composer,
            onSend: _send,
            hint: l10n.chatMessageHint,
          ),
        ],
      ),
    );
  }
}

class _MessageBubble extends StatelessWidget {
  const _MessageBubble({required this.message, required this.isOutgoing});

  final MessageRow message;
  final bool isOutgoing;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;
    final bool isDeleted = message.deletedAt != null;

    return Align(
      alignment: isOutgoing
          ? AlignmentDirectional.centerEnd
          : AlignmentDirectional.centerStart,
      child: ConstrainedBox(
        constraints: BoxConstraints(
          maxWidth: MediaQuery.sizeOf(context).width *
              SobhSizes.maxBubbleWidthFraction,
        ),
        child: Container(
          margin: const EdgeInsets.symmetric(vertical: SobhSpacing.xs),
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
            children: <Widget>[
              Text(
                isDeleted ? l10n.chatMessageDeleted : message.content,
                style: text.bodyMedium?.copyWith(
                  color: isOutgoing
                      ? palette.bubbleOutgoingText
                      : palette.bubbleIncomingText,
                  fontStyle: isDeleted ? FontStyle.italic : FontStyle.normal,
                ),
              ),
              const SizedBox(height: SobhSpacing.xxs),
              Row(
                mainAxisSize: MainAxisSize.min,
                children: <Widget>[
                  if (message.editedAt != null)
                    Text(l10n.chatMessageEdited, style: text.labelSmall),
                  const SizedBox(width: SobhSpacing.xs),
                  if (isOutgoing) _StatusIcon(status: message.status),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }
}

/// The delivery ticks. `pending` and `failed` only ever appear on this device —
/// they describe the outbox, not server state (§7).
class _StatusIcon extends StatelessWidget {
  const _StatusIcon({required this.status});

  final MessageStatus status;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    final (IconData icon, Color color) = switch (status) {
      MessageStatus.pending => (Icons.schedule, palette.textDisabled),
      MessageStatus.sending => (Icons.schedule, palette.textDisabled),
      MessageStatus.sent => (Icons.check, palette.textSecondary),
      MessageStatus.delivered => (Icons.done_all, palette.textSecondary),
      MessageStatus.read => (Icons.done_all, palette.info),
      MessageStatus.failed => (Icons.error_outline, palette.error),
    };

    return Icon(icon, size: SobhSizes.iconSmall, color: color);
  }
}

class _Composer extends StatelessWidget {
  const _Composer({
    required this.controller,
    required this.onSend,
    required this.hint,
  });

  final TextEditingController controller;
  final Future<void> Function() onSend;
  final String hint;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return SafeArea(
      child: Container(
        color: palette.surface,
        padding: const EdgeInsets.all(SobhSpacing.sm),
        child: Row(
          children: <Widget>[
            IconButton(
              onPressed: () {},
              icon: const Icon(Icons.attach_file),
              tooltip: MaterialLocalizations.of(context).moreButtonTooltip,
            ),
            Expanded(
              child: TextField(
                controller: controller,
                minLines: 1,
                maxLines: 5,
                textInputAction: TextInputAction.newline,
                decoration: InputDecoration(hintText: hint),
              ),
            ),
            const SizedBox(width: SobhSpacing.sm),
            IconButton.filled(
              onPressed: onSend,
              icon: const Icon(Icons.send),
              tooltip: hint,
            ),
          ],
        ),
      ),
    );
  }
}
