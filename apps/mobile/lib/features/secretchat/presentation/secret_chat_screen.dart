import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/secret_chat_controller.dart';
import 'safety_number_screen.dart';

/// An end-to-end encrypted conversation (§24).
///
/// It deliberately does not reuse the ordinary chat screen. That screen is
/// built on the local database — drafts, the outbox, read receipts, editing,
/// forwarding — and every one of those features would want to write this
/// conversation's plaintext to disk. Keeping the two apart is what makes the
/// guarantee checkable by reading the code rather than by trusting it.
class SecretChatScreen extends ConsumerStatefulWidget {
  const SecretChatScreen({
    super.key,
    required this.chatId,
    required this.peerUserId,
    required this.peerName,
  });

  final String chatId;
  final String peerUserId;
  final String peerName;

  @override
  ConsumerState<SecretChatScreen> createState() => _SecretChatScreenState();
}

class _SecretChatScreenState extends ConsumerState<SecretChatScreen> {
  final TextEditingController _composer = TextEditingController();
  final ScrollController _scroll = ScrollController();

  @override
  void dispose() {
    _composer.dispose();
    _scroll.dispose();
    super.dispose();
  }

  SecretChatArgs get _args => SecretChatArgs(
        chatId: widget.chatId,
        peerUserId: widget.peerUserId,
      );

  Future<void> _send() async {
    final String text = _composer.text;
    if (text.trim().isEmpty) {
      return;
    }
    _composer.clear();
    await ref.read(secretChatControllerProvider(_args).notifier).send(text);
    _scrollToEnd();
  }

  void _scrollToEnd() {
    if (!_scroll.hasClients) {
      return;
    }
    _scroll.animateTo(
      _scroll.position.maxScrollExtent,
      duration: SobhDuration.fast,
      curve: Curves.easeOut,
    );
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final SecretConversation conversation =
        ref.watch(secretChatControllerProvider(_args));

    return Scaffold(
      appBar: AppBar(
        title: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            Text(widget.peerName, maxLines: 1, overflow: TextOverflow.ellipsis),
            Text(
              l10n.secretChatTitle,
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            ),
          ],
        ),
        actions: <Widget>[
          IconButton(
            tooltip: l10n.secretChatVerify,
            icon: const Icon(Icons.verified_user_outlined),
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => SafetyNumberScreen(
                  peerUserId: widget.peerUserId,
                  peerName: widget.peerName,
                ),
              ),
            ),
          ),
        ],
      ),
      body: Column(
        children: <Widget>[
          _EphemeralNotice(text: l10n.secretChatEphemeralNotice),
          Expanded(child: _body(conversation, l10n)),
          _Composer(
            controller: _composer,
            hint: l10n.secretChatComposeHint,
            onSend: _send,
          ),
        ],
      ),
    );
  }

  Widget _body(SecretConversation conversation, AppLocalizations l10n) {
    if (conversation.isLoading && conversation.messages.isEmpty) {
      return const SobhLoading();
    }
    // An error with messages already on screen is shown as a bar rather than
    // replacing the conversation: losing sight of what was said because a poll
    // failed would be worse than the failure.
    if (conversation.error != null && conversation.messages.isEmpty) {
      return SobhErrorState(
        error: conversation.error!,
        onRetry: () =>
            ref.read(secretChatControllerProvider(_args).notifier).refresh(),
      );
    }
    if (conversation.messages.isEmpty) {
      return SobhEmptyState(
        icon: Icons.lock_outline,
        title: l10n.secretChatEmptyTitle,
        body: l10n.secretChatEmptyBody,
      );
    }

    return RefreshIndicator(
      onRefresh: () =>
          ref.read(secretChatControllerProvider(_args).notifier).refresh(),
      child: ListView.builder(
        controller: _scroll,
        padding: const EdgeInsets.symmetric(
          horizontal: SobhSpacing.lg,
          vertical: SobhSpacing.sm,
        ),
        itemCount: conversation.messages.length,
        itemBuilder: (BuildContext context, int index) => _Bubble(
          message: conversation.messages[index],
          onRetry: () => ref
              .read(secretChatControllerProvider(_args).notifier)
              .retry(conversation.messages[index]),
        ),
      ),
    );
  }
}

