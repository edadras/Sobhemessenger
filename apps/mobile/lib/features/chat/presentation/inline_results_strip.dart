import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../data/bot_interaction_repository.dart';

/// Inline mode: asking a bot for something from any chat's compose box (§13).
///
/// The repository for this existed in full — open a query, read the results,
/// send the chosen one — and no screen ever touched it, so a feature the
/// roadmap called done had no way in. Typing `@somebot pizza` did nothing but
/// leave the text sitting there.
///
/// It sits above the composer rather than replacing it, because the query is
/// still being typed while the results are showing, and taking the box away
/// would mean losing the thing that produces them.
class InlineResultsStrip extends ConsumerStatefulWidget {
  const InlineResultsStrip({
    super.key,
    required this.chatId,
    required this.text,
    required this.onChosen,
  });

  final String chatId;

  /// The compose box's current contents. The strip decides for itself whether
  /// this is an inline query.
  final String text;

  /// Called once a result has been sent, so the composer can clear.
  final VoidCallback onChosen;

  /// `@bot rest of the query`. The bot's username has to be complete and
  /// followed by a space: `@so` is somebody halfway through typing a mention,
  /// not a query, and asking the server on every keystroke of a username
  /// would be a request per character.
  static ({String bot, String query})? parse(String text) {
    final RegExpMatch? match =
        RegExp(r'^@([A-Za-z0-9_]{3,})\s+(.*)$').firstMatch(text.trimLeft());
    if (match == null) {
      return null;
    }
    return (bot: match.group(1)!, query: match.group(2)!);
  }

  @override
  ConsumerState<InlineResultsStrip> createState() => _InlineResultsStripState();
}

class _InlineResultsStripState extends ConsumerState<InlineResultsStrip> {
  Timer? _debounce;
  String? _queryId;
  List<InlineResult> _results = const <InlineResult>[];
  bool _loading = false;

  /// Set when the bot refused or does not do inline mode, so the strip stops
  /// asking rather than retrying on every keystroke.
  bool _unavailable = false;

  /// The query text the current results belong to, so a stale answer arriving
  /// after the user has typed on is discarded.
  String _showing = '';

  @override
  void didUpdateWidget(InlineResultsStrip old) {
    super.didUpdateWidget(old);
    if (old.text != widget.text) {
      _unavailable = false;
      _schedule();
    }
  }

  @override
  void dispose() {
    _debounce?.cancel();
    super.dispose();
  }

  void _schedule() {
    _debounce?.cancel();
    _debounce = Timer(SobhDuration.slow, _run);
  }

  Future<void> _run() async {
    final ({String bot, String query})? parsed =
        InlineResultsStrip.parse(widget.text);
    if (parsed == null || _unavailable) {
      if (_results.isNotEmpty && mounted) {
        setState(() {
          _results = const <InlineResult>[];
          _queryId = null;
        });
      }
      return;
    }

    final String asked = widget.text;
    setState(() => _loading = true);
    try {
      final BotInteractionRepository bots =
          ref.read(botInteractionRepositoryProvider);
      final InlineQuery query =
          await bots.openQuery(bot: parsed.bot, query: parsed.query);
      final List<InlineResult> results = await bots.results(query.id);
      // The user may have typed on while this was in flight; showing answers
      // to a question they have already changed is worse than showing none.
      if (!mounted || widget.text != asked) {
        return;
      }
      setState(() {
        _queryId = query.id;
        _results = results;
        _showing = asked;
      });
    } on ApiException {
      // No such bot, or one that does not answer inline queries. Neither is a
      // failure worth an error over somebody's typing.
      if (mounted) {
        setState(() {
          _unavailable = true;
          _results = const <InlineResult>[];
          _queryId = null;
        });
      }
    } finally {
      if (mounted) {
        setState(() => _loading = false);
      }
    }
  }

  Future<void> _choose(InlineResult result) async {
    final String? queryId = _queryId;
    if (queryId == null) {
      return;
    }

    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    try {
      await ref.read(botInteractionRepositoryProvider).choose(
            queryId: queryId,
            chatId: widget.chatId,
            resultId: result.id,
          );
      if (mounted) {
        setState(() {
          _results = const <InlineResult>[];
          _queryId = null;
        });
      }
      widget.onChosen();
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

    if (InlineResultsStrip.parse(widget.text) == null || _unavailable) {
      return const SizedBox.shrink();
    }
    if (_results.isEmpty) {
      // A bar rather than nothing, so it is visible that the bot is being
      // asked — otherwise typing a query looks like typing ordinary text.
      return Container(
        height: SobhSizes.iconLarge + SobhSpacing.md,
        alignment: AlignmentDirectional.centerStart,
        padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.lg),
        color: palette.surfaceVariant,
        child: Row(
          children: <Widget>[
            if (_loading)
              const SizedBox(
                height: SobhSizes.iconSmall,
                width: SobhSizes.iconSmall,
                child: CircularProgressIndicator(strokeWidth: 2),
              ),
            if (_loading) const SizedBox(width: SobhSpacing.md),
            Text(
              _loading ? l10n.inlineAsking : l10n.inlineNoResults,
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            ),
          ],
        ),
      );
    }

    return Container(
      constraints: const BoxConstraints(maxHeight: 220),
      color: palette.surfaceVariant,
      child: ListView.separated(
        shrinkWrap: true,
        itemCount: _results.length,
        separatorBuilder: (_, __) => const Divider(height: 1),
        itemBuilder: (BuildContext context, int index) {
          final InlineResult result = _results[index];
          return ListTile(
            dense: true,
            // The kind of thing offered, rather than a preview of it: the
            // strip is a list of choices, and fetching a thumbnail per result
            // for a query that changes on every keystroke would be a great
            // deal of traffic for a picture nobody looks at twice.
            leading: Icon(
              switch (result.type) {
                'photo' || 'image' => Icons.image_outlined,
                'video' => Icons.videocam_outlined,
                'audio' => Icons.audiotrack_outlined,
                'document' => Icons.description_outlined,
                _ => Icons.smart_toy_outlined,
              },
            ),
            title: Text(
              result.title,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
            ),
            subtitle: result.description.isEmpty
                ? null
                : Text(
                    result.description,
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                  ),
            // The message is sent by the person, not the bot, so it lands only
            // where they could have posted it themselves.
            onTap: () => _choose(result),
          );
        },
      ),
    );
  }

  /// The query these results answer, used by the chat screen to decide whether
  /// the strip is worth keeping on screen.
  String get showing => _showing;
}
