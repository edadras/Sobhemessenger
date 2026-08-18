import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/contact_requests_repository.dart';
import '../data/contacts_repository.dart';

/// Contact requests (§54).
///
/// Two lists, because they answer different questions. Incoming is what is
/// waiting on you and holds only what is still pending. Outgoing keeps its
/// history, so a declined request reads as declined rather than as one that
/// never arrived.
class ContactRequestsScreen extends ConsumerWidget {
  const ContactRequestsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return DefaultTabController(
      length: 2,
      child: Scaffold(
        appBar: AppBar(
          title: Text(l10n.contactRequestsTitle),
          bottom: TabBar(
            tabs: <Widget>[
              Tab(text: l10n.contactRequestsIncoming),
              Tab(text: l10n.contactRequestsOutgoing),
            ],
          ),
        ),
        body: const TabBarView(
          children: <Widget>[
            _RequestList(direction: 'incoming'),
            _RequestList(direction: 'outgoing'),
          ],
        ),
      ),
    );
  }
}

class _RequestList extends ConsumerWidget {
  const _RequestList({required this.direction});

  final String direction;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ContactRequest>> requests =
        ref.watch(contactRequestsProvider(direction));

    return requests.when(
      loading: () => const Center(child: CircularProgressIndicator()),
      error: (Object error, StackTrace stack) => SobhErrorState(
        error: error,
        onRetry: () => ref.invalidate(contactRequestsProvider(direction)),
      ),
      data: (List<ContactRequest> rows) {
        if (rows.isEmpty) {
          return Center(child: Text(l10n.contactRequestsEmpty));
        }
        return ListView.separated(
          itemCount: rows.length,
          separatorBuilder: (_, __) => const Divider(height: 1),
          itemBuilder: (BuildContext context, int index) => _RequestTile(
            request: rows[index],
            incoming: direction == 'incoming',
          ),
        );
      },
    );
  }
}

class _RequestTile extends ConsumerWidget {
  const _RequestTile({required this.request, required this.incoming});

  final ContactRequest request;
  final bool incoming;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return ListTile(
      leading: CircleAvatar(
        child: Text(
          request.displayName.isEmpty
              ? '?'
              : request.displayName.characters.first,
        ),
      ),
      title: Text(
        request.displayName.isEmpty
            ? (request.username ?? '')
            : request.displayName,
      ),
      subtitle: Text(
        request.message.isNotEmpty ? request.message : _status(l10n),
      ),
      trailing: _actions(context, ref, l10n),
    );
  }

  String _status(AppLocalizations l10n) => switch (request.status) {
        'accepted' => l10n.contactRequestStatusAccepted,
        'rejected' => l10n.contactRequestStatusRejected,
        _ => l10n.contactRequestStatusPending,
      };

  Widget? _actions(
    BuildContext context,
    WidgetRef ref,
    AppLocalizations l10n,
  ) {
    if (!request.isPending) {
      return Text(_status(l10n), style: Theme.of(context).textTheme.labelSmall);
    }

    if (!incoming) {
      return TextButton(
        onPressed: () => _resolve(context, ref, 'cancel'),
        child: Text(l10n.contactRequestCancel),
      );
    }

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        TextButton(
          onPressed: () => _resolve(context, ref, 'reject'),
          child: Text(l10n.contactRequestReject),
        ),
        const SizedBox(width: SobhSpacing.xs),
        FilledButton(
          onPressed: () => _resolve(context, ref, 'accept'),
          child: Text(l10n.contactRequestAccept),
        ),
      ],
    );
  }

  Future<void> _resolve(
    BuildContext context,
    WidgetRef ref,
    String action,
  ) async {
    final ContactRequestsRepository repository =
        ref.read(contactRequestsRepositoryProvider);

    try {
      switch (action) {
        case 'accept':
          await repository.accept(request.id);
        case 'reject':
          await repository.reject(request.id);
        case 'cancel':
          await repository.cancel(request.id);
      }
      ref
        ..invalidate(contactRequestsProvider('incoming'))
        ..invalidate(contactRequestsProvider('outgoing'));
      // Accepting writes both address books, so the contact list is stale.
      if (action == 'accept') {
        ref.invalidate(contactListProvider);
      }
    } on ApiException catch (error) {
      if (context.mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }
}
