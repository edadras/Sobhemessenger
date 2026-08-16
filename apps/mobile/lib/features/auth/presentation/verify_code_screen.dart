import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../session_controller.dart';
import 'phone_entry_screen.dart';

/// Step two: verify the code, and collect the two-step password if the account
/// has one (§11, §57).
class VerifyCodeScreen extends ConsumerStatefulWidget {
  const VerifyCodeScreen({required this.phone, super.key});

  final String phone;

  @override
  ConsumerState<VerifyCodeScreen> createState() => _VerifyCodeScreenState();
}

class _VerifyCodeScreenState extends ConsumerState<VerifyCodeScreen> {
  final TextEditingController _codeController = TextEditingController();
  final TextEditingController _passwordController = TextEditingController();

  bool _submitting = false;
  bool _needsPassword = false;
  String? _error;
  int _resendIn = 60;
  Timer? _resendTimer;

  @override
  void initState() {
    super.initState();
    _startResendCountdown();
  }

  @override
  void dispose() {
    _resendTimer?.cancel();
    _codeController.dispose();
    _passwordController.dispose();
    super.dispose();
  }

  void _startResendCountdown() {
    _resendTimer?.cancel();
    setState(() => _resendIn = 60);
    _resendTimer = Timer.periodic(const Duration(seconds: 1), (Timer timer) {
      if (!mounted) {
        timer.cancel();
        return;
      }
      setState(() => _resendIn--);
      if (_resendIn <= 0) {
        timer.cancel();
      }
    });
  }

  Future<void> _submit() async {
    setState(() {
      _submitting = true;
      _error = null;
    });

    try {
      await ref.read(sessionControllerAsyncProvider.notifier).verifyCode(
            phone: widget.phone,
            code: _codeController.text.trim(),
            password: _needsPassword ? _passwordController.text : null,
            deviceName: defaultDeviceName(),
            platform: currentPlatform(),
          );
      // The router redirects to the chat list once the session exists.
    } on ApiException catch (error) {
      if (!mounted) {
        return;
      }
      // The code stays valid across this retry, so only the password field
      // needs to appear — the user does not start over.
      if (error.code == ApiErrorCode.twoStepRequired) {
        setState(() => _needsPassword = true);
        return;
      }
      setState(() => _error = _messageFor(error));
    } finally {
      if (mounted) {
        setState(() => _submitting = false);
      }
    }
  }

  Future<void> _resend() async {
    try {
      await ref.read(sessionControllerAsyncProvider.notifier).requestCode(widget.phone);
      _startResendCountdown();
    } on ApiException catch (error) {
      if (mounted) {
        setState(() => _error = _messageFor(error));
      }
    }
  }

  String _messageFor(ApiException error) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    return switch (error.code) {
      ApiErrorCode.invalidOtp => l10n.authInvalidCode,
      ApiErrorCode.otpExpired || ApiErrorCode.otpAttemptsExceeded => l10n.authCodeExpired,
      ApiErrorCode.invalidPassword => l10n.authInvalidCode,
      ApiErrorCode.rateLimited => l10n.errorRateLimited,
      ApiErrorCode.network || ApiErrorCode.timeout => l10n.errorNetwork,
      _ => l10n.errorGeneric,
    };
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;

    return Scaffold(
      appBar: AppBar(title: Text(l10n.authCodeTitle)),
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(SobhSpacing.xl),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: <Widget>[
              Text(
                l10n.authCodeSubtitle(widget.phone),
                style: text.bodyMedium?.copyWith(color: palette.textSecondary),
              ),
              const SizedBox(height: SobhSpacing.xl),
              TextField(
                controller: _codeController,
                keyboardType: TextInputType.number,
                textDirection: TextDirection.ltr,
                autofocus: true,
                enabled: !_submitting,
                inputFormatters: <TextInputFormatter>[
                  FilteringTextInputFormatter.digitsOnly,
                  LengthLimitingTextInputFormatter(8),
                ],
                decoration: InputDecoration(
                  hintText: '------',
                  errorText: _needsPassword ? null : _error,
                ),
                onSubmitted: (_) => _submit(),
              ),
              if (_needsPassword) ...<Widget>[
                const SizedBox(height: SobhSpacing.lg),
                Text(l10n.authPasswordTitle, style: text.titleSmall),
                const SizedBox(height: SobhSpacing.sm),
                TextField(
                  controller: _passwordController,
                  obscureText: true,
                  autofocus: true,
                  enabled: !_submitting,
                  decoration: InputDecoration(
                    hintText: l10n.authPasswordHint,
                    errorText: _error,
                  ),
                  onSubmitted: (_) => _submit(),
                ),
              ],
              const SizedBox(height: SobhSpacing.xl),
              FilledButton(
                onPressed: _submitting ? null : _submit,
                child: _submitting
                    ? const SizedBox(
                        height: SobhSizes.iconSmall,
                        width: SobhSizes.iconSmall,
                        child: CircularProgressIndicator(strokeWidth: 2),
                      )
                    : Text(l10n.commonContinue),
              ),
              const SizedBox(height: SobhSpacing.md),
              Center(
                child: TextButton(
                  onPressed: _resendIn > 0 || _submitting ? null : _resend,
                  child: Text(
                    _resendIn > 0 ? l10n.authResendIn(_resendIn) : l10n.authResend,
                  ),
                ),
              ),
            ],
          ),
        ),
      ),
    );
  }
}
