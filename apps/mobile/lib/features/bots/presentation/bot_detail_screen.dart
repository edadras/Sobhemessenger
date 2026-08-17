import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/bots_repository.dart';
import 'bots_screen.dart';

/// One bot: its settings, its tokens and its webhook (§13).
class BotDetailScreen extends ConsumerWidget {
  const BotDetailScreen({required this.botId, super.key});

  final String botId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Bot>> bots = ref.watch(botListProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.botsTitle)),
      body: bots.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(botListProvider),
        ),
        data: (List<Bot> rows) {
          final Iterable<Bot> match =
              rows.where((Bot bot) => bot.userId == botId);
          if (match.isEmpty) {
            return SobhEmptyState(
              icon: Icons.smart_toy_outlined,
              title: l10n.botsEmptyTitle,
              body: l10n.botsEmptyMessage,
            );
          }
          return _BotDetail(bot: match.first);
        },
      ),
    );
  }
}

class _BotDetail extends ConsumerWidget {
  const _BotDetail({required this.bot});

  final Bot bot;

  Future<void> _set(
    BuildContext context,
    WidgetRef ref, {
    bool? canJoinGroups,
    bool? privacyMode,
    bool? inlineEnabled,
    bool? isActive,
  }) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    try {
      await ref.read(botsRepositoryProvider).updateSettings(
            bot.userId,
            canJoinGroups: canJoinGroups,
            privacyMode: privacyMode,
            inlineEnabled: inlineEnabled,
            isActive: isActive,
          );
      ref.invalidate(botListProvider);
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    }
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<BotToken>> tokens =
        ref.watch(botTokensProvider(bot.userId));
    final AsyncValue<BotWebhook?> webhook =
        ref.watch(botWebhookProvider(bot.userId));

