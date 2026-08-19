import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../media/data/media_repository.dart';
import '../data/account_repository.dart';

/// A copy of your account, and the end of it (§60).
///
/// Both halves of the same right, on one screen, because they are the two
/// things a person can ask for about their own data and separating them would
/// hide the second behind the first.
///
/// The deletion delay is read from the server rather than written here. A
/// figure typed into the UI would be a promise nothing enforces, and the two
/// could drift without either looking wrong.
class DataRightsScreen extends ConsumerStatefulWidget {
  const DataRightsScreen({super.key});

  @override
  ConsumerState<DataRightsScreen> createState() => _DataRightsScreenState();
}

class _DataRightsScreenState extends ConsumerState<DataRightsScreen> {
  bool _working = false;

  void _report(Object error) {
    if (!mounted) {
      return;
    }
    final AppLocalizations l10n = AppLocalizations.of(context);
    ScaffoldMessenger.of(context).showSnackBar(
      SnackBar(
        content: Text(
          error is ApiException
              ? (error.isOffline ? l10n.errorNetwork : error.message)
              : '$error',
        ),
      ),
    );
  }

  Future<void> _request(String type) async {
    if (_working) {
      return;
    }
    setState(() => _working = true);
    try {
      await ref.read(accountRepositoryProvider).requestData(type);
      ref.invalidate(dataRightsProvider);
    } on ApiException catch (error) {
      _report(error);
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  Future<void> _cancel(String requestId) async {
    setState(() => _working = true);
    try {
      await ref.read(accountRepositoryProvider).cancelDataRequest(requestId);
      ref.invalidate(dataRightsProvider);
    } on ApiException catch (error) {
      _report(error);
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  /// Opens the finished export.
  ///
  /// It goes out through the same presigned URL as any other file, so the
  /// bytes never pass through the API and the link expires on its own.
  Future<void> _download(String mediaId) async {
    try {
      final String url =
          await ref.read(mediaRepositoryProvider).downloadUrl(mediaId);
      final Uri uri = Uri.parse(url);
      if (!await launchUrl(uri, mode: LaunchMode.externalApplication)) {
        _report(Exception(url));
      }
    } on Object catch (error) {
      _report(error);
    }
  }

  /// Deletion is confirmed against the delay the server actually applies, and
  /// the dialog says what will happen rather than asking "are you sure?".
  Future<void> _confirmDeletion(Duration delay) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final int days = delay.inDays;
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.dataDeleteTitle),
            content: Text(l10n.dataDeleteConfirm(days)),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.dataDeleteAction),
              ),
            ],
          ),
        ) ??
        false;
    if (confirmed) {
      await _request('delete');
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<DataRights> rights = ref.watch(dataRightsProvider);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.dataRightsTitle)),
      body: rights.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(dataRightsProvider),
        ),
        data: (DataRights data) {
          final DataRequest? deletion = data.openDeletion;

          return ListView(
            children: <Widget>[
              if (_working) const LinearProgressIndicator(),

              ListTile(
                leading: const Icon(Icons.download_outlined),
                title: Text(l10n.dataExportTitle),
                subtitle: Text(l10n.dataExportBody),
                trailing: FilledButton(
                  onPressed: _working ? null : () => _request('export'),
                  child: Text(l10n.dataExportAction),
                ),
              ),
              const Divider(),

              // A pending deletion turns this row into the way to take it
              // back, because that is the only thing worth offering while one
              // is running.
              if (deletion != null)
                ListTile(
                  leading: Icon(Icons.timer_outlined, color: palette.error),
                  title: Text(l10n.dataDeletePendingTitle),
                  subtitle: Text(
                    l10n.dataDeletePendingBody(
                      DateFormat.yMd().add_Hm().format(deletion.executeAfter),
                    ),
                  ),
                  trailing: FilledButton(
                    onPressed: _working ? null : () => _cancel(deletion.id),
                    child: Text(l10n.dataDeleteWithdraw),
                  ),
                )
              else
                ListTile(
                  leading: Icon(
                    Icons.delete_forever_outlined,
                    color: palette.error,
                  ),
                  title: Text(
                    l10n.dataDeleteTitle,
                    style: TextStyle(color: palette.error),
                  ),
                  subtitle: Text(l10n.dataDeleteBody(data.deletionDelay.inDays)),
                  trailing: OutlinedButton(
                    onPressed: _working
                        ? null
                        : () => _confirmDeletion(data.deletionDelay),
                    child: Text(l10n.dataDeleteAction),
                  ),
                ),

              if (data.requests.isNotEmpty) ...<Widget>[
                const Divider(),
                Padding(
                  padding: const EdgeInsets.fromLTRB(
                    SobhSpacing.lg,
                    SobhSpacing.lg,
                    SobhSpacing.lg,
                    SobhSpacing.sm,
                  ),
                  child: Text(
                    l10n.dataRequestsHistory,
                    style: Theme.of(context)
                        .textTheme
                        .labelLarge
                        ?.copyWith(color: palette.textSecondary),
                  ),
                ),
                for (final DataRequest request in data.requests)
                  _RequestRow(
                    request: request,
                    working: _working,
                    onDownload: () => _download(request.resultMediaId!),
                    onCancel: () => _cancel(request.id),
                  ),
              ],
            ],
          );
        },
      ),
    );
  }
}

class _RequestRow extends StatelessWidget {
  const _RequestRow({
    required this.request,
    required this.working,
    required this.onDownload,
    required this.onCancel,
  });

  final DataRequest request;
  final bool working;
  final VoidCallback onDownload;
  final VoidCallback onCancel;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return ListTile(
      leading: Icon(
        request.isExport ? Icons.archive_outlined : Icons.delete_outline,
        color: request.status == 'failed' ? palette.error : null,
      ),
      title: Text(
        request.isExport ? l10n.dataExportTitle : l10n.dataDeleteTitle,
      ),
      subtitle: Text(
        <String>[
          switch (request.status) {
            'pending' => l10n.dataStatusPending,
            'processing' => l10n.dataStatusProcessing,
            'ready' => l10n.dataStatusReady,
            'completed' => l10n.dataStatusCompleted,
            'cancelled' => l10n.dataStatusCancelled,
            _ => l10n.dataStatusFailed,
          },
          DateFormat.yMd().format(request.createdAt),
          // The server's reason, when there is one — a failure with no
          // explanation leaves the person nothing to act on.
          if (request.error.isNotEmpty) request.error,
        ].join(' · '),
      ),
      trailing: switch (request) {
        final DataRequest r when r.downloadable => TextButton(
            onPressed: working ? null : onDownload,
            child: Text(l10n.dataExportDownload),
          ),
        final DataRequest r when r.cancellable => TextButton(
            onPressed: working ? null : onCancel,
            child: Text(l10n.commonCancel),
          ),
        _ => null,
      },
    );
  }
}
