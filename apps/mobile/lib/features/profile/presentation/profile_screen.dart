import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../../bots/presentation/bots_screen.dart';
import '../../communities/presentation/communities_screen.dart';
import '../../contacts/presentation/contacts_screen.dart';
import '../../notifications/presentation/notifications_screen.dart';
import '../../settings/presentation/settings_screen.dart';
import '../../stickers/presentation/sticker_picker.dart';
import '../data/profile_repository.dart';
import 'edit_profile_screen.dart';

/// The user's own profile and the entry point to settings (§43).
class ProfileScreen extends ConsumerWidget {
  const ProfileScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SessionState session = ref.watch(sessionControllerProvider);
    final AsyncValue<SelfProfile> profile = ref.watch(selfProfileProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.profileTitle),
        actions: <Widget>[
          IconButton(
            icon: const Icon(Icons.edit_outlined),
            tooltip: l10n.profileEditTitle,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const EditProfileScreen(),
              ),
            ),
          ),
          IconButton(
            icon: const Icon(Icons.settings_outlined),
            tooltip: l10n.settingsTitle,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const SettingsScreen()),
            ),
          ),
        ],
      ),
      body: ListView(
        children: <Widget>[
          const SizedBox(height: SobhSpacing.xl),
          Center(
            child: Column(
              children: <Widget>[
                SobhAvatar(
                  name: profile.maybeWhen(
                    orElse: () => l10n.appName,
                    data: (SelfProfile self) => self.displayName.isEmpty
                        ? l10n.appName
                        : self.displayName,
                  ),
                  radius: SobhSizes.avatarLarge / 2,
                ),
                const SizedBox(height: SobhSpacing.md),
                Text(
                  profile.maybeWhen(
                    orElse: () => l10n.profileTitle,
                    data: (SelfProfile self) => self.displayName.isEmpty
                        ? l10n.profileTitle
                        : self.displayName,
                  ),
                  style: Theme.of(context).textTheme.titleLarge,
                ),
                profile.maybeWhen(
                  orElse: () => const SizedBox.shrink(),
                  data: (SelfProfile self) => Column(
                    children: <Widget>[
                      if (self.username != null)
                        Text(
                          self.handle,
                          style: TextStyle(
                            color: SobhTheme.of(context).textSecondary,
                          ),
                        ),
                      if (self.about.isNotEmpty)
                        Padding(
                          padding: const EdgeInsets.symmetric(
                            horizontal: SobhSpacing.xl,
                            vertical: SobhSpacing.sm,
                          ),
                          child: Text(self.about, textAlign: TextAlign.center),
                        ),
                    ],
                  ),
                ),
              ],
            ),
          ),
          const SizedBox(height: SobhSpacing.xl),
          const Divider(),
          ListTile(
            leading: const Icon(Icons.contacts_outlined),
            title: Text(l10n.contactsTitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const ContactsScreen()),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.block_outlined),
            title: Text(l10n.contactsBlocked),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const BlockedContactsScreen(),
              ),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.smart_toy_outlined),
            title: Text(l10n.botsTitle),
            subtitle: Text(l10n.botsSubtitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const BotsScreen()),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.emoji_emotions_outlined),
            title: Text(l10n.stickersTitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const StickerStoreScreen(),
              ),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.groups_outlined),
            title: Text(l10n.communitiesTitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const CommunitiesScreen(),
              ),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.notifications_none),
            title: Text(l10n.notificationsTitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const NotificationsScreen(),
              ),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.devices_outlined),
            title: Text(l10n.settingsSessions),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const SessionsScreen()),
            ),
          ),
          const Divider(),
          ListTile(
            leading: Icon(Icons.logout, color: SobhTheme.of(context).error),
            title: Text(
              l10n.profileSignOut,
              style: TextStyle(color: SobhTheme.of(context).error),
            ),
            onTap: () => _confirmSignOut(context, ref, l10n),
          ),
          if (session.deviceId != null)
            Padding(
              padding: const EdgeInsets.all(SobhSpacing.lg),
              child: Text(
                session.deviceId!,
                textAlign: TextAlign.center,
                style: Theme.of(context)
                    .textTheme
                    .labelSmall
                    ?.copyWith(color: SobhTheme.of(context).textDisabled),
              ),
            ),
        ],
      ),
    );
  }

  Future<void> _confirmSignOut(
    BuildContext context,
    WidgetRef ref,
    AppLocalizations l10n,
  ) async {
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            content: Text(l10n.profileSignOutConfirm),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.profileSignOut),
              ),
            ],
          ),
        ) ??
        false;

    if (confirmed) {
      // The router redirects to sign-in as soon as the session clears, so
      // there is nothing to navigate to here.
      await ref.read(sessionControllerAsyncProvider.notifier).signOut();
    }
  }
}
