import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../data/calls_repository.dart';

/// Call history (§18).
class CallsScreen extends ConsumerWidget {
  const CallsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Call>> history = ref.watch(callHistoryProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.callsTitle)),
      body: history.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(callHistoryProvider),
        ),
        data: (List<Call> calls) => calls.isEmpty
            ? SobhEmptyState(icon: Icons.call_outlined, title: l10n.callsEmpty)
            : RefreshIndicator(
                onRefresh: () async => ref.invalidate(callHistoryProvider),
                child: ListView.separated(
                  itemCount: calls.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (BuildContext context, int index) =>
                      _CallTile(call: calls[index]),
                ),
              ),
      ),
    );
  }
}

class _CallTile extends ConsumerWidget {
  const _CallTile({required this.call});

  final Call call;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final String? me = ref.watch(sessionControllerProvider).userId;

    final bool outgoing = call.initiatorId == me;
    final bool missed = call.wasMissed && !outgoing;

    final String direction = missed
        ? l10n.callsMissed
        : outgoing
            ? l10n.callsOutgoing
            : l10n.callsIncoming;

    // The peer is whoever is not the current user; a group call falls back to
    // its participant count rather than picking one arbitrarily.
    final Iterable<CallParticipant> others =
        call.participants.where((CallParticipant p) => p.userId != me);
    final String title = others.length == 1
        ? others.first.displayName
        : others.map((CallParticipant p) => p.displayName).take(3).join('، ');

    return ListTile(
      leading: SobhAvatar(name: title),
      title: Text(
        title.isEmpty ? direction : title,
        maxLines: 1,
        overflow: TextOverflow.ellipsis,
      ),
      subtitle: Row(
        children: <Widget>[
          Icon(
            missed
                ? Icons.call_missed
                : outgoing
                    ? Icons.call_made
                    : Icons.call_received,
            size: SobhSizes.iconSmall,
            color: missed ? palette.error : palette.textSecondary,
          ),
          const SizedBox(width: SobhSpacing.xs),
          Expanded(
            child: Text(
              <String>[
                direction,
                DateFormat.yMd().add_Hm().format(call.startedAt),
                if (call.durationSeconds != null)
                  _formatDuration(call.durationSeconds!),
              ].join(' · '),
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: Theme.of(context).textTheme.bodySmall?.copyWith(
                    color: missed ? palette.error : palette.textSecondary,
                  ),
            ),
          ),
        ],
      ),
      trailing: Icon(
        call.isVideo ? Icons.videocam_outlined : Icons.call_outlined,
        color: palette.primary,
      ),
    );
  }

  static String _formatDuration(int seconds) {
    final Duration duration = Duration(seconds: seconds);
    final String minutes =
        duration.inMinutes.remainder(60).toString().padLeft(2, '0');
    final String remaining =
        duration.inSeconds.remainder(60).toString().padLeft(2, '0');
    return duration.inHours > 0
        ? '${duration.inHours}:$minutes:$remaining'
        : '$minutes:$remaining';
  }
}
