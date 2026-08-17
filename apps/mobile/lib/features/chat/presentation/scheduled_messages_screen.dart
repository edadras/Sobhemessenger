import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/organise_repository.dart';

/// The posts the caller has queued in one chat (§12).
class ScheduledMessagesScreen extends ConsumerWidget {
  const ScheduledMessagesScreen({required this.chatId, super.key});

  final String chatId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<ScheduledMessage>> queued =
        ref.watch(scheduledMessagesProvider(chatId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.chatScheduledTitle)),
      floatingActionButton: FloatingActionButton.extended(
        onPressed: () => _compose(context, ref),
        icon: const Icon(Icons.schedule_send_outlined),
        label: Text(l10n.chatScheduleNew),
      ),
      body: queued.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(scheduledMessagesProvider(chatId)),
        ),
        data: (List<ScheduledMessage> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.schedule_outlined,
                title: l10n.chatScheduledEmptyTitle,
                body: l10n.chatScheduledEmptyMessage,
              )
            : ListView.builder(
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) {
                  final ScheduledMessage post = rows[index];
                  return ListTile(
                    leading: Icon(
                      post.hasFailed
                          ? Icons.error_outline
                          : Icons.schedule_outlined,
                      color: post.hasFailed ? palette.error : null,
                    ),
                    title: Text(post.content),
                    subtitle: Text(
                      // A post that failed says why, rather than sitting in the
                      // list looking like it is still going to go out.
                      post.hasFailed
                          ? l10n.chatScheduledFailed(post.lastError)
                          : _when(context, post.scheduledAt),
                      style: TextStyle(
                        color: post.hasFailed ? palette.error : null,
                      ),
                    ),
                    trailing: IconButton(
                      tooltip: l10n.commonDelete,
                      icon: const Icon(Icons.delete_outline),
                      onPressed: () => _cancel(context, ref, post),
                    ),
                  );
                },
              ),
      ),
    );
  }

  String _when(BuildContext context, DateTime at) {
    final MaterialLocalizations formats = MaterialLocalizations.of(context);
    final String date = formats.formatFullDate(at);
    final String time = formats.formatTimeOfDay(TimeOfDay.fromDateTime(at));
    return '$date — $time';
  }

  Future<void> _cancel(
    BuildContext context,
    WidgetRef ref,
    ScheduledMessage post,
  ) async {
    try {
      await ref
          .read(organiseRepositoryProvider)
          .cancelScheduled(chatId, post.id);
      ref.invalidate(scheduledMessagesProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text(error.message)),
        );
      }
    }
  }

  Future<void> _compose(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final TextEditingController text = TextEditingController();

    final DateTime? date = await showDatePicker(
      context: context,
      firstDate: DateTime.now(),
      // A year ahead, matching what the server will accept, so the picker
      // cannot offer a date the save would then reject.
      lastDate: DateTime.now().add(const Duration(days: 365)),
      initialDate: DateTime.now(),
    );
    if (date == null || !context.mounted) {
      text.dispose();
      return;
    }

    final TimeOfDay? time = await showTimePicker(
      context: context,
      initialTime: TimeOfDay.now(),
    );
    if (time == null || !context.mounted) {
      text.dispose();
      return;
    }

    final DateTime at = DateTime(
      date.year,
      date.month,
      date.day,
      time.hour,
      time.minute,
    );

    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext dialog) => AlertDialog(
            title: Text(l10n.chatScheduleNew),
            content: TextField(
              controller: text,
              autofocus: true,
              maxLines: 4,
              decoration: InputDecoration(hintText: l10n.chatMessageHint),
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(dialog).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(dialog).pop(true),
                child: Text(l10n.commonSave),
              ),
            ],
          ),
        ) ??
        false;

    if (!confirmed || text.text.trim().isEmpty || !context.mounted) {
      text.dispose();
      return;
    }

    try {
      await ref.read(organiseRepositoryProvider).schedule(
            chatId: chatId,
            // Generated here and carried through to the published message, so
            // a retry over a flaky link edits this post rather than queueing a
            // second copy of it.
            clientMessageId: const Uuid().v4(),
            content: text.text.trim(),
            at: at,
          );
      ref.invalidate(scheduledMessagesProvider(chatId));
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      text.dispose();
    }
  }
}
