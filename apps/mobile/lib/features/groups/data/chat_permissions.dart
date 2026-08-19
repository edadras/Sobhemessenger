import 'package:flutter/widgets.dart';

import '../../../core/localization/generated/app_localizations.dart';

/// The permission vocabulary, and which kind of chat each key applies to.
///
/// It mirrors `messaging.PermissionKeys` on the server, which is the authority:
/// a key the server does not know is refused rather than quietly stored, so
/// there is no such thing as a permission this list can invent. Keeping the
/// applicability here as well means the editor never *offers* a key that would
/// be refused for this kind of chat — the refusal would be correct and the
/// screen would still have wasted the person's time.
enum ChatPermission {
  sendMessages('send_messages', _Applies.both),
  sendMedia('send_media', _Applies.both),
  sendFiles('send_files', _Applies.both),
  sendPolls('send_polls', _Applies.both),
  sendStickers('send_stickers', _Applies.group),
  embedLinks('embed_links', _Applies.both),
  addMembers('add_members', _Applies.both),
  removeMembers('remove_members', _Applies.both),
  banMembers('ban_members', _Applies.both),
  pinMessages('pin_messages', _Applies.both),
  editGroup('edit_group', _Applies.both),
  deleteMessages('delete_messages', _Applies.both),
  manageAdmins('manage_admins', _Applies.both),
  manageCalls('manage_calls', _Applies.group),
  manageInvites('manage_invites', _Applies.both),
  postStories('post_stories', _Applies.channel);

  const ChatPermission(this.key, this._applies);

  /// The wire key. This is what travels; the enum is only how the app spells
  /// it.
  final String key;
  final _Applies _applies;

  bool appliesTo(String chatType) => switch (_applies) {
        _Applies.both => true,
        _Applies.group => chatType == 'group',
        _Applies.channel => chatType == 'channel',
      };

  static List<ChatPermission> forChatType(String chatType) =>
      ChatPermission.values
          .where((ChatPermission p) => p.appliesTo(chatType))
          .toList(growable: false);

  String label(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    return switch (this) {
      ChatPermission.sendMessages => l10n.permissionSendMessages,
      ChatPermission.sendMedia => l10n.permissionSendMedia,
      ChatPermission.sendFiles => l10n.permissionSendFiles,
      ChatPermission.sendPolls => l10n.permissionSendPolls,
      ChatPermission.sendStickers => l10n.permissionSendStickers,
      ChatPermission.embedLinks => l10n.permissionEmbedLinks,
      ChatPermission.addMembers => l10n.permissionAddMembers,
      ChatPermission.removeMembers => l10n.permissionRemoveMembers,
      ChatPermission.banMembers => l10n.permissionBanMembers,
      ChatPermission.pinMessages => l10n.permissionPinMessages,
      ChatPermission.editGroup => l10n.permissionEditGroup,
      ChatPermission.deleteMessages => l10n.permissionDeleteMessages,
      ChatPermission.manageAdmins => l10n.permissionManageAdmins,
      ChatPermission.manageCalls => l10n.permissionManageCalls,
      ChatPermission.manageInvites => l10n.permissionManageInvites,
      ChatPermission.postStories => l10n.permissionPostStories,
    };
  }
}

enum _Applies { both, group, channel }
