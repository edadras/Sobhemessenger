import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/settings/settings_controller.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/account_repository.dart';

/// App settings (§43, §44, §55).
class SettingsScreen extends ConsumerWidget {
  const SettingsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AppSettings settings = ref.watch(settingsControllerProvider);
    final SettingsController controller =
        ref.read(settingsControllerProvider.notifier);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.settingsTitle)),
      body: ListView(
        children: <Widget>[
          _SectionHeader(title: l10n.settingsTheme),
          for (final (ThemeMode mode, String label) in <(ThemeMode, String)>[
            (ThemeMode.system, l10n.settingsThemeSystem),
            (ThemeMode.light, l10n.settingsThemeLight),
            (ThemeMode.dark, l10n.settingsThemeDark),
          ])
            RadioListTile<ThemeMode>(
              value: mode,
              groupValue: settings.themeMode,
              onChanged: (ThemeMode? value) =>
                  value == null ? null : controller.setThemeMode(value),
              title: Text(label),
            ),

          _SectionHeader(title: l10n.settingsLanguage),
          // The names stay in their own language: someone who switched the app
          // to a language they cannot read needs to find their way back.
          for (final (String code, String label) in <(String, String)>[
            ('fa', 'فارسی'),
            ('en', 'English'),
            ('tr', 'Türkçe'),
            ('ar', 'العربية'),
          ])
            RadioListTile<String>(
              value: code,
              groupValue: settings.locale.languageCode,
              onChanged: (String? value) =>
                  value == null ? null : controller.setLocale(Locale(value)),
              title: Text(label),
            ),

          const Divider(),
          ListTile(
            leading: const Icon(Icons.notifications_outlined),
            title: Text(l10n.settingsNotifications),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const NotificationSettingsScreen(),
              ),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.lock_outline),
            title: Text(l10n.settingsPrivacy),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const PrivacyScreen()),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.devices_outlined),
            title: Text(l10n.settingsSessions),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const SessionsScreen()),
            ),
          ),
          ListTile(
            leading: const Icon(Icons.alternate_email),
            title: Text(l10n.recoveryEmailTitle),
            onTap: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => const RecoveryEmailScreen(),
              ),
            ),
          ),
        ],
      ),
    );
  }
}

/// Notification preferences (§30).
class NotificationSettingsScreen extends ConsumerWidget {
  const NotificationSettingsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<NotificationSettings> settings =
        ref.watch(notificationSettingsProvider);

    Future<void> save(NotificationSettings updated) async {
      await ref
          .read(accountRepositoryProvider)
          .updateNotificationSettings(updated);
      ref.invalidate(notificationSettingsProvider);
    }

    return Scaffold(
      appBar: AppBar(title: Text(l10n.settingsNotifications)),
      body: settings.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(notificationSettingsProvider),
        ),
        data: (NotificationSettings current) => ListView(
          children: <Widget>[
            SwitchListTile(
              value: current.privateChats,
              onChanged: (bool value) =>
                  save(current.copyWith(privateChats: value)),
              title: Text(l10n.settingsNotificationsMessages),
            ),
            SwitchListTile(
              value: current.groups,
              onChanged: (bool value) => save(current.copyWith(groups: value)),
              title: Text(l10n.settingsNotificationsGroups),
            ),
            SwitchListTile(
              value: current.channels,
              onChanged: (bool value) =>
                  save(current.copyWith(channels: value)),
              title: Text(l10n.settingsNotificationsChannels),
            ),
            SwitchListTile(
              value: current.breakingNews,
              onChanged: (bool value) =>
                  save(current.copyWith(breakingNews: value)),
              title: Text(l10n.newsBreaking),
            ),
            SwitchListTile(
              value: current.calls,
              onChanged: (bool value) => save(current.copyWith(calls: value)),
              title: Text(l10n.callsTitle),
            ),
            const Divider(),
            SwitchListTile(
              value: current.showPreview,
              onChanged: (bool value) =>
                  save(current.copyWith(showPreview: value)),
              title: Text(l10n.settingsNotificationsPreview),
            ),
            ListTile(
              title: Text(l10n.settingsNotificationsQuietHours),
              subtitle: Text(
                current.quietHoursStart == null || current.quietHoursEnd == null
                    ? l10n.privacyNobody
                    : '${current.quietHoursStart}:00 – ${current.quietHoursEnd}:00',
              ),
            ),
          ],
        ),
      ),
    );
  }
}

/// Who may see what (§55).
///
/// The rules are enforced inside the server's own queries, so a change here
/// takes effect on the next request anyone makes about this account rather than
/// on some later sync. Every key is listed, including ones never touched: the
/// server fills those in with the default it applies anyway, so the screen
/// shows the whole picture rather than only the parts already changed.
class PrivacyScreen extends ConsumerStatefulWidget {
  const PrivacyScreen({super.key});

