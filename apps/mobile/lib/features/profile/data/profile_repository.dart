import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A user as this client may see them (§9, §55).
///
/// A field the viewer is not permitted to see arrives absent rather than
/// blank, so `null` here means "not shown to you", not "not set".
class UserProfile {
  const UserProfile({
    required this.userId,
    required this.displayName,
    this.username,
    this.about = '',
    this.avatarMediaId,
    this.language = 'fa',
    this.isBot = false,
    this.lastSeen,
    this.isContact = false,
    this.isBlocked = false,
  });

  factory UserProfile.fromJson(Map<String, dynamic> json) => UserProfile(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        username: json['username'] as String?,
        about: json['about'] as String? ?? '',
        avatarMediaId: json['avatar_media_id'] as String?,
        language: json['language'] as String? ?? 'fa',
        isBot: json['is_bot'] as bool? ?? false,
        lastSeen: json['last_seen'] == null
            ? null
            : DateTime.parse(json['last_seen'] as String).toLocal(),
        isContact: json['is_contact'] as bool? ?? false,
        isBlocked: json['is_blocked'] as bool? ?? false,
      );

  final String userId;
  final String displayName;
  final String? username;
  final String about;
  final String? avatarMediaId;
  final String language;
  final bool isBot;
  final DateTime? lastSeen;
  final bool isContact;
  final bool isBlocked;

  /// The handle as it is written and shared, or empty when none is claimed.
  String get handle => username == null ? '' : '@$username';
}

/// The caller's own profile, which carries the fields nobody else may see.
class SelfProfile extends UserProfile {
  const SelfProfile({
    required super.userId,
    required super.displayName,
    required this.phoneNumber,
    super.username,
    super.about,
    super.avatarMediaId,
    super.language,
    super.isBot,
    super.lastSeen,
    this.birthday,
    this.twoStepEnabled = false,
    this.twoStepHint = '',
  });

  factory SelfProfile.fromJson(Map<String, dynamic> json) => SelfProfile(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        phoneNumber: json['phone_number'] as String? ?? '',
        username: json['username'] as String?,
        about: json['about'] as String? ?? '',
        avatarMediaId: json['avatar_media_id'] as String?,
        language: json['language'] as String? ?? 'fa',
        isBot: json['is_bot'] as bool? ?? false,
        lastSeen: json['last_seen'] == null
            ? null
            : DateTime.parse(json['last_seen'] as String).toLocal(),
        birthday: json['birthday'] == null
            ? null
            : DateTime.parse(json['birthday'] as String).toLocal(),
        twoStepEnabled: json['two_step_enabled'] as bool? ?? false,
        twoStepHint: json['two_step_hint'] as String? ?? '',
      );

  final String phoneNumber;
  final DateTime? birthday;

  /// Whether a second factor guards this account. The settings screen has no
  /// other way to ask; without it the only way to find out was to be locked
  /// out at the next sign-in.
  final bool twoStepEnabled;

  /// The reminder the owner chose, shown back to them so they can see what it
  /// says before they are relying on it.
  final String twoStepHint;
}

/// Why a username cannot be claimed, as the server decided it.
enum UsernameStatus { available, invalid, reserved, taken }

class UsernameAvailability {
  const UsernameAvailability({required this.username, required this.status});

  factory UsernameAvailability.fromJson(Map<String, dynamic> json) {
    final bool available = json['available'] as bool? ?? false;
    if (available) {
      return UsernameAvailability(
        username: json['username'] as String? ?? '',
        status: UsernameStatus.available,
      );
    }
    return UsernameAvailability(
      username: json['username'] as String? ?? '',
      status: switch (json['reason'] as String?) {
        'invalid' => UsernameStatus.invalid,
        'reserved' => UsernameStatus.reserved,
        _ => UsernameStatus.taken,
      },
    );
  }

  final String username;
  final UsernameStatus status;

  bool get isAvailable => status == UsernameStatus.available;
}

/// Profiles and usernames (§9, §11).
class ProfileRepository {
  ProfileRepository(this._api);

  final ApiClient _api;

  Future<SelfProfile> self() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/users/me');
    return SelfProfile.fromJson(data);
  }

  Future<UserProfile> byId(String userId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/users/$userId');
    return UserProfile.fromJson(data);
  }

  /// Resolves a `@handle`, which is how an incoming link is opened.
  Future<UserProfile> byUsername(String username) async {
    final String bare =
        username.startsWith('@') ? username.substring(1) : username;
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/users/by-username/$bare');
    return UserProfile.fromJson(data);
  }

  /// Sends only the fields that changed.
  ///
  /// An omitted field is left alone by the server, so a client that does not
  /// know about a field cannot erase it — which matters as the app and the
  /// server are updated separately.
  Future<SelfProfile> updateProfile({
    String? displayName,
    String? about,
    String? avatarMediaId,
    String? language,
    DateTime? birthday,
  }) async {
    final Map<String, dynamic> body = <String, dynamic>{
      if (displayName != null) 'display_name': displayName,
      if (about != null) 'about': about,
      if (avatarMediaId != null) 'avatar_media_id': avatarMediaId,
      if (language != null) 'language': language,
      if (birthday != null) 'birthday': birthday.toUtc().toIso8601String(),
    };
    final Map<String, dynamic> data =
        await _api.patch<Map<String, dynamic>>('/users/me', body: body);
    return SelfProfile.fromJson(data);
  }

  Future<void> claimUsername(String username) => _api.put<dynamic>(
        '/users/me/username',
        body: <String, dynamic>{'username': username},
      );

  /// Asks whether a handle can be taken. The server decides with the same
  /// query the claim itself uses, so this cannot say yes and then refuse.
  Future<UsernameAvailability> checkUsername(String username) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/users/username-available',
      query: <String, dynamic>{'username': username},
    );
    return UsernameAvailability.fromJson(data);
  }
}

final Provider<ProfileRepository> profileRepositoryProvider =
    Provider<ProfileRepository>(
  (Ref ref) => ProfileRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<SelfProfile> selfProfileProvider =
    FutureProvider<SelfProfile>(
  (Ref ref) => ref.watch(profileRepositoryProvider).self(),
);

/// One other user's profile, keyed by id so several can be open at once.
final FutureProviderFamily<UserProfile, String> userProfileProvider =
    FutureProvider.family<UserProfile, String>(
  (Ref ref, String userId) => ref.watch(profileRepositoryProvider).byId(userId),
);
