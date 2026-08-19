import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/chat_permissions.dart';
import '../data/groups_repository.dart';

/// Named permission bundles for a group or channel (§14).
///
/// A bundle sits between what everyone in the chat may do and what one member
/// may do: "moderator" is defined once and given to people, rather than the
/// same six switches being set again for each of them — and changing what a
/// moderator is changes it for everyone who is one.
///
/// The server refuses a bundle that grants something the person defining it
/// does not itself hold, so this screen offers every key and lets that refusal
/// stand rather than guessing at the caller's own permissions.
class RoleBundlesScreen extends ConsumerWidget {
  const RoleBundlesScreen({
    super.key,
    required this.chatId,
    required this.chatType,
  });

  final String chatId;
  final String chatType;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<GroupRole>> roles =
        ref.watch(chatRolesProvider(chatId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.rolesTitle)),
      floatingActionButton: FloatingActionButton(
        onPressed: () => _edit(context, ref, null),
        child: const Icon(Icons.add),
      ),
      body: roles.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(chatRolesProvider(chatId)),
        ),
        data: (List<GroupRole> rows) {
          if (rows.isEmpty) {
            return SobhEmptyState(
              icon: Icons.badge_outlined,
              title: l10n.rolesEmptyTitle,
              body: l10n.rolesEmptyBody,
            );
          }
          return ListView.separated(
            itemCount: rows.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) {
              final GroupRole role = rows[index];
              final int granted = role.permissions.values
                  .where((bool allowed) => allowed)
                  .length;
              return ListTile(
                leading: const Icon(Icons.badge_outlined),
                title: Text(role.name),
                subtitle: Text(
                  '${l10n.rolesGrantedCount(granted)} · '
                  '${l10n.rolesHolderCount(role.memberCount)}',
                ),
                onTap: () => _edit(context, ref, role),
                trailing: IconButton(
                  icon: const Icon(Icons.delete_outline),
                  onPressed: () => _delete(context, ref, role),
                ),
              );
            },
          );
        },
      ),
    );
  }

  Future<void> _edit(
    BuildContext context,
    WidgetRef ref,
    GroupRole? existing,
  ) async {
    final _RoleDraft? draft = await showModalBottomSheet<_RoleDraft>(
      context: context,
      isScrollControlled: true,
      builder: (BuildContext context) => _RoleEditor(
        chatType: chatType,
        existing: existing,
      ),
    );
    if (draft == null || !context.mounted) {
      return;
    }

    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    try {
      final GroupsRepository repository = ref.read(groupsRepositoryProvider);
      if (existing == null) {
        await repository.createRole(
          chatId,
          name: draft.name,
          permissions: draft.permissions,
          rank: draft.rank,
        );
      } else {
        await repository.updateRole(
          chatId,
          existing.id,
          name: draft.name,
          permissions: draft.permissions,
          rank: draft.rank,
        );
      }
      ref.invalidate(chatRolesProvider(chatId));
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }

  Future<void> _delete(
    BuildContext context,
    WidgetRef ref,
    GroupRole role,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.rolesDeleteTitle),
            // Deleting a bundle is not a small change when people hold it:
            // everyone who did falls back to the chat's ordinary defaults.
            content: Text(l10n.rolesDeleteBody(role.name, role.memberCount)),
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
        ) ??
        false;
    if (!confirmed || !context.mounted) {
      return;
    }

    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    try {
      await ref.read(groupsRepositoryProvider).deleteRole(chatId, role.id);
      ref.invalidate(chatRolesProvider(chatId));
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

class _RoleDraft {
  const _RoleDraft({
    required this.name,
    required this.permissions,
    required this.rank,
  });

  final String name;
  final Map<String, bool> permissions;
  final int rank;
}

class _RoleEditor extends StatefulWidget {
  const _RoleEditor({required this.chatType, this.existing});

  final String chatType;
  final GroupRole? existing;

  @override
  State<_RoleEditor> createState() => _RoleEditorState();
}

class _RoleEditorState extends State<_RoleEditor> {
  late final TextEditingController _name =
      TextEditingController(text: widget.existing?.name ?? '');
  late final Map<String, bool> _permissions = <String, bool>{
    for (final ChatPermission permission
        in ChatPermission.forChatType(widget.chatType))
      permission.key: widget.existing?.permissions[permission.key] ?? false,
  };
  late int _rank = widget.existing?.rank ?? 0;

  @override
  void dispose() {
    _name.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return SafeArea(
      child: Padding(
        padding: EdgeInsets.only(
          bottom: MediaQuery.of(context).viewInsets.bottom,
        ),
        child: ListView(
          shrinkWrap: true,
          padding: const EdgeInsets.all(16),
          children: <Widget>[
            Text(
              widget.existing == null ? l10n.rolesCreate : l10n.rolesEdit,
              style: Theme.of(context).textTheme.titleMedium,
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _name,
              decoration: InputDecoration(labelText: l10n.rolesName),
              onChanged: (_) => setState(() {}),
            ),
            const SizedBox(height: 12),
            Text(
              l10n.rolesRank,
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            ),
            Slider(
              value: _rank.toDouble(),
              max: 100,
              divisions: 100,
              label: '$_rank',
              onChanged: (double value) =>
                  setState(() => _rank = value.round()),
            ),
            const Divider(),
            for (final ChatPermission permission
                in ChatPermission.forChatType(widget.chatType))
              SwitchListTile(
                value: _permissions[permission.key] ?? false,
                onChanged: (bool value) =>
                    setState(() => _permissions[permission.key] = value),
                title: Text(permission.label(context)),
              ),
            const SizedBox(height: 12),
            FilledButton(
              onPressed: _name.text.trim().isEmpty
                  ? null
                  : () => Navigator.of(context).pop(
                        _RoleDraft(
                          name: _name.text.trim(),
                          permissions: _permissions,
                          rank: _rank,
                        ),
                      ),
              child: Text(l10n.commonSave),
            ),
          ],
        ),
      ),
    );
  }
}