  @override
  ConsumerState<PrivacyScreen> createState() => _PrivacyScreenState();
}

class _PrivacyScreenState extends ConsumerState<PrivacyScreen> {
  Future<List<PrivacySetting>>? _settings;

  @override
  void initState() {
    super.initState();
    _load();
  }

  void _load() {
    setState(() {
      _settings = ref.read(accountRepositoryProvider).privacySettings();
    });
  }

  /// The label for a key, so the screen never shows a raw identifier like
  /// `group_invites` to someone.
  String _label(AppLocalizations l10n, String key) => switch (key) {
        'last_seen' => l10n.privacyLastSeen,
        'profile_photo' => l10n.privacyProfilePhoto,
        'phone_number' => l10n.privacyPhoneNumber,
        'read_receipts' => l10n.privacyReadReceipts,
        'typing' => l10n.privacyTyping,
        'calls' => l10n.privacyCalls,
        'group_invites' => l10n.privacyGroupInvites,
        'messages' => l10n.privacyMessages,
        'stories' => l10n.privacyStories,
        // A key the server knows and this build does not. Showing it under its
        // own name beats hiding a setting that is in force.
        _ => key,
      };

  String _ruleLabel(AppLocalizations l10n, String rule) => switch (rule) {
        'everyone' => l10n.privacyEveryone,
        'contacts' => l10n.privacyContacts,
        _ => l10n.privacyNobody,
      };

  Future<void> _change(PrivacySetting setting, String rule) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);

    try {
      await ref
          .read(accountRepositoryProvider)
          .setPrivacy(setting.copyWith(rule: rule));
      messenger.showSnackBar(SnackBar(content: Text(l10n.privacySaved)));
      _load();
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.privacyTitle)),
      body: FutureBuilder<List<PrivacySetting>>(
        future: _settings,
        builder: (
          BuildContext context,
          AsyncSnapshot<List<PrivacySetting>> snapshot,
        ) {
          if (snapshot.hasError) {
            return SobhErrorState(error: snapshot.error!, onRetry: _load);
          }
          if (!snapshot.hasData) {
            return const SobhLoading();
          }

          final List<PrivacySetting> settings = snapshot.data!;
          return ListView.separated(
            itemCount: settings.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) {
              final PrivacySetting setting = settings[index];
              final int exceptions =
                  setting.allowList.length + setting.denyList.length;

              return ListTile(
                title: Text(_label(l10n, setting.key)),
                subtitle: exceptions == 0
                    ? null
                    // Exceptions are kept when the rule changes, so saying they
                    // exist stops someone believing a rule applies to everyone
                    // when it does not.
                    : Text(
                        l10n.privacyExceptions(
                          setting.allowList.length,
                          setting.denyList.length,
                        ),
                        style: Theme.of(context)
                            .textTheme
                            .bodySmall
                            ?.copyWith(color: palette.textSecondary),
                      ),
                trailing: DropdownButton<String>(
                  value: setting.rule,
                  underline: const SizedBox.shrink(),
                  onChanged: (String? rule) {
                    if (rule != null && rule != setting.rule) {
                      _change(setting, rule);
                    }
                  },
                  items: <String>['everyone', 'contacts', 'nobody']
                      .map(
                        (String rule) => DropdownMenuItem<String>(
                          value: rule,
                          child: Text(_ruleLabel(l10n, rule)),
                        ),
                      )
                      .toList(),
                ),
              );
            },
          );
        },
      ),
    );
  }
}

/// Active sessions, and the ability to end them (§56).
class SessionsScreen extends ConsumerWidget {
  const SessionsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<UserSession>> sessions =
        ref.watch(sessionListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.settingsSessions),
        actions: <Widget>[
          TextButton(
            onPressed: () async {
              await ref.read(accountRepositoryProvider).revokeOtherSessions();
              ref.invalidate(sessionListProvider);
            },
            child: Text(l10n.settingsSessionsRevokeOthers),
          ),
        ],
      ),
      body: sessions.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(sessionListProvider),
        ),
        data: (List<UserSession> rows) => ListView.separated(
          itemCount: rows.length,
          separatorBuilder: (_, __) => const Divider(height: 1),
          itemBuilder: (BuildContext context, int index) {
            final UserSession session = rows[index];
            return ListTile(
              leading: Icon(
                session.isCurrent ? Icons.smartphone : Icons.devices_other,
                color: session.isCurrent ? SobhTheme.of(context).primary : null,
              ),
              title: Text(
                session.isCurrent
                    ? l10n.settingsSessionsCurrent
                    : (session.userAgent.isEmpty
                        ? session.deviceId
                        : session.userAgent),
              ),
              subtitle: Text(
                <String>[
                  DateFormat.yMd().add_Hm().format(session.lastUsedAt),
                  if (session.ip != null) session.ip!,
                ].join(' · '),
              ),
              // The current session is ended by signing out, not from this
              // list — revoking it here would leave the screen holding a dead
              // token.
              trailing: session.isCurrent
                  ? null
                  : TextButton(
                      onPressed: () async {
                        await ref
                            .read(accountRepositoryProvider)
                            .revokeSession(session.id);
                        ref.invalidate(sessionListProvider);
                      },
                      child: Text(l10n.settingsSessionsRevoke),
                    ),
            );
          },
        ),
      ),
    );
  }
}

