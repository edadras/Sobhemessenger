import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/polls_repository.dart';

/// A poll inside a message bubble (§17).
class PollView extends ConsumerStatefulWidget {
  const PollView({super.key, required this.pollId});

  final String pollId;

  @override
  ConsumerState<PollView> createState() => _PollViewState();
}

class _PollViewState extends ConsumerState<PollView> {
  /// The options selected but not yet submitted, for a multiple-choice poll.
  final Set<String> _pending = <String>{};
  bool _submitting = false;

  Future<void> _submit(Poll poll, List<String> optionIds) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _submitting = true);
    try {
      await ref.read(pollsRepositoryProvider).vote(poll.id, optionIds);
      ref.invalidate(pollProvider(poll.id));
      _pending.clear();
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
    final AsyncValue<Poll> poll = ref.watch(pollProvider(widget.pollId));

    return poll.when(
      loading: () => const SobhLoading(),
      error: (Object error, StackTrace _) => SobhErrorState(
        error: error,
        onRetry: () => ref.invalidate(pollProvider(widget.pollId)),
      ),
      data: (Poll data) => Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Text(data.question, style: Theme.of(context).textTheme.titleSmall),
          const SizedBox(height: SobhSpacing.sm),
          for (final PollOption option in data.options)
            _PollOptionRow(
              poll: data,
              option: option,
              selected: _pending.contains(option.id) ||
                  data.myVotes.contains(option.id),
              enabled: !data.isClosed && !_submitting,
              // Who chose an option is only knowable once the results are
              // visible, and only when the poll is not anonymous — that is
              // exactly the promise an anonymous poll makes, so the row does
              // not offer it rather than letting the server refuse.
              onShowVoters: !data.isAnonymous &&
                      data.showsResults &&
                      option.voteCount > 0
                  ? () => Navigator.of(context).push(
                        MaterialPageRoute<void>(
                          builder: (_) => PollVotersScreen(
                            pollId: data.id,
                            option: option,
                          ),
                        ),
                      )
                  : null,
              onTap: () {
                if (data.allowsMultiple) {
                  setState(() {
                    _pending.contains(option.id)
                        ? _pending.remove(option.id)
                        : _pending.add(option.id);
                  });
                } else {
                  // A single-choice vote is submitted on tap: there is nothing
                  // to confirm.
                  _submit(data, <String>[option.id]);
                }
              },
            ),
          const SizedBox(height: SobhSpacing.sm),
          Row(
            children: <Widget>[
              Text(
                l10n.pollsVotes(data.totalVoters),
                style: Theme.of(context).textTheme.labelSmall,
              ),
              const Spacer(),
              if (data.allowsMultiple && _pending.isNotEmpty && !_submitting)
                TextButton(
                  onPressed: () => _submit(data, _pending.toList()),
                  child: Text(l10n.commonDone),
                ),
              if (data.hasVoted && !data.isClosed && _pending.isEmpty)
                TextButton(
                  onPressed: _submitting
                      ? null
                      : () => _submit(data, const <String>[]),
                  child: Text(l10n.pollsRetract),
                ),
            ],
          ),
        ],
      ),
    );
  }
}

class _PollOptionRow extends StatelessWidget {
  const _PollOptionRow({
    required this.poll,
    required this.option,
    required this.selected,
    required this.enabled,
    required this.onTap,
    this.onShowVoters,
  });

  final Poll poll;
  final PollOption option;
  final bool selected;
  final bool enabled;
  final VoidCallback onTap;

  /// Null when this poll cannot say who voted — anonymous, or results still
  /// hidden.
  final VoidCallback? onShowVoters;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    // In a quiz the correct answer is revealed once the viewer has answered.
    final bool isCorrect = poll.isQuiz && poll.correctOption == option.position;
    final Color barColor =
        poll.showsResults && isCorrect ? palette.success : palette.primary;

    return InkWell(
      onTap: enabled ? onTap : null,
      child: Padding(
        padding: const EdgeInsets.symmetric(vertical: SobhSpacing.xs),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: <Widget>[
            Row(
              children: <Widget>[
                Icon(
                  selected
                      ? (poll.allowsMultiple
                          ? Icons.check_box
                          : Icons.radio_button_checked)
                      : (poll.allowsMultiple
                          ? Icons.check_box_outline_blank
                          : Icons.radio_button_unchecked),
                  size: SobhSizes.iconSmall,
                  color: selected ? palette.primary : palette.textDisabled,
                ),
                const SizedBox(width: SobhSpacing.sm),
                Expanded(child: Text(option.text)),
                if (onShowVoters != null)
                  IconButton(
                    onPressed: onShowVoters,
                    icon: const Icon(Icons.people_outline),
                    visualDensity: VisualDensity.compact,
                    tooltip: AppLocalizations.of(context).pollsVotersTitle,
                  ),
                if (poll.showsResults)
                  Text(
                    '${(poll.shareOf(option) * 100).round()}%',
                    style: Theme.of(context).textTheme.labelSmall,
                  ),
              ],
            ),
            if (poll.showsResults) ...<Widget>[
              const SizedBox(height: SobhSpacing.xxs),
              ClipRRect(
                borderRadius: BorderRadius.circular(SobhRadius.sm),
                child: LinearProgressIndicator(
                  value: poll.shareOf(option),
                  backgroundColor: palette.surfaceVariant,
                  valueColor: AlwaysStoppedAnimation<Color>(barColor),
                ),
              ),
            ],
          ],
        ),
      ),
    );
  }
}

/// Creates a poll in a chat (§17).
class CreatePollScreen extends ConsumerStatefulWidget {
  const CreatePollScreen({super.key, required this.chatId});

