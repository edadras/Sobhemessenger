import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../data/chat_repository.dart';
import '../data/organise_repository.dart';

/// What a long press on a message offers (§12).
Future<void> showMessageActions(
  BuildContext context,
  WidgetRef ref,
  MessageRow message,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final SobhPalette palette = SobhTheme.of(context);

  // A message still in the outbox has no server id, so nothing that names one
  // can work on it yet.
  final bool isSent = message.seq != null;

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
