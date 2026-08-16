import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One signed-in device (§10, §56).
class UserDevice {
  const UserDevice({
    required this.id,
    required this.name,
    required this.platform,
    required this.appVersion,
    required this.lastSeenAt,
    required this.isCurrent,
  });

  factory UserDevice.fromJson(Map<String, dynamic> json) => UserDevice(
        id: json['id'] as String,
        name: json['name'] as String? ?? '',
        platform: json['platform'] as String? ?? 'unknown',
        appVersion: json['app_version'] as String? ?? '',
        lastSeenAt: DateTime.parse(json['last_seen_at'] as String).toLocal(),
        isCurrent: json['is_current'] as bool? ?? false,
      );

  final String id;
  final String name;
  final String platform;
  final String appVersion;
  final DateTime lastSeenAt;
  final bool isCurrent;
}

/// One active session, which is what "sign this device out" actually revokes.
class UserSession {
  const UserSession({
    required this.id,
    required this.deviceId,
    required this.userAgent,
    required this.lastUsedAt,
    required this.isCurrent,
    this.ip,
  });

  factory UserSession.fromJson(Map<String, dynamic> json) => UserSession(
        id: json['id'] as String,
        deviceId: json['device_id'] as String,
        userAgent: json['user_agent'] as String? ?? '',
        lastUsedAt: DateTime.parse(json['last_used_at'] as String).toLocal(),
        isCurrent: json['is_current'] as bool? ?? false,
        ip: json['ip'] as String?,
      );

  final String id;
  final String deviceId;
  final String userAgent;
  final DateTime lastUsedAt;
  final bool isCurrent;
  final String? ip;
}

/// Per-user notification preferences (§30).
class NotificationSettings {
  const NotificationSettings({
    required this.privateChats,
    required this.groups,
    required this.channels,
    required this.breakingNews,
    required this.calls,
    required this.stories,
    required this.showPreview,
    this.quietHoursStart,
    this.quietHoursEnd,
  });

  factory NotificationSettings.fromJson(Map<String, dynamic> json) =>
      NotificationSettings(
        privateChats: json['private_chats'] as bool? ?? true,
        groups: json['groups'] as bool? ?? true,
        channels: json['channels'] as bool? ?? true,
        breakingNews: json['breaking_news'] as bool? ?? true,
        calls: json['calls'] as bool? ?? true,
        stories: json['stories'] as bool? ?? true,
        showPreview: json['show_preview'] as bool? ?? true,
        quietHoursStart: (json['quiet_hours_start'] as num?)?.toInt(),
        quietHoursEnd: (json['quiet_hours_end'] as num?)?.toInt(),
      );

  final bool privateChats;
  final bool groups;
  final bool channels;
  final bool breakingNews;
  final bool calls;
  final bool stories;
  final bool showPreview;
  final int? quietHoursStart;
  final int? quietHoursEnd;

  NotificationSettings copyWith({
    bool? privateChats,
    bool? groups,
    bool? channels,
    bool? breakingNews,
    bool? calls,
    bool? stories,
    bool? showPreview,
    int? quietHoursStart,
    int? quietHoursEnd,
  }) =>
      NotificationSettings(
        privateChats: privateChats ?? this.privateChats,
        groups: groups ?? this.groups,
        channels: channels ?? this.channels,
        breakingNews: breakingNews ?? this.breakingNews,
        calls: calls ?? this.calls,
        stories: stories ?? this.stories,
        showPreview: showPreview ?? this.showPreview,
        quietHoursStart: quietHoursStart ?? this.quietHoursStart,
        quietHoursEnd: quietHoursEnd ?? this.quietHoursEnd,
      );

  Map<String, dynamic> toJson() => <String, dynamic>{
        'private_chats': privateChats,
        'groups': groups,
        'channels': channels,
        'breaking_news': breakingNews,
        'calls': calls,
        'stories': stories,
        'show_preview': showPreview,
        if (quietHoursStart != null) 'quiet_hours_start': quietHoursStart,
        if (quietHoursEnd != null) 'quiet_hours_end': quietHoursEnd,
      };
}

/// Devices, sessions and notification settings.
class AccountRepository {
  AccountRepository(this._api);

  final ApiClient _api;

  Future<List<UserDevice>> devices() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/auth/devices');
    return <UserDevice>[
      for (final dynamic entry
          in data['devices'] as List<dynamic>? ?? const <dynamic>[])
        UserDevice.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<List<UserSession>> sessions() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/auth/sessions');
    return <UserSession>[
      for (final dynamic entry
          in data['sessions'] as List<dynamic>? ?? const <dynamic>[])
        UserSession.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<void> revokeSession(String sessionId) =>
      _api.delete<Map<String, dynamic>>('/auth/sessions/$sessionId');

  Future<int> revokeOtherSessions() async {
    final Map<String, dynamic> data =
        await _api.post<Map<String, dynamic>>('/auth/sessions/revoke-others');
    return (data['revoked'] as num?)?.toInt() ?? 0;
  }

  Future<NotificationSettings> notificationSettings() async =>
      NotificationSettings.fromJson(
        await _api.get<Map<String, dynamic>>('/notifications/settings'),
      );

  Future<void> updateNotificationSettings(NotificationSettings settings) =>
      _api.put<Map<String, dynamic>>(
        '/notifications/settings',
        body: settings.toJson(),
      );

  Future<int> unreadNotificationCount() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/notifications/unread-count');
    return (data['unread_count'] as num?)?.toInt() ?? 0;
  }
}

final Provider<AccountRepository> accountRepositoryProvider =
    Provider<AccountRepository>(
  (Ref ref) => AccountRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<UserSession>> sessionListProvider =
    FutureProvider<List<UserSession>>(
  (Ref ref) => ref.watch(accountRepositoryProvider).sessions(),
);

final FutureProvider<List<UserDevice>> deviceListProvider =
    FutureProvider<List<UserDevice>>(
  (Ref ref) => ref.watch(accountRepositoryProvider).devices(),
);

final FutureProvider<NotificationSettings> notificationSettingsProvider =
    FutureProvider<NotificationSettings>(
  (Ref ref) => ref.watch(accountRepositoryProvider).notificationSettings(),
);