/// States plainly that history is not kept. A guarantee the user does not know
/// about cannot inform what they choose to say.
class _EphemeralNotice extends StatelessWidget {
  const _EphemeralNotice({required this.text});

  final String text;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return Container(
      width: double.infinity,
      color: palette.surfaceVariant,
      padding: const EdgeInsets.symmetric(
        horizontal: SobhSpacing.lg,
        vertical: SobhSpacing.sm,
      ),
      child: Row(
        children: <Widget>[
          Icon(
            Icons.lock_clock_outlined,
            size: SobhSizes.iconSmall,
            color: palette.textSecondary,
          ),
          const SizedBox(width: SobhSpacing.sm),
          Expanded(
            child: Text(
              text,
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            ),
          ),
        ],
      ),
    );
  }
}

class _Bubble extends StatelessWidget {
  const _Bubble({required this.message, required this.onRetry});

  final SecretMessage message;
  final VoidCallback onRetry;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final ThemeData theme = Theme.of(context);

    return Align(
      alignment: message.isMine
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
            color: message.isMine
                ? palette.bubbleOutgoing
                : palette.bubbleIncoming,
            borderRadius: BorderRadius.circular(SobhRadius.lg),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            mainAxisSize: MainAxisSize.min,
            children: <Widget>[
              Text(
                message.text,
                style: theme.textTheme.bodyMedium?.copyWith(
                  color: message.isMine
                      ? palette.bubbleOutgoingText
                      : palette.bubbleIncomingText,
                ),
              ),
              const SizedBox(height: SobhSpacing.xxs),
              if (message.failed)
                // A failed send offers the only useful action rather than a
                // silent red mark.
                TextButton.icon(
                  onPressed: onRetry,
                  icon: const Icon(Icons.refresh, size: SobhSizes.iconSmall),
                  label: Text(
                    '${l10n.secretChatSendFailed} · ${l10n.commonRetry}',
                  ),
                )
              else
                Row(
                  mainAxisSize: MainAxisSize.min,
                  children: <Widget>[
                    Text(
                      TimeOfDay.fromDateTime(message.at).format(context),
                      style: theme.textTheme.bodySmall
                          ?.copyWith(color: palette.textSecondary),
                    ),
                    if (message.isSending) ...<Widget>[
                      const SizedBox(width: SobhSpacing.xs),
                      SizedBox(
                        width: SobhSizes.iconSmall,
                        height: SobhSizes.iconSmall,
                        child: CircularProgressIndicator(
                          strokeWidth: 1.5,
                          color: palette.textSecondary,
                        ),
                      ),
                    ],
                  ],
                ),
            ],
          ),
        ),
      ),
    );
  }
}

class _Composer extends StatelessWidget {
  const _Composer({
    required this.controller,
    required this.hint,
    required this.onSend,
  });

  final TextEditingController controller;
  final String hint;
  final VoidCallback onSend;

  @override
  Widget build(BuildContext context) {
    return SafeArea(
      top: false,
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.sm),
        child: Row(
          children: <Widget>[
            Expanded(
              child: TextField(
                controller: controller,
                minLines: 1,
                maxLines: 5,
                textInputAction: TextInputAction.send,
                onSubmitted: (_) => onSend(),
                decoration: InputDecoration(hintText: hint),
              ),
            ),
            const SizedBox(width: SobhSpacing.sm),
            IconButton.filled(
              onPressed: onSend,
              icon: const Icon(Icons.lock_outline),
              tooltip: hint,
            ),
          ],
        ),
      ),
    );
  }
}
