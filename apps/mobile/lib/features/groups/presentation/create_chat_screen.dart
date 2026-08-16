import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../contacts/data/contacts_repository.dart';
import '../data/groups_repository.dart';

/// Creates a group or a channel (§14, §15).
///
/// Both use one screen because the fields are the same; only the wording and
/// whether members are picked up front differ.
class CreateChatScreen extends ConsumerStatefulWidget {
  const CreateChatScreen({super.key, required this.kind});

  final ChatKind kind;

  @override
  ConsumerState<CreateChatScreen> createState() => _CreateChatScreenState();
}

class _CreateChatScreenState extends ConsumerState<CreateChatScreen> {
  final TextEditingController _title = TextEditingController();
  final TextEditingController _description = TextEditingController();
  final TextEditingController _username = TextEditingController();
  final Set<String> _selected = <String>{};

  bool _isPublic = false;
  bool _submitting = false;
  String? _error;

  @override
  void dispose() {
    _title.dispose();
    _description.dispose();
    _username.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() {
      _submitting = true;
      _error = null;
    });

    try {
      final String chatId = await ref.read(groupsRepositoryProvider).create(
            kind: widget.kind,
            title: _title.text.trim(),
            description: _description.text.trim(),
            isPublic: _isPublic,
            username: _isPublic ? _username.text.trim() : null,
            memberIds: _selected.toList(),
          );
      if (mounted) {
        context.go('/chats/$chatId');
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(
          () => _error = error.isOffline ? l10n.errorNetwork : error.message,
        );
      }
    } finally {
      if (mounted) {
        setState(() => _submitting = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<Contact>> contacts = ref.watch(contactListProvider);
    final bool canSubmit = _title.text.trim().isNotEmpty && !_submitting;

    return Scaffold(
      appBar: AppBar(
        title: Text(
          widget.kind == ChatKind.channel
              ? l10n.channelsCreateTitle
              : l10n.groupsCreateTitle,
        ),
        actions: <Widget>[
          TextButton(
            onPressed: canSubmit ? _submit : null,
            child: Text(l10n.commonDone),
          ),
        ],
      ),
      body: ListView(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        children: <Widget>[
          TextField(
            controller: _title,
            decoration: InputDecoration(labelText: l10n.groupsNameHint),
            textInputAction: TextInputAction.next,
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: SobhSpacing.lg),
          TextField(
            controller: _description,
            decoration: InputDecoration(labelText: l10n.groupsDescriptionHint),
            maxLines: 3,
          ),
          const SizedBox(height: SobhSpacing.lg),
          SwitchListTile(
            value: _isPublic,
            onChanged: (bool value) => setState(() => _isPublic = value),
            title: Text(l10n.groupsPublicLabel),
            subtitle: Text(l10n.groupsPublicHint),
            contentPadding: EdgeInsets.zero,
          ),
          if (_isPublic)
            TextField(
              controller: _username,
              decoration: InputDecoration(
                labelText: l10n.groupsUsernameHint,
                prefixText: '@',
              ),
            ),
          if (_error != null) ...<Widget>[
            const SizedBox(height: SobhSpacing.lg),
            Text(_error!, style: TextStyle(color: palette.error)),
          ],
          const SizedBox(height: SobhSpacing.xl),
          Text(
            l10n.groupsMembersTitle,
            style: Theme.of(context).textTheme.titleSmall,
          ),
          const SizedBox(height: SobhSpacing.sm),
          contacts.when(
            loading: () => const SobhLoading(),
            error: (Object error, StackTrace _) => SobhErrorState(
              error: error,
              onRetry: () => ref.invalidate(contactListProvider),
            ),
            data: (List<Contact> rows) => rows.isEmpty
                ? SobhEmptyState(
                    icon: Icons.person_add_alt_outlined,
                    title: l10n.contactsEmptyTitle,
                    body: l10n.contactsEmptyBody,
                  )
                : Column(
                    children: <Widget>[
                      for (final Contact contact in rows)
                        CheckboxListTile(
                          value: _selected.contains(contact.userId),
                          onChanged: (bool? selected) => setState(() {
                            selected ?? false
                                ? _selected.add(contact.userId)
                                : _selected.remove(contact.userId);
                          }),
                          title: Text(contact.label),
                          secondary: SobhAvatar(name: contact.label),
                          contentPadding: EdgeInsets.zero,
                        ),
                    ],
                  ),
          ),
        ],
      ),
    );
  }
}

/// The public directory of groups and channels (§15).
class DiscoverScreen extends ConsumerStatefulWidget {
  const DiscoverScreen({super.key});

  @override
  ConsumerState<DiscoverScreen> createState() => _DiscoverScreenState();
}

class _DiscoverScreenState extends ConsumerState<DiscoverScreen> {
  String _query = '';

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<DiscoverableChat>> chats =
        ref.watch(discoverProvider(_query));

    return Scaffold(
      appBar: AppBar(
        title: TextField(
          decoration: InputDecoration(
            hintText: l10n.groupsDiscover,
            border: InputBorder.none,
          ),
          onSubmitted: (String value) => setState(() => _query = value.trim()),
        ),
      ),
      body: chats.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(discoverProvider(_query)),
        ),
        data: (List<DiscoverableChat> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.explore_outlined,
                title: l10n.groupsDiscoverEmpty,
              )
            : ListView.separated(
                itemCount: rows.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final DiscoverableChat chat = rows[index];
                  return ListTile(
                    leading: SobhAvatar(name: chat.title),
                    title: Text(chat.title),
                    subtitle: Text(
                      chat.isChannel
                          ? l10n.groupsSubscribers(chat.memberCount)
                          : l10n.groupsMembers(chat.memberCount),
                    ),
                    trailing: TextButton(
                      onPressed: () async {
                        final bool joined = await ref
                            .read(groupsRepositoryProvider)
                            .join(chat.chatId);
                        if (!context.mounted) {
                          return;
                        }
                        if (joined) {
                          context.go('/chats/${chat.chatId}');
                        } else {
                          ScaffoldMessenger.of(context).showSnackBar(
                            SnackBar(content: Text(l10n.groupsJoinPending)),
                          );
                        }
                      },
                      child: Text(l10n.groupsJoin),
                    ),
                  );
                },
              ),
      ),
    );
  }
}