  final String chatId;

  @override
  ConsumerState<CreatePollScreen> createState() => _CreatePollScreenState();
}

class _CreatePollScreenState extends ConsumerState<CreatePollScreen> {
  final TextEditingController _question = TextEditingController();
  final List<TextEditingController> _options = <TextEditingController>[
    TextEditingController(),
    TextEditingController(),
  ];

  bool _anonymous = false;
  bool _multiple = false;
  bool _quiz = false;
  bool _submitting = false;
  String? _error;

  @override
  void dispose() {
    _question.dispose();
    for (final TextEditingController controller in _options) {
      controller.dispose();
    }
    super.dispose();
  }

  List<String> get _filledOptions => <String>[
        for (final TextEditingController controller in _options)
          if (controller.text.trim().isNotEmpty) controller.text.trim(),
      ];

  Future<void> _create() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() {
      _submitting = true;
      _error = null;
    });

    try {
      await ref.read(pollsRepositoryProvider).create(
            chatId: widget.chatId,
            question: _question.text.trim(),
            options: _filledOptions,
            isAnonymous: _anonymous,
            allowsMultiple: _multiple,
            isQuiz: _quiz,
            // In a quiz the first option is the correct one until the author
            // says otherwise; the server validates the index.
            correctOption: _quiz ? 0 : null,
          );
      if (mounted) {
        Navigator.of(context).pop();
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(
          () => _error = error.isOffline ? l10n.errorNetwork : error.message,
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
    final SobhPalette palette = SobhTheme.of(context);
    final bool canCreate = !_submitting &&
        _question.text.trim().isNotEmpty &&
        _filledOptions.length >= 2;

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.pollsCreate),
        actions: <Widget>[
          TextButton(
            onPressed: canCreate ? _create : null,
            child: Text(l10n.commonDone),
          ),
        ],
      ),
      body: ListView(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        children: <Widget>[
          TextField(
            controller: _question,
            decoration: InputDecoration(labelText: l10n.pollsQuestionHint),
            onChanged: (_) => setState(() {}),
          ),
          const SizedBox(height: SobhSpacing.lg),
          for (int i = 0; i < _options.length; i++)
            Padding(
              padding: const EdgeInsets.only(bottom: SobhSpacing.sm),
              child: TextField(
                controller: _options[i],
                decoration: InputDecoration(
                  labelText: '${l10n.pollsOptionHint} ${i + 1}',
                ),
                onChanged: (_) => setState(() {}),
              ),
            ),
          // Ten is the server's limit, so the button disappears rather than
          // offering an option the server would reject.
          if (_options.length < 10)
            TextButton.icon(
              onPressed: () =>
                  setState(() => _options.add(TextEditingController())),
              icon: const Icon(Icons.add),
              label: Text(l10n.pollsAddOption),
            ),
          const Divider(),
          SwitchListTile(
            value: _multiple,
            onChanged: _quiz
                // A quiz has one correct answer, so multiple choice is
                // meaningless and the toggle is disabled rather than ignored.
                ? null
                : (bool value) => setState(() => _multiple = value),
            title: Text(l10n.pollsMultiple),
          ),
          SwitchListTile(
            value: _anonymous,
            onChanged: (bool value) => setState(() => _anonymous = value),
            title: Text(l10n.pollsAnonymous),
          ),
          SwitchListTile(
            value: _quiz,
            onChanged: (bool value) => setState(() {
              _quiz = value;
              if (value) {
                _multiple = false;
              }
            }),
            title: Text(l10n.pollsQuiz),
          ),
          if (_error != null)
            Padding(
              padding: const EdgeInsets.only(top: SobhSpacing.lg),
              child: Text(_error!, style: TextStyle(color: palette.error)),
            ),
        ],
      ),
    );
  }
}

/// Who chose one option (§18).
///
/// Reached only from a poll that is not anonymous. The list is the server's
/// answer verbatim — including the order, which is the order people voted in,
/// because "who was first" is part of what a visible poll is for.
class PollVotersScreen extends ConsumerWidget {
  const PollVotersScreen({
    super.key,
    required this.pollId,
    required this.option,
  });

  final String pollId;
  final PollOption option;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<PollVoter>> voters =
        ref.watch(pollVotersProvider((pollId, option.id)));

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.pollsVotersTitle),
        bottom: PreferredSize(
          preferredSize: const Size.fromHeight(28),
          child: Padding(
            padding: const EdgeInsets.only(
              bottom: SobhSpacing.sm,
              left: SobhSpacing.lg,
              right: SobhSpacing.lg,
            ),
            child: Align(
              alignment: AlignmentDirectional.centerStart,
              child: Text(
                option.text,
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.textSecondary),
              ),
            ),
          ),
        ),
      ),
      body: voters.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () =>
              ref.invalidate(pollVotersProvider((pollId, option.id))),
        ),
        data: (List<PollVoter> rows) {
          if (rows.isEmpty) {
            return SobhEmptyState(
              icon: Icons.how_to_vote_outlined,
              title: l10n.pollsVotersEmpty,
            );
          }
          return ListView.separated(
            itemCount: rows.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) {
              final PollVoter voter = rows[index];
              return ListTile(
                leading: SobhAvatar(name: voter.displayName),
                title: Text(voter.displayName),
                subtitle: voter.username == null
                    ? null
                    : Text('@${voter.username}'),
                trailing: Text(
                  DateFormat.Hm().format(voter.votedAt),
                  style: TextStyle(color: palette.textSecondary),
                ),
              );
            },
          );
        },
      ),
    );
  }
}
