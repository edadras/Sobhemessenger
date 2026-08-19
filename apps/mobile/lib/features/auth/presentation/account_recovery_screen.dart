import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../settings/data/account_repository.dart';

/// Recovering an account behind a forgotten two-step password (§4).
///
/// The one flow in the app that runs with nobody signed in, because not being
/// able to sign in is the problem it solves. It does not sign anyone in
/// either: the account is still behind its phone number and a code. What it
/// clears is the forgotten second factor — and every session that existed
/// before it, which is what protects an account somebody else already had a
/// foothold in.
///
/// The server answers a start request identically whether or not the address
/// is on an account, so this screen cannot be used to find out who has one.
/// That is why it always advances to the code step and never says "no such
/// address".
class AccountRecoveryScreen extends ConsumerStatefulWidget {
  const AccountRecoveryScreen({super.key});

  @override
  ConsumerState<AccountRecoveryScreen> createState() =>
      _AccountRecoveryScreenState();
}

class _AccountRecoveryScreenState extends ConsumerState<AccountRecoveryScreen> {
  final TextEditingController _email = TextEditingController();
  final TextEditingController _code = TextEditingController();

  bool _codeSent = false;
  bool _done = false;
  bool _working = false;
  String? _error;

  @override
  void dispose() {
    _email.dispose();
    _code.dispose();
    super.dispose();
  }

  Future<void> _start() async {
    final String email = _email.text.trim();
    if (email.isEmpty || _working) {
      return;
    }
    setState(() {
      _working = true;
      _error = null;
    });

    try {
      await ref.read(accountRepositoryProvider).startRecovery(email);
      if (mounted) {
        setState(() => _codeSent = true);
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(() => _error = _messageFor(error));
      }
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  Future<void> _complete() async {
    final String code = _code.text.trim();
    if (code.isEmpty || _working) {
      return;
    }
    setState(() {
      _working = true;
      _error = null;
    });

    try {
      await ref
          .read(accountRepositoryProvider)
          .completeRecovery(_email.text.trim(), code);
      if (mounted) {
        setState(() => _done = true);
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(() => _error = _messageFor(error));
      }
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  String _messageFor(ApiException error) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    return switch (error.code) {
      ApiErrorCode.network || ApiErrorCode.timeout => l10n.errorNetwork,
      ApiErrorCode.rateLimited => l10n.errorRateLimited,
      // A wrong or expired code is one message: telling the two apart would
      // say whether the guess was close.
      ApiErrorCode.invalidOtp ||
      ApiErrorCode.otpExpired ||
      ApiErrorCode.otpAttemptsExceeded =>
        l10n.recoveryCodeWrong,
      _ => error.message,
    };
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;

    return Scaffold(
      appBar: AppBar(title: Text(l10n.recoveryTitle)),
      body: Padding(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        child: _done
            ? Column(
                mainAxisAlignment: MainAxisAlignment.center,
                children: <Widget>[
                  Icon(
                    Icons.lock_open_outlined,
                    size: SobhSizes.iconLarge,
                    color: palette.success,
                  ),
                  const SizedBox(height: SobhSpacing.lg),
                  Text(l10n.recoveryDoneTitle, style: text.titleMedium),
                  const SizedBox(height: SobhSpacing.sm),
                  Text(
                    l10n.recoveryDoneBody,
                    textAlign: TextAlign.center,
                    style: text.bodyMedium?.copyWith(
                      color: palette.textSecondary,
                    ),
                  ),
                  const SizedBox(height: SobhSpacing.xl),
                  FilledButton(
                    onPressed: () => Navigator.of(context).pop(),
                    child: Text(l10n.recoveryBackToSignIn),
                  ),
                ],
              )
            : ListView(
                children: <Widget>[
                  Text(
                    _codeSent ? l10n.recoveryCodeSent : l10n.recoveryBody,
                    style: text.bodyMedium?.copyWith(
                      color: palette.textSecondary,
                    ),
                  ),
                  const SizedBox(height: SobhSpacing.lg),
                  TextField(
                    controller: _email,
                    enabled: !_codeSent && !_working,
                    autofocus: !_codeSent,
                    keyboardType: TextInputType.emailAddress,
                    textDirection: TextDirection.ltr,
                    decoration: InputDecoration(
                      labelText: l10n.recoveryEmailField,
                      prefixIcon: const Icon(Icons.alternate_email),
                      errorText: _codeSent ? null : _error,
                    ),
                    onSubmitted: (_) => _start(),
                  ),
                  if (_codeSent) ...<Widget>[
                    const SizedBox(height: SobhSpacing.lg),
                    TextField(
                      controller: _code,
                      autofocus: true,
                      enabled: !_working,
                      keyboardType: TextInputType.number,
                      textDirection: TextDirection.ltr,
                      decoration: InputDecoration(
                        labelText: l10n.recoveryCodeField,
                        prefixIcon: const Icon(Icons.pin_outlined),
                        errorText: _error,
                      ),
                      onSubmitted: (_) => _complete(),
                    ),
                  ],
                  const SizedBox(height: SobhSpacing.xl),
                  FilledButton(
                    onPressed:
                        _working ? null : (_codeSent ? _complete : _start),
                    child: Text(
                      _codeSent ? l10n.recoveryConfirm : l10n.recoverySend,
                    ),
                  ),
                  if (_working)
                    const Padding(
                      padding: EdgeInsets.only(top: SobhSpacing.lg),
                      child: LinearProgressIndicator(),
                    ),
                ],
              ),
      ),
    );
  }
}
