import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/profile_repository.dart';

/// Editing the caller's own profile (§9, §11).
class EditProfileScreen extends ConsumerWidget {
  const EditProfileScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<SelfProfile> profile = ref.watch(selfProfileProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.profileEditTitle)),
      body: profile.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(selfProfileProvider),
        ),
        data: (SelfProfile self) => _EditProfileForm(profile: self),
      ),
    );
  }
}

class _EditProfileForm extends ConsumerStatefulWidget {
  const _EditProfileForm({required this.profile});

  final SelfProfile profile;

  @override
  ConsumerState<_EditProfileForm> createState() => _EditProfileFormState();
}

class _EditProfileFormState extends ConsumerState<_EditProfileForm> {
  late final TextEditingController _name =
      TextEditingController(text: widget.profile.displayName);
  late final TextEditingController _about =
      TextEditingController(text: widget.profile.about);
  late String _language = widget.profile.language;
  bool _saving = false;

  @override
  void dispose() {
    _name.dispose();
    _about.dispose();
    super.dispose();
  }

  bool get _changed =>
      _name.text.trim() != widget.profile.displayName ||
      _about.text != widget.profile.about ||
      _language != widget.profile.language;

  Future<void> _save() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _saving = true);

    try {
      // Only what changed is sent: the server leaves an omitted field alone,
      // so this cannot overwrite a field this build does not know about.
      await ref.read(profileRepositoryProvider).updateProfile(
            displayName: _name.text.trim() == widget.profile.displayName
                ? null
                : _name.text.trim(),
            about: _about.text == widget.profile.about ? null : _about.text,
            language: _language == widget.profile.language ? null : _language,
          );
      if (!mounted) {
        return;
      }
      ref.invalidate(selfProfileProvider);
      _tell(l10n.profileSaved);
      unawaited(Navigator.of(context).maybePop());
    } on ApiException catch (error) {
      if (mounted) {
        _tell(error.isOffline ? l10n.errorNetwork : error.message);
      }
    } finally {
      if (mounted) {
        setState(() => _saving = false);
      }
    }
  }

  void _tell(String message) => ScaffoldMessenger.of(context)
      .showSnackBar(SnackBar(content: Text(message)));

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return ListView(
      padding: const EdgeInsets.all(SobhSpacing.lg),
      children: <Widget>[
        Center(
          child: SobhAvatar(
            name: widget.profile.displayName.isEmpty
                ? l10n.profileTitle
                : widget.profile.displayName,
            radius: SobhSizes.avatarLarge / 2,
          ),
        ),
        const SizedBox(height: SobhSpacing.xl),
        TextField(
          controller: _name,
          textInputAction: TextInputAction.next,
          maxLength: 64,
          decoration: InputDecoration(
            labelText: l10n.profileEditName,
            border: const OutlineInputBorder(),
          ),
          onChanged: (_) => setState(() {}),
        ),
        const SizedBox(height: SobhSpacing.md),
        TextField(
          controller: _about,
          maxLines: 3,
          // Counted in characters, matching the server: a Persian bio gets the
          // same 280 as an English one.
          maxLength: 280,
          decoration: InputDecoration(
            labelText: l10n.profileBio,
            helperText: l10n.profileBioHelp,
            border: const OutlineInputBorder(),
          ),
          onChanged: (_) => setState(() {}),
        ),
        const SizedBox(height: SobhSpacing.md),
        DropdownButtonFormField<String>(
          value: _language,
          decoration: InputDecoration(
            labelText: l10n.settingsLanguage,
            border: const OutlineInputBorder(),
          ),
          items: <DropdownMenuItem<String>>[
            DropdownMenuItem<String>(
              value: 'fa',
              child: Text(l10n.languageFarsi),
            ),
            DropdownMenuItem<String>(
              value: 'en',
              child: Text(l10n.languageEnglish),
            ),
            DropdownMenuItem<String>(
              value: 'tr',
              child: Text(l10n.languageTurkish),
            ),
            DropdownMenuItem<String>(
              value: 'ar',
              child: Text(l10n.languageArabic),
            ),
          ],
          onChanged: (String? value) {
            if (value != null) {
              setState(() => _language = value);
            }
          },
        ),
        const SizedBox(height: SobhSpacing.lg),
        const Divider(),
        ListTile(
          contentPadding: EdgeInsets.zero,
          leading: const Icon(Icons.alternate_email),
          title: Text(l10n.profileUsername),
          subtitle: Text(
            widget.profile.username == null
                ? l10n.profileUsernameNone
                : widget.profile.handle,
            style: TextStyle(
              color: widget.profile.username == null
                  ? palette.textDisabled
                  : palette.textPrimary,
            ),
          ),
          trailing: const Icon(Icons.chevron_right),
          onTap: () async {
            await Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) =>
                    ClaimUsernameScreen(current: widget.profile.username),
              ),
            );
            ref.invalidate(selfProfileProvider);
          },
        ),
        ListTile(
          contentPadding: EdgeInsets.zero,
          leading: const Icon(Icons.phone_outlined),
          title: Text(l10n.profilePhone),
          subtitle: Text(widget.profile.phoneNumber),
        ),
        const SizedBox(height: SobhSpacing.xl),
        FilledButton(
          onPressed: _saving || !_changed ? null : _save,
          child: _saving
              ? const SizedBox(
                  width: SobhSizes.iconSmall,
                  height: SobhSizes.iconSmall,
                  child: CircularProgressIndicator(strokeWidth: 2),
                )
              : Text(l10n.commonSave),
        ),
      ],
    );
  }
}

