import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../chat/data/chat_repository.dart';
import '../../secretchat/data/secret_chat_service.dart';
import '../../secretchat/presentation/secret_chat_screen.dart';
import '../data/address_book.dart';
import '../data/contacts_repository.dart';
import 'contact_requests_screen.dart';

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
            tooltip: l10n.contactRequestsTitle,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const ContactRequestsScreen(),
              ),
            ),
            icon: const Icon(Icons.person_add_alt_1_outlined),
          ),
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

  /// Opens an encrypted conversation, creating it on first use (§24).
  ///
  /// It is pushed rather than routed through GoRouter: the screen needs the
  /// peer's id and name, and putting a user id in the address bar of a
  /// conversation whose whole point is that it leaves no trace is the wrong
  /// default.
  Future<void> _openSecretChat(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    final NavigatorState navigator = Navigator.of(context);

    try {
      final String chatId =
          await ref.read(secretChatServiceProvider).openChat(contact.userId);

      await navigator.push(
        MaterialPageRoute<void>(
          builder: (_) => SecretChatScreen(
            chatId: chatId,
            peerUserId: contact.userId,
            peerName: contact.label,
          ),
        ),
      );
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(
            switch (error.code) {
              // The server refuses a chat with someone who has published no
              // keys, which is a fact about them rather than a fault.
              ApiErrorCode.notFound => l10n.secretChatNoDevices,
              ApiErrorCode.network => l10n.errorNetwork,
              _ => error.message,
            },
          ),
        ),
      );
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
            case 'secret':
              if (context.mounted) {
                await _openSecretChat(context, ref);
              }
              return;
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
            value: 'secret',
            child: Row(
              children: <Widget>[
                const Icon(Icons.lock_outline, size: SobhSizes.iconMedium),
                const SizedBox(width: SobhSpacing.sm),
                Text(l10n.secretChatStart),
              ],
            ),
          ),
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
