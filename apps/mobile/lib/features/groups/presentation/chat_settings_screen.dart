import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/comments_repository.dart';
import '../data/groups_repository.dart';

/// The administrative settings on one group or channel (§14, §15).
///
/// Only the settings that belong to this chat's type are shown: the server
/// reports a channel's signature switch and a group's sticker set, and reports
/// nothing at all for the one that does not apply. Rendering a control the
/// chat has no setting for would invite people to change something that is
/// not there.
class ChatSettingsScreen extends ConsumerWidget {
  const ChatSettingsScreen({required this.chatId, super.key});

  final String chatId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<ChatSettings> settings =
        ref.watch(chatSettingsProvider(chatId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.groupsInfoTitle)),
      body: settings.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(chatSettingsProvider(chatId)),
        ),
        data: (ChatSettings current) => ListView(
          children: <Widget>[
            if (current.signatureEnabled != null)
              SwitchListTile(
                title: Text(l10n.channelSignatures),
                value: current.signatureEnabled!,
                onChanged: (bool value) => _save(
                  context,
                  ref,
                  current.copyWith(signatureEnabled: value),
                ),
              ),
            if (current.commentsEnabled != null)
              SwitchListTile(
                title: Text(l10n.channelComments),
                value: current.commentsEnabled!,
                onChanged: (bool value) => _save(
                  context,
                  ref,
                  current.copyWith(commentsEnabled: value),
                ),
              ),
            if (current.discussionChatId != null)
              ListTile(
                leading: const Icon(Icons.forum_outlined),
                title: Text(l10n.channelDiscussionGroup),
                subtitle: Text(current.discussionChatId!),
                trailing: TextButton(
                  onPressed: () => _unlink(context, ref),
                  child: Text(l10n.channelDiscussionUnlink),
                ),
              ),
            if (current.isBroadcast != null)
              SwitchListTile(
                title: Text(l10n.groupBroadcast),
                value: current.isBroadcast!,
                onChanged: (bool value) => _save(
                  context,
                  ref,
                  current.copyWith(isBroadcast: value),
                ),
              ),
            if (current.stickerSet != null)
              ListTile(
                leading: const Icon(Icons.emoji_emotions_outlined),
                title: Text(l10n.groupStickerSet),
                subtitle: Text(
                  current.stickerSet!.isEmpty ? '—' : current.stickerSet!,
                ),
                onTap: () => _editStickerSet(context, ref, current),
              ),
          ],
        ),
      ),
    );
  }

  Future<void> _save(
    BuildContext context,
    WidgetRef ref,
    ChatSettings settings,
  ) async {
    try {
      await ref.read(groupsRepositoryProvider).updateSettings(chatId, settings);
      ref.invalidate(chatSettingsProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }

  Future<void> _unlink(BuildContext context, WidgetRef ref) async {
    try {
      await ref.read(commentsRepositoryProvider).unlinkDiscussion(chatId);
      ref.invalidate(chatSettingsProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }

  Future<void> _editStickerSet(
    BuildContext context,
    WidgetRef ref,
    ChatSettings current,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TextEditingController controller =
        TextEditingController(text: current.stickerSet ?? '');

    final String? slug = await showDialog<String>(
      context: context,
      builder: (BuildContext context) => AlertDialog(
        title: Text(l10n.groupStickerSet),
        content: TextField(
          controller: controller,
          autofocus: true,
          decoration: const InputDecoration(hintText: 'sticker_set_slug'),
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
    if (slug == null || !context.mounted) {
      return;
    }
    // An empty string clears the set, which is why it is not treated as a
    // cancelled edit.
    await _save(context, ref, current.copyWith(stickerSet: slug));
  }
}

/// The gap the settings list leaves when a chat has none of these settings.
const double kChatSettingsEmptyGap = SobhSpacing.lg;
