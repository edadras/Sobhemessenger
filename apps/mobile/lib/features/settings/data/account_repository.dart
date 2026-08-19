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

/// One entry in the sign-in log (§56).
///
/// Both halves matter. A successful sign-in from a place the owner does not
/// recognise is the first sign of a stolen code; a run of failures is the
/// first sign of someone trying to guess one. Showing only the successes
/// would hide the attempt that has not worked yet.
class LoginEvent {
  const LoginEvent({
    required this.event,
    required this.platform,
    required this.succeeded,
    required this.createdAt,
    this.userAgent = '',
    this.ip,
  });

  factory LoginEvent.fromJson(Map<String, dynamic> json) => LoginEvent(
        event: json['event'] as String? ?? '',
        platform: json['platform'] as String? ?? '',
        succeeded: json['succeeded'] as bool? ?? false,
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        userAgent: json['user_agent'] as String? ?? '',
        ip: json['ip'] as String?,
      );

  final String event;
  final String platform;
  final bool succeeded;
  final DateTime createdAt;
  final String userAgent;
  final String? ip;
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

  /// Every privacy rule, including keys never explicitly set — the server
  /// fills those in with the default it applies anyway, so the screen shows a
  /// complete list rather than one that grows as settings are first touched.
  Future<List<PrivacySetting>> privacySettings() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/users/me/privacy');
    return <PrivacySetting>[
      for (final dynamic entry
          in data['settings'] as List<dynamic>? ?? const <dynamic>[])
        PrivacySetting.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Changes one rule. The exception lists are sent unchanged, so switching
  /// "last seen" from contacts to everyone does not quietly discard the person
  /// who was blocked from seeing it.
  Future<void> setPrivacy(PrivacySetting setting) =>
      _api.put<Map<String, dynamic>>(
        '/users/me/privacy/${setting.key}',
        body: <String, dynamic>{
          'rule': setting.rule,
          'allow_list': setting.allowList,
          'deny_list': setting.denyList,
        },
      );

  Future<int> unreadNotificationCount() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/notifications/unread-count');
    return (data['unread_count'] as num?)?.toInt() ?? 0;
  }

  /// Recent sign-in attempts on this account, newest first.
  Future<List<LoginEvent>> loginHistory() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/auth/login-history');
    return <LoginEvent>[
      for (final dynamic entry
          in data['history'] as List<dynamic>? ?? const <dynamic>[])
        LoginEvent.fromJson(entry as Map<String, dynamic>),
    ];
  }

  // -------------------------------------------------------- email recovery

  Future<RecoveryEmail> recoveryEmail() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/auth/recovery/email');
    return RecoveryEmail.fromJson(data);
  }

  /// Enrols an address and sends it a code.
  ///
  /// It is not the recovery address yet: it becomes one when the code is
  /// presented, and not before. An unverified address is worse than none,
  /// because it would be a second way into the account that the owner never
  /// confirmed.
  Future<void> setRecoveryEmail(String email) => _api.put<dynamic>(
        '/auth/recovery/email',
        body: <String, dynamic>{'email': email},
      );

  Future<void> verifyRecoveryEmail(String code) => _api.post<dynamic>(
        '/auth/recovery/email/verify',
        body: <String, dynamic>{'code': code},
      );

  Future<void> removeRecoveryEmail() =>
      _api.delete<dynamic>('/auth/recovery/email');
}

/// The recovery address on the account (§4).
class RecoveryEmail {
  const RecoveryEmail({
    this.email = '',
    this.verified = false,
    this.available = false,
    this.verifiedAt,
  });

  factory RecoveryEmail.fromJson(Map<String, dynamic> json) => RecoveryEmail(
        email: json['email'] as String? ?? '',
        verified: json['verified'] as bool? ?? false,
        available: json['available'] as bool? ?? false,
        verifiedAt: json['verified_at'] == null
            ? null
            : DateTime.parse(json['verified_at'] as String).toLocal(),
      );

  /// Masked by the server: the settings screen has to show which address is on
  /// the account without handing back the whole of it.
  final String email;

  /// The only field that decides whether recovery works.
  final bool verified;

  /// Whether this deployment can send mail at all. When false the feature is
  /// off and the screen says so rather than offering something that would be
  /// refused.
  final bool available;
  final DateTime? verifiedAt;

  bool get isSet => email.isNotEmpty;
}

/// One privacy rule of §55.
///
/// The rule decides the general case; the two lists are the exceptions, which
/// is how "everyone except her" and "nobody but him" are expressed. The server
/// applies a deny before an allow before the rule.
class PrivacySetting {
  const PrivacySetting({
    required this.key,
    required this.rule,
    this.allowList = const <String>[],
    this.denyList = const <String>[],
  });

  factory PrivacySetting.fromJson(Map<String, dynamic> json) => PrivacySetting(
        key: json['key'] as String,
        rule: json['rule'] as String? ?? 'contacts',
        allowList: <String>[
          for (final dynamic id
              in json['allow_list'] as List<dynamic>? ?? const <dynamic>[])
            id as String,
        ],
        denyList: <String>[
          for (final dynamic id
              in json['deny_list'] as List<dynamic>? ?? const <dynamic>[])
            id as String,
        ],
      );

  final String key;
  final String rule;
  final List<String> allowList;
  final List<String> denyList;

  PrivacySetting copyWith({String? rule}) => PrivacySetting(
        key: key,
        rule: rule ?? this.rule,
        allowList: allowList,
        denyList: denyList,
      );
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

final FutureProvider<RecoveryEmail> recoveryEmailProvider =
    FutureProvider<RecoveryEmail>(
  (Ref ref) => ref.watch(accountRepositoryProvider).recoveryEmail(),
);

final FutureProvider<List<LoginEvent>> loginHistoryProvider =
    FutureProvider<List<LoginEvent>>(
  (Ref ref) => ref.watch(accountRepositoryProvider).loginHistory(),
);
