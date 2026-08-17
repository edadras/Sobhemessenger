import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/bots_repository.dart';
import 'bot_detail_screen.dart';

/// The bots the caller owns (§13).
class BotsScreen extends ConsumerWidget {
  const BotsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Bot>> bots = ref.watch(botListProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.botsTitle)),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => _register(context, ref),
        icon: const Icon(Icons.add),
        label: Text(l10n.botsRegister),
      ),
      body: bots.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(botListProvider),
        ),
        data: (List<Bot> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.smart_toy_outlined,
                title: l10n.botsEmptyTitle,
                body: l10n.botsEmptyMessage,
              )
            : RefreshIndicator(
                onRefresh: () async => ref.invalidate(botListProvider),
                child: ListView.builder(
                  itemCount: rows.length,
                  itemBuilder: (BuildContext context, int index) {
                    final Bot bot = rows[index];
                    return ListTile(
                      leading: SobhAvatar(name: bot.displayName),
                      title: Text(bot.displayName),
                      subtitle: Text(bot.handle),
                      trailing: bot.isActive
                          ? null
                          : Chip(
                              label: Text(l10n.botsInactive),
                              visualDensity: VisualDensity.compact,
                            ),
                      onTap: () => Navigator.of(context).push(
                        MaterialPageRoute<void>(
                          builder: (_) => BotDetailScreen(botId: bot.userId),
                        ),
                      ),
                    );
                  },
                ),
              ),
      ),
    );
  }

  Future<void> _register(BuildContext context, WidgetRef ref) async {
    final (Bot, IssuedToken)? created = await Navigator.of(context).push(
      MaterialPageRoute<(Bot, IssuedToken)>(
        builder: (_) => const RegisterBotScreen(),
      ),
    );
    if (created == null || !context.mounted) {
      return;
    }
    ref.invalidate(botListProvider);
    await showTokenOnce(context, created.$2);
  }
}

/// Registering a bot.
class RegisterBotScreen extends ConsumerStatefulWidget {
  const RegisterBotScreen({super.key});

  @override
  ConsumerState<RegisterBotScreen> createState() => _RegisterBotScreenState();
}

class _RegisterBotScreenState extends ConsumerState<RegisterBotScreen> {
  final TextEditingController _name = TextEditingController();
  final TextEditingController _username = TextEditingController();
  final TextEditingController _description = TextEditingController();
  bool _submitting = false;

  @override
  void dispose() {
    _name.dispose();
    _username.dispose();
    _description.dispose();
    super.dispose();
  }

  Future<void> _submit() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _submitting = true);

    try {
      final (Bot, IssuedToken) created =
          await ref.read(botsRepositoryProvider).register(
                username: _username.text.trim(),
                displayName: _name.text.trim(),
                description: _description.text.trim(),
              );
      if (mounted) {
        Navigator.of(context).pop(created);
      }
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
        setState(() => _submitting = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final String username = _username.text.trim();
    final bool valid = _name.text.trim().isNotEmpty &&
        username.length >= 5 &&
        username.endsWith('bot');

    return Scaffold(
      appBar: AppBar(title: Text(l10n.botsRegister)),
      body: ListView(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        children: <Widget>[
          TextField(
            controller: _name,
            textInputAction: TextInputAction.next,
            maxLength: 64,
            decoration: InputDecoration(
              labelText: l10n.botsName,
              border: const OutlineInputBorder(),
            ),
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: SobhSpacing.md),
          TextField(
            controller: _username,
            autocorrect: false,
            inputFormatters: <TextInputFormatter>[
              FilteringTextInputFormatter.allow(RegExp('[a-z0-9_]')),
              LengthLimitingTextInputFormatter(32),
            ],
            decoration: InputDecoration(
              prefixText: '@',
              labelText: l10n.profileUsername,
              // The suffix is not decoration: it is how a person can tell from
              // the handle alone that there is no human on the other end.
              helperText: l10n.botsUsernameRules,
              helperMaxLines: 2,
              border: const OutlineInputBorder(),
            ),
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: SobhSpacing.md),
          TextField(
            controller: _description,
            maxLines: 3,
            maxLength: 512,
            decoration: InputDecoration(
              labelText: l10n.botsDescription,
              border: const OutlineInputBorder(),
            ),
          ),
          const SizedBox(height: SobhSpacing.lg),
          FilledButton(
            onPressed: _submitting || !valid ? null : _submit,
            child: _submitting
                ? const SizedBox(
                    width: SobhSizes.iconSmall,
                    height: SobhSizes.iconSmall,
                    child: CircularProgressIndicator(strokeWidth: 2),
                  )
                : Text(l10n.botsRegister),
          ),
        ],
      ),
    );
  }
}

/// Shows a freshly issued token, once.
///
/// The server keeps only a hash, so this really is the only time it can be
/// read. The dialog therefore cannot be dismissed by tapping outside it: losing
/// the token to a stray tap means issuing another one.
Future<void> showTokenOnce(BuildContext context, IssuedToken token) {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final SobhPalette palette = SobhTheme.of(context);

  return showDialog<void>(
    context: context,
    barrierDismissible: false,
    builder: (BuildContext context) => AlertDialog(
      title: Text(l10n.botsTokenTitle),
      content: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: <Widget>[
          Text(l10n.botsTokenOnce, style: TextStyle(color: palette.error)),
          const SizedBox(height: SobhSpacing.md),
          SelectableText(
            token.secret,
            style: const TextStyle(fontFamily: 'monospace'),
          ),
        ],
      ),
      actions: <Widget>[
        TextButton.icon(
          onPressed: () async {
            await Clipboard.setData(ClipboardData(text: token.secret));
            if (context.mounted) {
              ScaffoldMessenger.of(context).showSnackBar(
                SnackBar(content: Text(l10n.botsTokenCopied)),
              );
            }
          },
          icon: const Icon(Icons.copy),
          label: Text(l10n.commonCopy),
        ),
        FilledButton(
          onPressed: () => Navigator.of(context).pop(),
          child: Text(l10n.commonDone),
        ),
      ],
    ),
  );
}
