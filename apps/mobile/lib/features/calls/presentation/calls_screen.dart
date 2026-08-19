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
      // Opening a past call shows what each leg negotiated. It is the answer
      // to "why was that one so bad?", and afterwards is the only time anyone
      // asks — which is why the record is kept rather than only logged.
      onTap: () => showModalBottomSheet<void>(
        context: context,
        isScrollControlled: true,
        builder: (_) => CallDetailsSheet(call: call, title: title),
      ),
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

/// What each side of a past call negotiated (§21).
///
/// Deliberately plain. It is a diagnostic, read by somebody who has just had a
/// bad call, so it answers the two questions they actually have — who was on
/// it and over what, and did the two sides ever manage to agree on a route —
/// rather than presenting a protocol trace.
class CallDetailsSheet extends ConsumerWidget {
  const CallDetailsSheet({
    super.key,
    required this.call,
    required this.title,
  });

  final Call call;
  final String title;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final String? me = ref.watch(sessionControllerProvider).userId;
    final AsyncValue<List<CallSession>> sessions =
        ref.watch(callSessionsProvider(call.id));

    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: <Widget>[
            Text(
              l10n.callDetailsTitle,
              style: Theme.of(context).textTheme.titleMedium,
            ),
            Text(
              title,
              style: Theme.of(context)
                  .textTheme
                  .bodySmall
                  ?.copyWith(color: palette.textSecondary),
            ),
            const SizedBox(height: SobhSpacing.lg),
            sessions.when(
              loading: () => const Padding(
                padding: EdgeInsets.all(SobhSpacing.lg),
                child: SobhLoading(),
              ),
              error: (Object error, StackTrace _) => SobhErrorState(
                error: error,
                onRetry: () => ref.invalidate(callSessionsProvider(call.id)),
              ),
              data: (List<CallSession> legs) {
                if (legs.isEmpty) {
                  return Text(
                    l10n.callDetailsNothingRecorded,
                    style: TextStyle(color: palette.textSecondary),
                  );
                }
                return Column(
                  mainAxisSize: MainAxisSize.min,
                  children: <Widget>[
                    for (final CallSession leg in legs)
                      ListTile(
                        contentPadding: EdgeInsets.zero,
                        leading: Icon(
                          leg.negotiated
                              ? Icons.check_circle_outline
                              : Icons.error_outline,
                          color: leg.negotiated
                              ? palette.success
                              : palette.error,
                        ),
                        title: Text(
                          leg.userId == me
                              ? l10n.callDetailsThisDevice
                              : _nameOf(leg.userId, l10n),
                        ),
                        subtitle: Text(
                          <String>[
                            switch (leg.networkType) {
                              'wifi' => l10n.callDetailsWifi,
                              'cellular' => l10n.callDetailsCellular,
                              'ethernet' => l10n.callDetailsEthernet,
                              '' => l10n.callDetailsNetworkUnknown,
                              _ => leg.networkType,
                            },
                            l10n.callDetailsRoutes(leg.candidateCount),
                            if (!leg.negotiated) l10n.callDetailsIncomplete,
                          ].join(' · '),
                        ),
                      ),
                  ],
                );
              },
            ),
          ],
        ),
      ),
    );
  }

  String _nameOf(String userId, AppLocalizations l10n) {
    for (final CallParticipant participant in call.participants) {
      if (participant.userId == userId) {
        return participant.displayName;
      }
    }
    return l10n.callDetailsOtherSide;
  }
}
