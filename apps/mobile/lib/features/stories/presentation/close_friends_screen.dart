import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/widgets/async_states.dart';
import '../../contacts/data/contacts_repository.dart';
import '../data/stories_repository.dart';

/// Who counts as a close friend for story privacy (§17).
///
/// The list is drawn from contacts rather than from everyone the account has
/// ever spoken to: a story shared with "close friends" is a deliberately small
/// audience, and a picker over every stranger who has ever sent a message
/// would make it easy to add the wrong person.
///
/// The whole list is sent on save because that is what the server stores — it
/// replaces rather than merges, so an edit that sent only the additions would
/// quietly remove everybody else.
class CloseFriendsScreen extends ConsumerStatefulWidget {
  const CloseFriendsScreen({super.key});

  @override
  ConsumerState<CloseFriendsScreen> createState() => _CloseFriendsScreenState();
}

class _CloseFriendsScreenState extends ConsumerState<CloseFriendsScreen> {
  /// Null until the saved list has loaded — an empty set would otherwise be
  /// indistinguishable from "nobody is chosen", and saving before the load
  /// finished would wipe the list.
  Set<String>? _chosen;
  bool _saving = false;

  Future<void> _save() async {
    final Set<String>? chosen = _chosen;
    if (chosen == null || _saving) {
      return;
    }

    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    setState(() => _saving = true);
    try {
      await ref
          .read(storiesRepositoryProvider)
          .setCloseFriends(chosen.toList(growable: false));
      ref.invalidate(closeFriendsProvider);
      if (mounted) {
        Navigator.of(context).pop();
      }
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    } finally {
      if (mounted) {
        setState(() => _saving = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<String>> saved = ref.watch(closeFriendsProvider);
    final AsyncValue<List<Contact>> contacts = ref.watch(contactListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.storiesCloseFriends),
        actions: <Widget>[
          TextButton(
            onPressed: _chosen == null || _saving ? null : _save,
            child: Text(l10n.commonSave),
          ),
        ],
      ),
      body: saved.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(closeFriendsProvider),
        ),
        data: (List<String> current) {
          _chosen ??= current.toSet();
          return contacts.when(
            loading: () => const SobhLoading(),
            error: (Object error, StackTrace _) => SobhErrorState(
              error: error,
              onRetry: () => ref.invalidate(contactListProvider),
            ),
            data: (List<Contact> rows) {
              if (rows.isEmpty) {
                return SobhEmptyState(
                  icon: Icons.people_outline,
                  title: l10n.contactsEmptyTitle,
                );
              }
              return ListView.builder(
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) {
                  final Contact contact = rows[index];
                  final bool chosen = _chosen!.contains(contact.userId);
                  return CheckboxListTile(
                    value: chosen,
                    onChanged: (bool? value) => setState(() {
                      if (value ?? false) {
                        _chosen!.add(contact.userId);
                      } else {
                        _chosen!.remove(contact.userId);
                      }
                    }),
                    secondary: SobhAvatar(name: contact.displayName),
                    title: Text(contact.displayName),
                    subtitle: contact.username == null
                        ? null
                        : Text('@${contact.username}'),
                  );
                },
              );
            },
          );
        },
      ),
    );
  }
}
