import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/profile_repository.dart';

/// Somebody else's profile (§55).
///
/// The client and its provider existed and nothing opened them, so the only
/// profile anyone could read was their own.
///
/// What appears here is whatever the server decided this viewer may see: last
/// seen, the photo and the number are each governed by a privacy rule, and a
/// field the viewer is not entitled to simply does not come back. That is why
/// nothing below is conditional on a rule — the absence *is* the rule, and
/// re-deciding it here would be a second privacy model to keep in step with
/// the real one.
class UserProfileSheet extends ConsumerWidget {
  const UserProfileSheet({super.key, required this.userId});

  final String userId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<UserProfile> profile =
        ref.watch(userProfileProvider(userId));

    return SafeArea(
      child: profile.when(
        loading: () => const Padding(
          padding: EdgeInsets.all(SobhSpacing.xl),
          child: SobhLoading(),
        ),
        error: (Object error, StackTrace _) => Padding(
          padding: const EdgeInsets.all(SobhSpacing.xl),
          child: SobhErrorState(
            error: error,
            onRetry: () => ref.invalidate(userProfileProvider(userId)),
          ),
        ),
        data: (UserProfile user) => Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            const SizedBox(height: SobhSpacing.lg),
            SobhAvatar(name: user.displayName, radius: SobhSizes.avatarLarge / 2),
            const SizedBox(height: SobhSpacing.md),
            Text(
              user.displayName,
              style: Theme.of(context).textTheme.titleMedium,
            ),
            if (user.username != null)
              Text(
                '@${user.username}',
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.primary),
              ),
            if (user.about.isNotEmpty)
              Padding(
                padding: const EdgeInsets.symmetric(
                  horizontal: SobhSpacing.xl,
                  vertical: SobhSpacing.md,
                ),
                child: Text(user.about, textAlign: TextAlign.center),
              ),
            // Absent when the rule says this viewer may not see it, which is
            // why there is no "hidden" state to render. Online now is the
            // sharper form of the same fact, so it displaces the timestamp
            // rather than sitting beside it.
            if (user.isOnline)
              Text(
                l10n.profileOnline,
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.success),
              )
            else if (user.lastSeen != null)
              Text(
                l10n.profileLastSeen(
                  DateFormat.yMd().add_Hm().format(user.lastSeen!),
                ),
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.textSecondary),
              ),
            if (user.isBot)
              Padding(
                padding: const EdgeInsets.only(top: SobhSpacing.sm),
                child: Chip(label: Text(l10n.profileIsBot)),
              ),
            const SizedBox(height: SobhSpacing.xl),
          ],
        ),
      ),
    );
  }
}
