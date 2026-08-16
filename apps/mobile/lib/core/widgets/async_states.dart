import 'package:flutter/material.dart';

import '../localization/generated/app_localizations.dart';
import '../network/api_exception.dart';
import '../theme/app_theme.dart';
import '../theme/design_tokens.dart';

/// The states every asynchronous screen owes the user (§45): loading, a
/// retryable error, and empty.
///
/// They live in one place so a screen cannot ship with a spinner and nothing
/// else, and so the wording of a failure is decided once rather than per
/// screen.

/// SobhLoading is the loading state. A screen that already has cached content
/// should keep showing it rather than replacing it with this.
class SobhLoading extends StatelessWidget {
  const SobhLoading({super.key});

  @override
  Widget build(BuildContext context) => const Center(
        child: Padding(
          padding: EdgeInsets.all(SobhSpacing.xl),
          child: CircularProgressIndicator(),
        ),
      );
}

/// SobhErrorState explains what went wrong in the user's language and offers
/// the only useful action: try again.
class SobhErrorState extends StatelessWidget {
  const SobhErrorState({super.key, required this.error, this.onRetry});

  final Object error;
  final VoidCallback? onRetry;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    // Network and rate-limit failures get their own wording: "something went
    // wrong" is useless when the real answer is "you are offline".
    final String message = switch (error) {
      ApiException(code: ApiErrorCode.network) => l10n.errorNetwork,
      ApiException(code: ApiErrorCode.timeout) => l10n.errorNetwork,
      ApiException(code: ApiErrorCode.rateLimited) => l10n.errorRateLimited,
      ApiException(message: final String message) => message,
      _ => l10n.errorGeneric,
    };

    return Center(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.xl),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            Icon(
              Icons.cloud_off_outlined,
              size: SobhSizes.iconLarge,
              color: palette.textDisabled,
            ),
            const SizedBox(height: SobhSpacing.lg),
            Text(
              message,
              textAlign: TextAlign.center,
              style: Theme.of(context).textTheme.bodyMedium,
            ),
            if (onRetry != null) ...<Widget>[
              const SizedBox(height: SobhSpacing.lg),
              FilledButton(onPressed: onRetry, child: Text(l10n.commonRetry)),
            ],
          ],
        ),
      ),
    );
  }
}

/// SobhEmptyState is shown when a request succeeded and there is genuinely
/// nothing — which is not an error and must not look like one.
class SobhEmptyState extends StatelessWidget {
  const SobhEmptyState({
    super.key,
    required this.icon,
    required this.title,
    this.body,
    this.action,
  });

  final IconData icon;
  final String title;
  final String? body;
  final Widget? action;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;

    return Center(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.xl),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            Icon(
              icon,
              size: SobhSizes.avatarLarge,
              color: palette.textDisabled,
            ),
            const SizedBox(height: SobhSpacing.lg),
            Text(title, textAlign: TextAlign.center, style: text.titleMedium),
            if (body != null) ...<Widget>[
              const SizedBox(height: SobhSpacing.sm),
              Text(
                body!,
                textAlign: TextAlign.center,
                style: text.bodyMedium?.copyWith(color: palette.textSecondary),
              ),
            ],
            if (action != null) ...<Widget>[
              const SizedBox(height: SobhSpacing.lg),
              action!,
            ],
          ],
        ),
      ),
    );
  }
}

/// A circular avatar that falls back to the first letter of a name.
///
/// Media is addressed by id and fetched through the authenticated media
/// endpoint, so until a screen needs real thumbnails this renders the initial
/// rather than a broken image.
class SobhAvatar extends StatelessWidget {
  const SobhAvatar({
    super.key,
    required this.name,
    this.radius = SobhSizes.avatarMedium / 2,
  });

  final String name;
  final double radius;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    final String initial =
        name.trim().isEmpty ? '؟' : name.trim().characters.first;

    return CircleAvatar(
      radius: radius,
      backgroundColor: palette.surfaceVariant,
      child: Text(initial, style: Theme.of(context).textTheme.titleMedium),
    );
  }
}
