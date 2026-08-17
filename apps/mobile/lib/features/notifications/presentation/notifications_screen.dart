import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:intl/intl.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../../settings/data/account_repository.dart';

/// One stored notification (§30).
class AppNotification {
  const AppNotification({
    required this.id,
    required this.type,
    required this.title,
    required this.body,
    required this.createdAt,
    this.readAt,
  });

  factory AppNotification.fromJson(Map<String, dynamic> json) =>
      AppNotification(
        id: json['id'] as String,
        type: json['type'] as String? ?? '',
        title: json['title'] as String? ?? '',
        body: json['body'] as String? ?? '',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        readAt: json['read_at'] == null
            ? null
            : DateTime.parse(json['read_at'] as String).toLocal(),
      );

  final String id;
  final String type;
  final String title;
  final String body;
  final DateTime createdAt;
  final DateTime? readAt;

  bool get isUnread => readAt == null;
}

final FutureProvider<List<AppNotification>> notificationsProvider =
    FutureProvider<List<AppNotification>>((Ref ref) async {
  final Map<String, dynamic> data = await ref
      .watch(apiClientProvider)
      .get<Map<String, dynamic>>('/notifications');
  return <AppNotification>[
    for (final dynamic entry
        in data['notifications'] as List<dynamic>? ?? const <dynamic>[])
      AppNotification.fromJson(entry as Map<String, dynamic>),
  ];
});

/// The notification inbox (§30).
class NotificationsScreen extends ConsumerWidget {
  const NotificationsScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<AppNotification>> notifications = ref.watch(
      notificationsProvider,
    );

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.notificationsTitle),
        actions: <Widget>[
          TextButton(
            onPressed: () async {
              // An empty id list marks everything read, which is what the
              // button says it does.
              await ref.read(apiClientProvider).post<Map<String, dynamic>>(
                '/notifications/read',
                body: const <String, dynamic>{},
              );
              ref
                ..invalidate(notificationsProvider)
                ..invalidate(notificationSettingsProvider);
            },
            child: Text(l10n.notificationsMarkRead),
          ),
        ],
      ),
      body: notifications.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(notificationsProvider),
        ),
        data: (List<AppNotification> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.notifications_none,
                title: l10n.notificationsEmpty,
              )
            : RefreshIndicator(
                onRefresh: () async => ref.invalidate(notificationsProvider),
                child: ListView.separated(
                  itemCount: rows.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (BuildContext context, int index) {
                    final AppNotification notification = rows[index];
                    return ListTile(
                      // An unread entry gets a dot rather than a different
                      // background: the list stays readable in both themes.
                      leading: Icon(
                        notification.isUnread
                            ? Icons.circle
                            : Icons.circle_outlined,
                        size: SobhSizes.iconSmall,
                        color: notification.isUnread
                            ? palette.primary
                            : palette.textDisabled,
                      ),
                      title: Text(notification.title),
                      subtitle: Text(
                        notification.body,
                        maxLines: 2,
                        overflow: TextOverflow.ellipsis,
                      ),
                      trailing: Text(
                        DateFormat.Md().add_Hm().format(notification.createdAt),
                        style: Theme.of(context).textTheme.labelSmall,
                      ),
                    );
                  },
                ),
              ),
      ),
    );
  }
}
