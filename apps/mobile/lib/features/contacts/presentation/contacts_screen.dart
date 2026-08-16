import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../chat/data/chat_repository.dart';
import '../data/address_book.dart';
import '../data/contacts_repository.dart';

/// The address book (§54).
class ContactsScreen extends ConsumerStatefulWidget {
  const ContactsScreen({super.key});

  @override
  ConsumerState<ContactsScreen> createState() => _ContactsScreenState();
}

class _ContactsScreenState extends ConsumerState<ContactsScreen> {
  bool _syncing = false;

  Future<void> _sync() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _syncing = true);

    try {
      final List<LocalContact>? book =
          await ref.read(addressBookProvider).read();
      if (!mounted) {
        return;
      }
      if (book == null) {
        _tell(l10n.contactsPermissionDenied);
        return;
      }

      final int matched = await ref.read(contactsRepositoryProvider).sync(book);
      if (!mounted) {
        return;
      }
      _tell(l10n.contactsSyncResult(matched));
      ref.invalidate(contactListProvider);
    } on ApiException catch (error) {
      if (mounted) {
        _tell(error.isOffline ? l10n.errorNetwork : error.message);
      }
    } finally {
      if (mounted) {
        setState(() => _syncing = false);
      }
    }
  }

  void _tell(String message) => ScaffoldMessenger.of(context)
      .showSnackBar(SnackBar(content: Text(message)));

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Contact>> contacts = ref.watch(contactListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.contactsTitle),
        actions: <Widget>[
          IconButton(
            tooltip: l10n.contactsBlocked,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const BlockedContactsScreen(),
              ),
            ),
            icon: const Icon(Icons.block_outlined),
          ),
          IconButton(
            tooltip: l10n.contactsSync,
            onPressed: _syncing ? null : _sync,
            icon: _syncing
                ? const SizedBox(
                    width: SobhSizes.iconSmall,
                    height: SobhSizes.iconSmall,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                : const Icon(Icons.sync),
          ),
        ],
      ),
      body: contacts.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(contactListProvider),
        ),
        data: (List<Contact> rows) {
          if (rows.isEmpty) {
            return SobhEmptyState(
              icon: Icons.person_add_alt_outlined,
              title: l10n.contactsEmptyTitle,
              body: '${l10n.contactsEmptyBody}\n\n${l10n.contactsSyncPrivacy}',
              action: FilledButton.icon(
                onPressed: _syncing ? null : _sync,
                icon: const Icon(Icons.sync),
                label:
                    Text(_syncing ? l10n.contactsSyncing : l10n.contactsSync),
              ),
            );
          }

          return RefreshIndicator(
            onRefresh: () async => ref.invalidate(contactListProvider),
            child: ListView.separated(
              itemCount: rows.length,
              separatorBuilder: (_, __) => const Divider(
                indent: SobhSpacing.xxl + SobhSpacing.lg,
                height: 1,
              ),
              itemBuilder: (BuildContext context, int index) =>
                  _ContactTile(contact: rows[index]),
            ),
          );
        },
      ),
    );
  }
}

class _ContactTile extends ConsumerWidget {
  const _ContactTile({required this.contact});

  final Contact contact;

  Future<void> _openChat(BuildContext context, WidgetRef ref) async {
    final String chatId =
        await ref.read(chatRepositoryProvider).openPrivateChat(contact.userId);
    if (context.mounted) {
      context.go('/chats/$chatId');
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return ListTile(
      onTap: () => _openChat(context, ref),
      leading: SobhAvatar(name: contact.label),
      title: Text(contact.label, maxLines: 1, overflow: TextOverflow.ellipsis),
      subtitle: contact.username != null
          ? Text(
              '@${contact.username}',
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            )
          : null,
      trailing: PopupMenuButton<String>(
        onSelected: (String action) async {
          final ContactsRepository repository =
              ref.read(contactsRepositoryProvider);
          switch (action) {
            case 'favorite':
              await repository.setFavorite(contact.userId, !contact.isFavorite);
            case 'block':
              await repository.block(contact.userId);
              ref.invalidate(blockedContactsProvider);
            case 'remove':
              await repository.remove(contact.userId);
          }
          ref.invalidate(contactListProvider);
        },
        itemBuilder: (BuildContext context) => <PopupMenuEntry<String>>[
          PopupMenuItem<String>(
            value: 'favorite',
            child: Row(
              children: <Widget>[
                Icon(
                  contact.isFavorite ? Icons.star : Icons.star_border,
                  size: SobhSizes.iconMedium,
                ),
                const SizedBox(width: SobhSpacing.sm),
                Text(l10n.contactsFavorite),
              ],
            ),
          ),
          PopupMenuItem<String>(
            value: 'block',
            child: Text(l10n.contactsBlock),
          ),
          PopupMenuItem<String>(
            value: 'remove',
            child: Text(l10n.commonRemove),
          ),
        ],
      ),
    );
  }
}

/// Blocked users (§55).
class BlockedContactsScreen extends ConsumerWidget {
  const BlockedContactsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Contact>> blocked =
        ref.watch(blockedContactsProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.contactsBlocked)),
      body: blocked.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(blockedContactsProvider),
        ),
        data: (List<Contact> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.block_outlined,
                title: l10n.contactsBlockedEmpty,
              )
            : ListView.separated(
                itemCount: rows.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Contact contact = rows[index];
                  return ListTile(
                    leading: SobhAvatar(name: contact.label),
                    title: Text(contact.label),
                    trailing: TextButton(
                      onPressed: () async {
                        await ref
                            .read(contactsRepositoryProvider)
                            .unblock(contact.userId);
                        ref
                          ..invalidate(blockedContactsProvider)
                          ..invalidate(contactListProvider);
                      },
                      child: Text(l10n.contactsUnblock),
                    ),
                  );
                },
              ),
      ),
    );
  }
}