class _SectionHeader extends StatelessWidget {
  const _SectionHeader({required this.title});

  final String title;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.fromLTRB(
          SobhSpacing.lg,
          SobhSpacing.lg,
          SobhSpacing.lg,
          SobhSpacing.sm,
        ),
        child: Text(
          title,
          style: Theme.of(context)
              .textTheme
              .labelLarge
              ?.copyWith(color: SobhTheme.of(context).textSecondary),
        ),
      );
}

/// The recovery address on the account (§4).
///
/// Enrolling an address is not the same as having one: it does nothing at all
/// until a code proves it, which is what stops a stolen session from adding a
/// second way in that the owner never confirmed. The screen says so rather
/// than leaving an unverified address looking like protection.
class RecoveryEmailScreen extends ConsumerStatefulWidget {
  const RecoveryEmailScreen({super.key});

  @override
  ConsumerState<RecoveryEmailScreen> createState() =>
      _RecoveryEmailScreenState();
}

class _RecoveryEmailScreenState extends ConsumerState<RecoveryEmailScreen> {
  final TextEditingController _email = TextEditingController();
  final TextEditingController _code = TextEditingController();
  bool _busy = false;

  @override
  void dispose() {
    _email.dispose();
    _code.dispose();
    super.dispose();
  }

  Future<void> _run(Future<void> Function() action, String success) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _busy = true);

    try {
      await action();
      ref.invalidate(recoveryEmailProvider);
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(success)));
      }
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(
              error.isOffline ? l10n.errorNetwork : error.message,
            ),
          ),
        );
      }
    } finally {
      if (mounted) {
        setState(() => _busy = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<RecoveryEmail> status = ref.watch(recoveryEmailProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.recoveryEmailTitle)),
      body: status.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(recoveryEmailProvider),
        ),
        data: (RecoveryEmail recovery) {
          if (!recovery.available) {
            return Padding(
              padding: const EdgeInsets.all(SobhSpacing.xl),
              child: Text(
                l10n.recoveryEmailUnavailable,
                textAlign: TextAlign.center,
              ),
            );
          }

          return ListView(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            children: <Widget>[
              Text(l10n.recoveryEmailBody),
              const SizedBox(height: SobhSpacing.lg),
              ListTile(
                contentPadding: EdgeInsets.zero,
                title: Text(
                  recovery.isSet ? recovery.email : l10n.recoveryEmailNotSet,
                ),
                subtitle: Text(
                  !recovery.isSet
                      ? ''
                      : recovery.verified
                          ? l10n.recoveryEmailVerified
                          : l10n.recoveryEmailUnverified,
                ),
                trailing: recovery.isSet
                    ? IconButton(
                        icon: const Icon(Icons.delete_outline),
                        tooltip: l10n.recoveryEmailRemove,
                        onPressed: _busy
                            ? null
                            : () => _run(
                                  ref
                                      .read(accountRepositoryProvider)
                                      .removeRecoveryEmail,
                                  l10n.recoveryEmailNotSet,
                                ),
                      )
                    : null,
              ),
              const Divider(),
              TextField(
                controller: _email,
                keyboardType: TextInputType.emailAddress,
                decoration: InputDecoration(labelText: l10n.recoveryEmailHint),
              ),
              const SizedBox(height: SobhSpacing.sm),
              FilledButton(
                onPressed: _busy
                    ? null
                    : () => _run(
                          () => ref
                              .read(accountRepositoryProvider)
                              .setRecoveryEmail(_email.text.trim()),
                          l10n.recoveryEmailCodeSent,
                        ),
                child: Text(l10n.commonSave),
              ),
              // The code box is offered whenever an address is enrolled but
              // unproved, which is exactly when it is useful.
              if (recovery.isSet && !recovery.verified) ...<Widget>[
                const SizedBox(height: SobhSpacing.lg),
                TextField(
                  controller: _code,
                  keyboardType: TextInputType.number,
                  decoration:
                      InputDecoration(labelText: l10n.recoveryEmailCodeHint),
                ),
                const SizedBox(height: SobhSpacing.sm),
                FilledButton(
                  onPressed: _busy
                      ? null
                      : () => _run(
                            () => ref
                                .read(accountRepositoryProvider)
                                .verifyRecoveryEmail(_code.text.trim()),
                            l10n.recoveryEmailVerified,
                          ),
                  child: Text(l10n.commonContinue),
                ),
              ],
            ],
          );
        },
      ),
    );
  }
}