    return ListView(
      children: <Widget>[
        const SizedBox(height: SobhSpacing.lg),
        Center(
          child: Column(
            children: <Widget>[
              SobhAvatar(
                name: bot.displayName,
                radius: SobhSizes.avatarLarge / 2,
              ),
              const SizedBox(height: SobhSpacing.md),
              Text(
                bot.displayName,
                style: Theme.of(context).textTheme.titleLarge,
              ),
              Text(
                bot.handle,
                style: TextStyle(color: palette.textSecondary),
              ),
              if (bot.description.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.symmetric(
                    horizontal: SobhSpacing.xl,
                    vertical: SobhSpacing.sm,
                  ),
                  child: Text(bot.description, textAlign: TextAlign.center),
                ),
            ],
          ),
        ),
        const SizedBox(height: SobhSpacing.lg),
        const Divider(),
        _SectionHeader(title: l10n.botsSettings),
        SwitchListTile(
          value: bot.privacyMode,
          title: Text(l10n.botsPrivacyMode),
          subtitle: Text(l10n.botsPrivacyModeHelp),
          onChanged: (bool value) => _set(context, ref, privacyMode: value),
        ),
        SwitchListTile(
          value: bot.canJoinGroups,
          title: Text(l10n.botsCanJoinGroups),
          onChanged: (bool value) => _set(context, ref, canJoinGroups: value),
        ),
        SwitchListTile(
          value: bot.isActive,
          title: Text(l10n.botsActive),
          subtitle: Text(l10n.botsActiveHelp),
          onChanged: (bool value) => _set(context, ref, isActive: value),
        ),
        const Divider(),
        _SectionHeader(
          title: l10n.botsTokens,
          action: TextButton.icon(
            onPressed: () => _issueToken(context, ref),
            icon: const Icon(Icons.add),
            label: Text(l10n.botsIssueToken),
          ),
        ),
        tokens.when(
          loading: () => const Padding(
            padding: EdgeInsets.all(SobhSpacing.lg),
            child: SobhLoading(),
          ),
          error: (Object error, StackTrace _) => Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child:
                Text(l10n.errorGeneric, style: TextStyle(color: palette.error)),
          ),
          data: (List<BotToken> rows) => Column(
            children: <Widget>[
              for (final BotToken token in rows)
                ListTile(
                  leading: const Icon(Icons.key_outlined),
                  title: Text(
                    // Only the prefix is stored in the clear, which is enough
                    // to tell two tokens apart and useless as a credential.
                    '${token.prefix}…',
                    style: const TextStyle(fontFamily: 'monospace'),
                  ),
                  subtitle: Text(
                    token.label.isEmpty ? l10n.botsTokenNoLabel : token.label,
                  ),
                  trailing: IconButton(
                    tooltip: l10n.botsRevokeToken,
                    icon: Icon(Icons.delete_outline, color: palette.error),
                    onPressed: () => _revoke(context, ref, token),
                  ),
                ),
              if (rows.isEmpty)
                Padding(
                  padding: const EdgeInsets.all(SobhSpacing.lg),
                  child: Text(
                    l10n.botsNoTokens,
                    style: TextStyle(color: palette.textSecondary),
                  ),
                ),
            ],
          ),
        ),
        const Divider(),
        _SectionHeader(title: l10n.botsWebhook),
        webhook.when(
          loading: () => const Padding(
            padding: EdgeInsets.all(SobhSpacing.lg),
            child: SobhLoading(),
          ),
          error: (Object error, StackTrace _) => Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child:
                Text(l10n.errorGeneric, style: TextStyle(color: palette.error)),
          ),
          data: (BotWebhook? registration) => Column(
            children: <Widget>[
              if (registration == null)
                ListTile(
                  subtitle: Text(l10n.botsWebhookNoneHelp),
                  title: Text(l10n.botsWebhookNone),
                )
              else ...<Widget>[
                ListTile(
                  leading: const Icon(Icons.link),
                  title: Text(registration.url),
                  subtitle: registration.lastError.isEmpty
                      ? null
                      : Text(
                          registration.lastError,
                          style: TextStyle(color: palette.error),
                        ),
                  trailing: IconButton(
                    tooltip: l10n.botsWebhookRemove,
                    icon: Icon(Icons.delete_outline, color: palette.error),
                    onPressed: () async {
                      await ref
                          .read(botsRepositoryProvider)
                          .deleteWebhook(bot.userId);
                      ref.invalidate(botWebhookProvider(bot.userId));
                    },
                  ),
                ),
              ],
              Padding(
                padding: const EdgeInsets.symmetric(
                  horizontal: SobhSpacing.lg,
                  vertical: SobhSpacing.sm,
                ),
                child: OutlinedButton.icon(
                  onPressed: () => _setWebhook(context, ref),
                  icon: const Icon(Icons.edit_outlined),
                  label: Text(l10n.botsWebhookSet),
                ),
              ),
            ],
          ),
        ),
        const SizedBox(height: SobhSpacing.xxl),
      ],
    );
  }

  Future<void> _issueToken(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TextEditingController label = TextEditingController();

    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.botsIssueToken),
            content: TextField(
              controller: label,
              autofocus: true,
              maxLength: 64,
              decoration: InputDecoration(
                labelText: l10n.botsTokenLabel,
                helperText: l10n.botsTokenLabelHelp,
              ),
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.commonAdd),
              ),
            ],
          ),
        ) ??
        false;

    if (!confirmed || !context.mounted) {
      label.dispose();
      return;
    }

    try {
      final IssuedToken issued = await ref
          .read(botsRepositoryProvider)
          .issueToken(bot.userId, label: label.text.trim());
      ref.invalidate(botTokensProvider(bot.userId));
      if (context.mounted) {
        await showTokenOnce(context, issued);
      }
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      label.dispose();
    }
  }

  Future<void> _revoke(
    BuildContext context,
    WidgetRef ref,
    BotToken token,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);

    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            content: Text(l10n.botsRevokeTokenConfirm),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.botsRevokeToken),
              ),
            ],
          ),
        ) ??
        false;
    if (!confirmed) {
      return;
    }

    await ref.read(botsRepositoryProvider).revokeToken(bot.userId, token.id);
    ref.invalidate(botTokensProvider(bot.userId));
  }

  Future<void> _setWebhook(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TextEditingController url = TextEditingController();

    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.botsWebhookSet),
            content: TextField(
              controller: url,
              autofocus: true,
              keyboardType: TextInputType.url,
              decoration: InputDecoration(
                labelText: l10n.botsWebhookUrl,
                // https and a public address are both server-enforced; saying
                // so here saves a round trip to find out.
                helperText: l10n.botsWebhookUrlHelp,
                helperMaxLines: 3,
              ),
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.commonSave),
              ),
            ],
          ),
        ) ??
        false;

    if (!confirmed || !context.mounted) {
      url.dispose();
      return;
    }

    try {
      final String secret = await ref
          .read(botsRepositoryProvider)
          .setWebhook(bot.userId, url: url.text.trim());
      ref.invalidate(botWebhookProvider(bot.userId));
      if (context.mounted) {
        await showDialog<void>(
          context: context,
          barrierDismissible: false,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.botsWebhookSecretTitle),
            content: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: <Widget>[
                Text(l10n.botsWebhookSecretHelp),
                const SizedBox(height: SobhSpacing.md),
                SelectableText(
                  secret,
                  style: const TextStyle(fontFamily: 'monospace'),
                ),
              ],
            ),
            actions: <Widget>[
              FilledButton(
                onPressed: () => Navigator.of(context).pop(),
                child: Text(l10n.commonDone),
              ),
            ],
          ),
        );
      }
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      url.dispose();
    }
  }
}

class _SectionHeader extends StatelessWidget {
  const _SectionHeader({required this.title, this.action});

  final String title;
  final Widget? action;

  @override
  Widget build(BuildContext context) => Padding(
        padding: const EdgeInsets.fromLTRB(
          SobhSpacing.lg,
          SobhSpacing.lg,
          SobhSpacing.sm,
          SobhSpacing.sm,
        ),
        child: Row(
          children: <Widget>[
            Expanded(
              child: Text(
                title,
                style: Theme.of(context).textTheme.titleSmall?.copyWith(
                      color: SobhTheme.of(context).textSecondary,
                    ),
              ),
            ),
            if (action != null) action!,
          ],
        ),
      );
}
