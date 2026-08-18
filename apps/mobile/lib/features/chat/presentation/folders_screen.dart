import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/folders_repository.dart';

/// Managing chat folders (§12).
///
/// A folder is a saved filter, so this screen edits rules rather than moving
/// conversations: nothing here takes a chat out of the main list, and deleting
/// a folder leaves every chat exactly where it was.
class FoldersScreen extends ConsumerWidget {
  const FoldersScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ChatFolder>> folders = ref.watch(chatFoldersProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.foldersTitle)),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => _edit(context, ref, null),
        icon: const Icon(Icons.create_new_folder_outlined),
        label: Text(l10n.foldersNew),
      ),
      body: folders.when(
        loading: () => const Center(child: CircularProgressIndicator()),
        error: (Object error, StackTrace stack) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(chatFoldersProvider),
        ),
        data: (List<ChatFolder> rows) {
          if (rows.isEmpty) {
            return Padding(
              padding: const EdgeInsets.all(SobhSpacing.xl),
              child: Column(
                mainAxisAlignment: MainAxisAlignment.center,
                children: <Widget>[
                  Text(
                    l10n.foldersEmpty,
                    style: Theme.of(context).textTheme.titleMedium,
                    textAlign: TextAlign.center,
                  ),
                  const SizedBox(height: SobhSpacing.sm),
                  Text(
                    l10n.foldersEmptyBody,
                    style: Theme.of(context).textTheme.bodyMedium,
                    textAlign: TextAlign.center,
                  ),
                ],
              ),
            );
          }

          return ReorderableListView.builder(
            itemCount: rows.length,
            padding: const EdgeInsets.only(bottom: SobhSpacing.xxxl * 2),
            onReorder: (int from, int to) async {
              final List<ChatFolder> reordered = <ChatFolder>[...rows];
              final ChatFolder moved = reordered.removeAt(from);
              reordered.insert(to > from ? to - 1 : to, moved);
              await ref.read(foldersRepositoryProvider).reorder(
                <String>[for (final ChatFolder folder in reordered) folder.id],
              );
              ref.invalidate(chatFoldersProvider);
            },
            itemBuilder: (BuildContext context, int index) {
              final ChatFolder folder = rows[index];
              return ListTile(
                key: ValueKey<String>(folder.id),
                leading: Text(
                  folder.emoji.isEmpty ? '📁' : folder.emoji,
                  style: const TextStyle(fontSize: SobhSizes.iconMedium),
                ),
                title: Text(folder.title),
                subtitle: Text(l10n.foldersChatCount(folder.chatCount)),
                trailing: IconButton(
                  icon: const Icon(Icons.delete_outline),
                  tooltip: l10n.commonDelete,
                  onPressed: () => _delete(context, ref, folder),
                ),
                onTap: () => _edit(context, ref, folder),
              );
            },
          );
        },
      ),
    );
  }

  Future<void> _delete(
    BuildContext context,
    WidgetRef ref,
    ChatFolder folder,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final bool? confirmed = await showDialog<bool>(
      context: context,
      builder: (BuildContext context) => AlertDialog(
        title: Text(folder.title),
        content: Text(l10n.foldersDeleteConfirm),
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

    try {
      await ref.read(foldersRepositoryProvider).delete(folder.id);
      ref.invalidate(chatFoldersProvider);
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }

  Future<void> _edit(
    BuildContext context,
    WidgetRef ref,
    ChatFolder? folder,
  ) async {
    final bool? saved = await showModalBottomSheet<bool>(
      context: context,
      isScrollControlled: true,
      useSafeArea: true,
      builder: (BuildContext context) => _FolderEditor(folder: folder),
    );
    if (saved == true) {
      ref.invalidate(chatFoldersProvider);
    }
  }
}

/// The editor for one folder.
///
/// The whole folder is sent on save because that is how it is edited: one
/// small object on one screen. A partial update of a filter is harder to
/// reason about than replacing it.
class _FolderEditor extends ConsumerStatefulWidget {
  const _FolderEditor({this.folder});

  final ChatFolder? folder;

  @override
  ConsumerState<_FolderEditor> createState() => _FolderEditorState();
}

class _FolderEditorState extends ConsumerState<_FolderEditor> {
  late final TextEditingController _title =
      TextEditingController(text: widget.folder?.title ?? '');
  late ChatFolder _draft =
      widget.folder ?? const ChatFolder(id: '', title: '', includeGroups: true);
  bool _saving = false;

  @override
  void dispose() {
    _title.dispose();
    super.dispose();
  }

  Future<void> _save() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _saving = true);

    try {
      final ChatFolder folder = _draft.copyWith(title: _title.text.trim());
      final FoldersRepository repository = ref.read(foldersRepositoryProvider);
      if (widget.folder == null) {
        await repository.create(folder);
      } else {
        await repository.update(folder);
      }
      if (mounted) {
        Navigator.of(context).pop(true);
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(() => _saving = false);
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(
              error.isOffline ? l10n.errorNetwork : error.message,
            ),
          ),
        );
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Padding(
      padding: EdgeInsets.only(
        left: SobhSpacing.lg,
        right: SobhSpacing.lg,
        top: SobhSpacing.lg,
        bottom: MediaQuery.of(context).viewInsets.bottom + SobhSpacing.lg,
      ),
      child: SingleChildScrollView(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: <Widget>[
            TextField(
              controller: _title,
              decoration: InputDecoration(labelText: l10n.foldersName),
              textInputAction: TextInputAction.done,
            ),
            const SizedBox(height: SobhSpacing.md),
            _Toggle(
              label: l10n.foldersIncludeContacts,
              value: _draft.includeContacts,
              onChanged: (bool value) => setState(
                () => _draft = _draft.copyWith(includeContacts: value),
              ),
            ),
            _Toggle(
              label: l10n.foldersIncludeNonContacts,
              value: _draft.includeNonContacts,
              onChanged: (bool value) => setState(
                () => _draft = _draft.copyWith(includeNonContacts: value),
              ),
            ),
            _Toggle(
              label: l10n.foldersIncludeGroups,
              value: _draft.includeGroups,
              onChanged: (bool value) => setState(
                () => _draft = _draft.copyWith(includeGroups: value),
              ),
            ),
            _Toggle(
              label: l10n.foldersIncludeChannels,
              value: _draft.includeChannels,
              onChanged: (bool value) => setState(
                () => _draft = _draft.copyWith(includeChannels: value),
              ),
            ),
            _Toggle(
              label: l10n.foldersIncludeBots,
              value: _draft.includeBots,
              onChanged: (bool value) =>
                  setState(() => _draft = _draft.copyWith(includeBots: value)),
            ),
            const Divider(),
            _Toggle(
              label: l10n.foldersExcludeMuted,
              value: _draft.excludeMuted,
              onChanged: (bool value) =>
                  setState(() => _draft = _draft.copyWith(excludeMuted: value)),
            ),
            _Toggle(
              label: l10n.foldersExcludeRead,
              value: _draft.excludeRead,
              onChanged: (bool value) =>
                  setState(() => _draft = _draft.copyWith(excludeRead: value)),
            ),
            _Toggle(
              label: l10n.foldersExcludeArchived,
              value: _draft.excludeArchived,
              onChanged: (bool value) => setState(
                () => _draft = _draft.copyWith(excludeArchived: value),
              ),
            ),
            const SizedBox(height: SobhSpacing.lg),
            FilledButton(
              onPressed: _saving || _title.text.trim().isEmpty ? null : _save,
              child: Text(l10n.commonSave),
            ),
          ],
        ),
      ),
    );
  }
}

class _Toggle extends StatelessWidget {
  const _Toggle({
    required this.label,
    required this.value,
    required this.onChanged,
  });

  final String label;
  final bool value;
  final ValueChanged<bool> onChanged;

  @override
  Widget build(BuildContext context) => SwitchListTile.adaptive(
        contentPadding: EdgeInsets.zero,
        title: Text(label),
        value: value,
        onChanged: onChanged,
      );
}
