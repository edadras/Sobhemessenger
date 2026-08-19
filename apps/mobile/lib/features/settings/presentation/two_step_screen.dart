import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../profile/data/profile_repository.dart';
import '../data/account_repository.dart';

/// The two-step password (§57).
///
/// The app could ask for this password at sign-in and, since the recovery
/// screen was built, clear a forgotten one — with no way to set one in the
/// first place. A second factor nobody can turn on is not a second factor.
///
/// Three actions through one endpoint, because they are one decision: what the
/// second factor on this account is, with nothing being a valid answer.
class TwoStepScreen extends ConsumerStatefulWidget {
  const TwoStepScreen({super.key});

  @override
  ConsumerState<TwoStepScreen> createState() => _TwoStepScreenState();
}

class _TwoStepScreenState extends ConsumerState<TwoStepScreen> {
  final TextEditingController _current = TextEditingController();
  final TextEditingController _password = TextEditingController();
  final TextEditingController _repeat = TextEditingController();
  final TextEditingController _hint = TextEditingController();

  bool _working = false;
  String? _error;

  @override
  void dispose() {
    _current.dispose();
    _password.dispose();
    _repeat.dispose();
    _hint.dispose();
    super.dispose();
  }

  void _report(Object error) {
    if (!mounted) {
      return;
    }
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() {
      _error = error is ApiException
          ? (error.isOffline ? l10n.errorNetwork : error.message)
          : '$error';
    });
  }

  Future<void> _save({required bool enabled}) async {
    final AppLocalizations l10n = AppLocalizations.of(context);

    if (enabled) {
      if (_password.text.length < 8) {
        setState(() => _error = l10n.twoStepTooShort);
        return;
      }
      // Checked here as well as on the server, because a mistyped repeat is
      // the one failure the server cannot see: both fields are the caller's,
      // and it only ever receives one of them.
      if (_password.text != _repeat.text) {
        setState(() => _error = l10n.twoStepMismatch);
        return;
      }
    }

    setState(() {
      _working = true;
      _error = null;
    });

    try {
      await ref.read(accountRepositoryProvider).setTwoStepPassword(
            password: enabled ? _password.text : '',
            current: _current.text,
            hint: enabled ? _hint.text.trim() : '',
          );
      ref.invalidate(selfProfileProvider);
      if (mounted) {
        _current.clear();
        _password.clear();
        _repeat.clear();
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(enabled ? l10n.twoStepSaved : l10n.twoStepRemoved),
          ),
        );
      }
    } on ApiException catch (error) {
      _report(error);
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  /// Removing the second factor is confirmed, and says what it costs: recovery
  /// by email exists to rescue a forgotten password, so turning the password
  /// off is turning off the thing recovery recovers.
  Future<void> _confirmRemoval() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.twoStepRemove),
            content: Text(l10n.twoStepRemoveConfirm),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.twoStepRemove),
              ),
            ],
          ),
        ) ??
        false;
    if (confirmed) {
      await _save(enabled: false);
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<SelfProfile> profile = ref.watch(selfProfileProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.twoStepTitle)),
      body: profile.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(selfProfileProvider),
        ),
        data: (SelfProfile me) {
          final bool enabled = me.twoStepEnabled;

          return ListView(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            children: <Widget>[
              Text(
                enabled ? l10n.twoStepOn : l10n.twoStepOff,
                style: Theme.of(context).textTheme.titleSmall?.copyWith(
                      color: enabled ? palette.success : palette.textSecondary,
                    ),
              ),
              const SizedBox(height: SobhSpacing.sm),
              Text(
                l10n.twoStepBody,
                style: Theme.of(context)
                    .textTheme
                    .bodyMedium
                    ?.copyWith(color: palette.textSecondary),
              ),
              if (enabled && me.twoStepHint.isNotEmpty) ...<Widget>[
                const SizedBox(height: SobhSpacing.md),
                Text(l10n.twoStepCurrentHint(me.twoStepHint)),
              ],
              const SizedBox(height: SobhSpacing.lg),

              // Only when one already exists: proving you know the current
              // password is what stops somebody holding a live session from
              // replacing the factor that guards against exactly that.
              if (enabled) ...<Widget>[
                TextField(
                  controller: _current,
                  obscureText: true,
                  enabled: !_working,
                  decoration: InputDecoration(
                    labelText: l10n.twoStepCurrent,
                    prefixIcon: const Icon(Icons.lock_outline),
                  ),
                ),
                const SizedBox(height: SobhSpacing.lg),
              ],

              TextField(
                controller: _password,
                obscureText: true,
                enabled: !_working,
                decoration: InputDecoration(
                  labelText: enabled ? l10n.twoStepNew : l10n.twoStepPassword,
                  helperText: l10n.twoStepMinimum,
                  prefixIcon: const Icon(Icons.password_outlined),
                ),
                onChanged: (_) {
                  if (_error != null) {
                    setState(() => _error = null);
                  }
                },
              ),
              const SizedBox(height: SobhSpacing.md),
              TextField(
                controller: _repeat,
                obscureText: true,
                enabled: !_working,
                decoration: InputDecoration(
                  labelText: l10n.twoStepRepeat,
                  errorText: _error,
                  prefixIcon: const Icon(Icons.password_outlined),
                ),
              ),
              const SizedBox(height: SobhSpacing.md),
              TextField(
                controller: _hint,
                enabled: !_working,
                decoration: InputDecoration(
                  labelText: l10n.twoStepHint,
                  // The hint is shown to anyone who reaches the password
                  // prompt, which is anyone holding a code for this number.
                  helperText: l10n.twoStepHintWarning,
                  prefixIcon: const Icon(Icons.lightbulb_outline),
                ),
              ),
              const SizedBox(height: SobhSpacing.xl),
              FilledButton(
                onPressed: _working ? null : () => _save(enabled: true),
                child: Text(enabled ? l10n.twoStepChange : l10n.twoStepEnable),
              ),
              if (enabled) ...<Widget>[
                const SizedBox(height: SobhSpacing.md),
                OutlinedButton(
                  onPressed: _working ? null : _confirmRemoval,
                  child: Text(
                    l10n.twoStepRemove,
                    style: TextStyle(color: palette.error),
                  ),
                ),
              ],
              if (_working)
                const Padding(
                  padding: EdgeInsets.only(top: SobhSpacing.lg),
                  child: LinearProgressIndicator(),
                ),
            ],
          );
        },
      ),
    );
  }
}