/// Claiming or changing a username.
///
/// Availability is checked as the user types, but the check is only ever
/// advisory: the claim is what decides, and it can still fail if someone else
/// takes the name in between.
class ClaimUsernameScreen extends ConsumerStatefulWidget {
  const ClaimUsernameScreen({required this.current, super.key});

  final String? current;

  @override
  ConsumerState<ClaimUsernameScreen> createState() =>
      _ClaimUsernameScreenState();
}

class _ClaimUsernameScreenState extends ConsumerState<ClaimUsernameScreen> {
  late final TextEditingController _controller =
      TextEditingController(text: widget.current ?? '');
  Timer? _debounce;
  UsernameAvailability? _availability;
  bool _checking = false;
  bool _claiming = false;

  @override
  void dispose() {
    _debounce?.cancel();
    _controller.dispose();
    super.dispose();
  }

  /// Waits for a pause in typing before asking, so a name is checked once
  /// rather than once per keystroke.
  void _onChanged(String value) {
    _debounce?.cancel();
    setState(() => _availability = null);

    final String candidate = value.trim();
    if (candidate.isEmpty || candidate == widget.current) {
      return;
    }
    _debounce = Timer(SobhDuration.slow, () => _check(candidate));
  }

  Future<void> _check(String candidate) async {
    setState(() => _checking = true);
    try {
      final UsernameAvailability result =
          await ref.read(profileRepositoryProvider).checkUsername(candidate);
      // A slow answer for a name the user has already typed past would be
      // misleading, so it is dropped.
      if (mounted && _controller.text.trim() == candidate) {
        setState(() => _availability = result);
      }
    } on ApiException {
      // Availability is advisory; failing to reach the server is not an error
      // worth interrupting typing for, and the claim will say so properly.
    } finally {
      if (mounted) {
        setState(() => _checking = false);
      }
    }
  }

  Future<void> _claim() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _claiming = true);

    try {
      await ref
          .read(profileRepositoryProvider)
          .claimUsername(_controller.text.trim());
      if (!mounted) {
        return;
      }
      ref.invalidate(selfProfileProvider);
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(l10n.profileUsernameClaimed)));
      unawaited(Navigator.of(context).maybePop());
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      if (mounted) {
        setState(() => _claiming = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    final String candidate = _controller.text.trim();
    final bool canClaim = !_claiming &&
        candidate.isNotEmpty &&
        candidate != widget.current &&
        (_availability?.isAvailable ?? false);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.profileUsername)),
      body: ListView(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        children: <Widget>[
          TextField(
            controller: _controller,
            autofocus: true,
            autocorrect: false,
            // The alphabet is ASCII only, deliberately: a handle is typed from
            // memory and read out of a link, so characters that look alike in
            // different scripts are kept out rather than folded together.
            inputFormatters: <TextInputFormatter>[
              FilteringTextInputFormatter.allow(RegExp('[a-zA-Z0-9_]')),
              LengthLimitingTextInputFormatter(32),
            ],
            decoration: InputDecoration(
              prefixText: '@',
              labelText: l10n.profileUsername,
              helperText: l10n.profileUsernameRules,
              helperMaxLines: 2,
              border: const OutlineInputBorder(),
              suffixIcon: _suffix(palette),
            ),
            onChanged: _onChanged,
          ),
          if (_availability != null && !_availability!.isAvailable)
            Padding(
              padding: const EdgeInsets.only(top: SobhSpacing.sm),
              child: Text(
                switch (_availability!.status) {
                  UsernameStatus.invalid => l10n.profileUsernameInvalid,
                  UsernameStatus.reserved => l10n.profileUsernameReserved,
                  _ => l10n.profileUsernameTaken,
                },
                style: TextStyle(color: palette.error),
              ),
            ),
          const SizedBox(height: SobhSpacing.lg),
          // Renaming is not free, and saying so beforehand is kinder than a
          // surprise when the old name turns out to be unavailable later.
          if (widget.current != null)
            Card(
              child: Padding(
                padding: const EdgeInsets.all(SobhSpacing.md),
                child: Row(
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: <Widget>[
                    Icon(Icons.info_outline, color: palette.textSecondary),
                    const SizedBox(width: SobhSpacing.md),
                    Expanded(
                      child: Text(
                        l10n.profileUsernameReleaseNotice(widget.current!),
                        style: Theme.of(context).textTheme.bodySmall,
                      ),
                    ),
                  ],
                ),
              ),
            ),
          const SizedBox(height: SobhSpacing.lg),
          FilledButton(
            onPressed: canClaim ? _claim : null,
            child: _claiming
                ? const SizedBox(
                    width: SobhSizes.iconSmall,
                    height: SobhSizes.iconSmall,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                : Text(l10n.profileUsernameClaim),
          ),
        ],
      ),
    );
  }

  Widget? _suffix(SobhPalette palette) {
    if (_checking) {
      return const Padding(
        padding: EdgeInsets.all(SobhSpacing.md),
        child: SizedBox(
          width: SobhSizes.iconSmall,
          height: SobhSizes.iconSmall,
          child: CircularProgressIndicator(strokeWidth: 2),
        ),
      );
    }
    if (_availability == null) {
      return null;
    }
    return Icon(
      _availability!.isAvailable
          ? Icons.check_circle_outline
          : Icons.error_outline,
      color: _availability!.isAvailable ? palette.success : palette.error,
    );
  }
}
