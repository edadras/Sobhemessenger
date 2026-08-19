import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../../groups/presentation/comments_sheet.dart';
import '../data/chat_repository.dart';
import '../data/organise_repository.dart';

/// What a long press on a message offers (§12).
///
/// [chatType] decides whether commenting is offered. Comments belong to a
/// channel post and nothing else, and a channel with no discussion group says
/// so when the sheet is opened rather than hiding the option — the reader
/// cannot tell the difference from the outside, and a silently missing action
/// reads as a broken app.
Future<void> showMessageActions(
  BuildContext context,
  WidgetRef ref,
  MessageRow message, {
  String chatType = '',
}) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final SobhPalette palette = SobhTheme.of(context);

  // A message still in the outbox has no server id, so nothing that names one
  // can work on it yet.
  final bool isSent = message.seq != null;
  // A null sender is this device's own optimistic copy, before the server has
  // told it whose the message is.
  final String? currentUserId = ref.read(sessionControllerProvider).userId;
  final bool isOwn =
      message.senderId == null || message.senderId == currentUserId;

  await showModalBottomSheet<void>(
    context: context,
    builder: (BuildContext sheet) => SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          if (message.content.isNotEmpty)
            ListTile(
              leading: const Icon(Icons.copy_outlined),
              title: Text(l10n.commonCopy),
              onTap: () async {
                await Clipboard.setData(
                  ClipboardData(text: message.content),
                );
                if (sheet.mounted) {
                  Navigator.of(sheet).pop();
                }
              },
            ),
          if (isSent)
            ListTile(
              leading: const Icon(Icons.forward_outlined),
              title: Text(l10n.commonForward),
              onTap: () {
                Navigator.of(sheet).pop();
                _forward(context, ref, message);
              },
            ),
          if (isSent)
            ListTile(
              leading: Icon(
                message.isPinned ? Icons.push_pin : Icons.push_pin_outlined,
              ),
              title: Text(
                message.isPinned ? l10n.chatUnpin : l10n.chatPin,
              ),
              onTap: () {
                Navigator.of(sheet).pop();
                _setPinned(context, ref, message, !message.isPinned);
              },
            ),
          if (isSent && chatType == 'channel')
            ListTile(
              leading: const Icon(Icons.mode_comment_outlined),
              title: Text(l10n.commentsTitle),
              onTap: () {
                Navigator.of(sheet).pop();
                CommentsSheet.show(
                  context,
                  channelId: message.chatId,
                  postId: message.id,
                );
              },
            ),
          // Who has read it, for the sender. The cursor in the chat list
          // answers "how far has each person read"; this is the other
          // question, and it was built with nothing to open it.
          if (isSent && isOwn)
            ListTile(
              leading: const Icon(Icons.done_all),
              title: Text(l10n.readsTitle),
              onTap: () {
                Navigator.of(sheet).pop();
                showModalBottomSheet<void>(
                  context: context,
                  isScrollControlled: true,
                  builder: (_) => MessageReadsSheet(messageId: message.id),
                );
              },
            ),
          if (isSent)
            ListTile(
              leading: const Icon(Icons.add_reaction_outlined),
              title: Text(l10n.chatReact),
              onTap: () {
                Navigator.of(sheet).pop();
                _react(context, ref, message);
              },
            ),
          // Editing and deleting are offered only on the user's own messages.
          // A moderator may delete anyone's, but the server decides that — so
          // rather than duplicating the permission model here and getting it
          // subtly wrong, the action is hidden for the common case and any
          // refusal comes back from the server with its own reason.
          if (isSent && isOwn && message.content.isNotEmpty)
            ListTile(
              leading: const Icon(Icons.edit_outlined),
              title: Text(l10n.chatEditMessage),
              onTap: () {
                Navigator.of(sheet).pop();
                _edit(context, ref, message);
              },
            ),
          if (isSent && isOwn)
            ListTile(
              leading: Icon(Icons.delete_outline, color: palette.error),
              title: Text(
                l10n.chatDeleteMessage,
                style: TextStyle(color: palette.error),
              ),
              onTap: () {
                Navigator.of(sheet).pop();
                _delete(context, ref, message);
              },
            ),
          if (!isSent)
            Padding(
              padding: const EdgeInsets.all(SobhSpacing.lg),
              child: Text(
                l10n.chatMessageNotSentYet,
                style: TextStyle(color: palette.textSecondary),
              ),
            ),
        ],
      ),
    ),
  );
}

