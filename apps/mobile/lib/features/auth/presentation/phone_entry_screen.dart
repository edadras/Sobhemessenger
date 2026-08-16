import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/routing/app_router.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../session_controller.dart';

/// Step one of sign-in: collect the phone number and request a code (§11).
class PhoneEntryScreen extends ConsumerStatefulWidget {
  const PhoneEntryScreen({super.key});

  @override
  ConsumerState<PhoneEntryScreen> createState() => _PhoneEntryScreenState();
}

class _PhoneEntryScreenState extends ConsumerState<PhoneEntryScreen> {
  final TextEditingController _controller = TextEditingController();
  bool _submitting = false;
  String? _error;

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final String phone = _controller.text.trim();
    if (phone.isEmpty) {
      setState(() => _error = AppLocalizations.of(context).authInvalidPhone);
      return;
    }

    setState(() {
      _submitting = true;
      _error = null;
    });

    try {
      await ref
          .read(sessionControllerAsyncProvider.notifier)
          .requestCode(phone);
      if (!mounted) {
        return;
      }
      context.go('${Routes.verifyCode}?phone=${Uri.encodeComponent(phone)}');
    } on ApiException catch (error) {
      if (!mounted) {
        return;
      }
      setState(() => _error = _messageFor(error));
    } finally {
      if (mounted) {
        setState(() => _submitting = false);
      }
    }
  }

  String _messageFor(ApiException error) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    return switch (error.code) {
      ApiErrorCode.validationFailed => l10n.authInvalidPhone,
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
      body: SafeArea(
        child: Padding(
          padding: const EdgeInsets.all(SobhSpacing.xl),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: <Widget>[
              const SizedBox(height: SobhSpacing.xxxl),
              Text(
                l10n.appName,
                style: text.headlineLarge?.copyWith(color: palette.primary),
              ),
              const SizedBox(height: SobhSpacing.xxl),
              Text(l10n.authPhoneTitle, style: text.titleLarge),
              const SizedBox(height: SobhSpacing.sm),
              Text(
                l10n.authPhoneSubtitle,
                style: text.bodyMedium?.copyWith(
                  color: palette.textSecondary,
                ),
              ),
              const SizedBox(height: SobhSpacing.xl),
              TextField(
                controller: _controller,
                keyboardType: TextInputType.phone,
                autofillHints: const <String>[AutofillHints.telephoneNumber],
                // The number is always LTR, even inside an RTL layout.
                textDirection: TextDirection.ltr,
                enabled: !_submitting,
                decoration: InputDecoration(
                  hintText: l10n.authPhoneHint,
                  errorText: _error,
                  prefixIcon: const Icon(Icons.phone_outlined),
                ),
                inputFormatters: <TextInputFormatter>[
                  LengthLimitingTextInputFormatter(20),
                ],
                onSubmitted: (_) => _submit(),
              ),
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
            ],
          ),
        ),
      ),
    );
  }
}

/// Used as the registered device name, so the user can recognise the entry in
/// their active-devices list (§56).
String defaultDeviceName() {
  if (Platform.isAndroid) {
    return 'Android';
  }
  if (Platform.isIOS) {
    return 'iPhone';
  }
  return Platform.operatingSystem;
}

String currentPlatform() {
  if (Platform.isAndroid) {
    return 'android';
  }
  if (Platform.isIOS) {
    return 'ios';
  }
  return 'desktop';
}