Future<void> _setPinned(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
  bool pinned,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  try {
    await ref.read(organiseRepositoryProvider).setPinned(message.id, pinned);
    ref.invalidate(pinnedMessagesProvider(message.chatId));
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

/// Emoji offered for a quick reaction.
///
/// A fixed row rather than a full picker: it is one tap for the reactions
/// people actually use, and a keyboard for the rest is a separate piece of work
/// that should not hold up having any reactions at all.
const List<String> quickReactions = <String>[
  '👍',
  '❤️',
  '😂',
  '😮',
  '😢',
  '🙏',
];

Future<void> _react(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
) async {
  final String? emoji = await showModalBottomSheet<String>(
    context: context,
    builder: (BuildContext sheet) => SafeArea(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        child: Wrap(
          alignment: WrapAlignment.spaceEvenly,
          children: <Widget>[
            for (final String emoji in quickReactions)
              IconButton(
                // A real tap target rather than a bare glyph, which on a phone
                // is the difference between reacting and missing.
                constraints: const BoxConstraints(
                  minWidth: SobhSizes.minTapTarget,
                  minHeight: SobhSizes.minTapTarget,
                ),
                onPressed: () => Navigator.of(sheet).pop(emoji),
                icon: Text(emoji, style: const TextStyle(fontSize: 26)),
              ),
          ],
        ),
      ),
    ),
  );
  if (emoji == null || !context.mounted) {
    return;
  }

  final AppLocalizations l10n = AppLocalizations.of(context);
  try {
    await ref.read(chatRepositoryProvider).react(message.id, emoji);
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

Future<void> _edit(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final TextEditingController controller =
      TextEditingController(text: message.content);

  final String? edited = await showDialog<String>(
    context: context,
    builder: (BuildContext dialog) => AlertDialog(
      title: Text(l10n.chatEditMessage),
      content: TextField(
        controller: controller,
        autofocus: true,
        minLines: 1,
        maxLines: 6,
      ),
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(dialog).pop(),
          child: Text(l10n.commonCancel),
        ),
        FilledButton(
          onPressed: () => Navigator.of(dialog).pop(controller.text),
          child: Text(l10n.commonSave),
        ),
      ],
    ),
  );
  controller.dispose();

  final String? trimmed = edited?.trim();
  // Nothing to do for a cancel, an unchanged message, or an empty one —
  // emptying a message is a delete, and should be asked for as one.
  if (trimmed == null ||
      trimmed.isEmpty ||
      trimmed == message.content ||
      !context.mounted) {
    return;
  }

  try {
    await ref.read(chatRepositoryProvider).editMessage(message.id, trimmed);
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          // The server owns the edit window, so "too old to edit" arrives here
          // with its own wording rather than being guessed at locally.
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

Future<void> _delete(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);

  // Confirmed because it cannot be undone and it removes the message for
  // everyone, not only on this device.
  final bool confirmed = await showDialog<bool>(
        context: context,
        builder: (BuildContext dialog) => AlertDialog(
          title: Text(l10n.chatDeleteMessage),
          content: Text(l10n.chatDeleteConfirm),
          actions: <Widget>[
            TextButton(
              onPressed: () => Navigator.of(dialog).pop(false),
              child: Text(l10n.commonCancel),
            ),
            FilledButton(
              onPressed: () => Navigator.of(dialog).pop(true),
              child: Text(l10n.commonDelete),
            ),
          ],
        ),
      ) ??
      false;
  if (!confirmed || !context.mounted) {
    return;
  }

  try {
    await ref.read(chatRepositoryProvider).deleteMessage(message.id);
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

/// Picks a destination chat and forwards into it.
Future<void> _forward(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);

  final ChatRow? target = await showModalBottomSheet<ChatRow>(
    context: context,
    isScrollControlled: true,
    builder: (BuildContext sheet) => const _ForwardTargetPicker(),
  );
  if (target == null || !context.mounted) {
    return;
  }

  try {
    await ref.read(organiseRepositoryProvider).forward(
      fromChatId: message.chatId,
      toChatId: target.id,
      messageIds: <String>[message.id],
    );
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(content: Text(l10n.chatForwarded(target.title))),
      );
    }
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context).showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

class _ForwardTargetPicker extends ConsumerWidget {
  const _ForwardTargetPicker();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ChatRow>> chats = ref.watch(chatListProvider);

    return DraggableScrollableSheet(
      initialChildSize: 0.6,
      expand: false,
      builder: (BuildContext context, ScrollController controller) => Column(
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child: Text(
              l10n.chatForwardTo,
              style: Theme.of(context).textTheme.titleMedium,
            ),
          ),
          const Divider(height: 1),
          Expanded(
            child: chats.when(
              loading: () => const Center(child: CircularProgressIndicator()),
              error: (Object _, StackTrace __) =>
                  Center(child: Text(l10n.errorGeneric)),
              data: (List<ChatRow> rows) => ListView.builder(
                controller: controller,
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) => ListTile(
                  title: Text(rows[index].title),
                  onTap: () => Navigator.of(context).pop(rows[index]),
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}

/// Who has read one message (§7).
///
/// The chat list keeps a cursor per person — how far each has read — which is
/// all a list of conversations needs. This is the question a cursor cannot
/// answer, asked only when somebody opens the detail for one message, which is
/// why it is a request rather than something synced for every message on
/// screen.
class MessageReadsSheet extends ConsumerWidget {
  const MessageReadsSheet({super.key, required this.messageId});

  final String messageId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<MessageRead>> reads =
        ref.watch(messageReadsProvider(messageId));

    return SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child: Text(
              l10n.readsTitle,
              style: Theme.of(context).textTheme.titleMedium,
            ),
          ),
          reads.when(
            loading: () => const Padding(
              padding: EdgeInsets.all(SobhSpacing.xl),
              child: CircularProgressIndicator(),
            ),
            error: (Object error, StackTrace _) => Padding(
              padding: const EdgeInsets.all(SobhSpacing.xl),
              child: Text(l10n.errorGeneric),
            ),
            data: (List<MessageRead> rows) {
              if (rows.isEmpty) {
                return Padding(
                  padding: const EdgeInsets.all(SobhSpacing.xl),
                  child: Text(
                    l10n.readsNobody,
                    style: TextStyle(color: palette.textSecondary),
                  ),
                );
              }
              return Flexible(
                child: ListView.builder(
                  shrinkWrap: true,
                  itemCount: rows.length,
                  itemBuilder: (BuildContext context, int index) {
                    final MessageRead read = rows[index];
                    return ListTile(
                      leading: SobhAvatar(name: read.displayName),
                      title: Text(read.displayName),
                      trailing: Text(
                        DateFormat.Hm().format(read.readAt),
                        style: TextStyle(color: palette.textSecondary),
                      ),
                    );
                  },
                ),
              );
            },
          ),
        ],
      ),
    );
  }
}
