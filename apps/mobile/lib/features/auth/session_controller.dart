import 'dart:async';

import 'package:flutter/foundation.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../core/config/app_config.dart';
import '../../core/network/api_client.dart';
import '../../core/network/api_exception.dart';
import '../../core/storage/local_database.dart';
import '../../core/storage/token_store.dart';
import '../../core/websocket/socket_client.dart';
import '../notifications/data/push_registration.dart';

final Provider<AppConfig> appConfigProvider =
    Provider<AppConfig>((Ref ref) => AppConfig.fromEnvironment());

final Provider<TokenStore> tokenStoreProvider =
    Provider<TokenStore>((Ref ref) => TokenStore());

final Provider<LocalDatabase> localDatabaseProvider =
    Provider<LocalDatabase>((Ref ref) {
  final LocalDatabase database = LocalDatabase();
  ref.onDispose(database.close);
  return database;
});

final Provider<ApiClient> apiClientProvider = Provider<ApiClient>((Ref ref) {
  final ApiClient client = ApiClient(
    config: ref.watch(appConfigProvider),
    tokenStore: ref.watch(tokenStoreProvider),
  );
  ref.onDispose(client.dispose);
  return client;
});

final Provider<SocketClient> socketClientProvider =
    Provider<SocketClient>((Ref ref) {
  final SocketClient client = SocketClient(
    config: ref.watch(appConfigProvider),
    tokenStore: ref.watch(tokenStoreProvider),
  );
  ref.onDispose(client.dispose);
  return client;
});

/// Where the user stands with respect to authentication.
@immutable
class SessionState {
  const SessionState({
    this.isRestoring = true,
    this.userId,
    this.deviceId,
  });

  /// True until the stored session has been read from the keychain, so the
  /// router does not bounce the user to sign-in during a cold start.
  final bool isRestoring;
  final String? userId;
  final String? deviceId;

  bool get isAuthenticated => userId != null;
}

/// Owns the session: restoring it at launch, establishing it after sign-in,
/// and tearing it down on sign-out.
class SessionController extends AsyncNotifier<SessionState> {
  @override
  Future<SessionState> build() async {
    // A dropped refresh token means the server ended the session; return the
    // user to sign-in rather than leaving them on a dead screen.
    ref.listen<ApiClient>(apiClientProvider, (_, ApiClient client) {
      client.onSessionExpired.listen((_) => signOut());
    });
    return _restore();
  }

  Future<SessionState> _restore() async {
    final TokenStore tokens = ref.read(tokenStoreProvider);
    final String? userId = await tokens.readUserId();
    if (userId == null) {
      return const SessionState(isRestoring: false);
    }

    final SessionState restored = SessionState(
      isRestoring: false,
      userId: userId,
      deviceId: await tokens.readDeviceId(),
    );
    unawaitedConnect();
    return restored;
  }

  /// Opens the realtime connection without blocking the caller.
  void unawaitedConnect() {
    ref.read(socketClientProvider).connect().ignore();
  }

  /// Requests a verification code for [phone].
  Future<void> requestCode(String phone) async {
    final ApiClient api = ref.read(apiClientProvider);
    await api.post<Map<String, dynamic>>(
      '/auth/otp/request',
      body: <String, String>{'phone': phone},
      authenticated: false,
    );
  }

  /// Verifies a code and establishes the session.
  ///
  /// Throws [ApiException] with `TWO_STEP_REQUIRED` when the account has a
  /// second factor; the caller retries with [password].
  Future<void> verifyCode({
    required String phone,
    required String code,
    String? password,
    required String deviceName,
    required String platform,
  }) async {
    final ApiClient api = ref.read(apiClientProvider);
    final AppConfig config = ref.read(appConfigProvider);

    final Map<String, dynamic> data = await api.post<Map<String, dynamic>>(
      '/auth/otp/verify',
      body: <String, dynamic>{
        'phone': phone,
        'code': code,
        if (password != null && password.isNotEmpty) 'password': password,
        'device_name': deviceName,
        'platform': platform,
        'app_version': config.appVersion,
      },
      authenticated: false,
    );

    await ref.read(tokenStoreProvider).save(
          accessToken: data['access_token'] as String,
          refreshToken: data['refresh_token'] as String,
          userId: data['user_id'] as String,
          deviceId: data['device_id'] as String,
        );

    state = AsyncData<SessionState>(
      SessionState(
        isRestoring: false,
        userId: data['user_id'] as String,
        deviceId: data['device_id'] as String,
      ),
    );
    unawaitedConnect();

    // Push registration needs a session, so it starts here rather than at
    // launch. It is best effort and never blocks sign-in (§30).
    unawaited(ref.read(pushRegistrationProvider).start());
  }

  /// Ends the session and removes every local trace of the account (§58).
  Future<void> signOut() async {
    final ApiClient api = ref.read(apiClientProvider);

    // Retire the push token first, while the access token is still valid:
    // afterwards the server would reject the call and keep sending to a
    // device that has signed out.
    await ref.read(pushRegistrationProvider).stop();

    try {
      await api.post<Map<String, dynamic>>('/auth/logout');
    } on ApiException {
      // The local session is torn down regardless: a failed server call must
      // not leave the user apparently signed in.
    }

    await ref.read(socketClientProvider).disconnect();
    await ref.read(tokenStoreProvider).clear();
    await ref.read(localDatabaseProvider).wipe();

    state = const AsyncData<SessionState>(SessionState(isRestoring: false));
  }
}

final AsyncNotifierProvider<SessionController, SessionState>
    sessionControllerAsyncProvider =
    AsyncNotifierProvider<SessionController, SessionState>(
  SessionController.new,
);

/// Synchronous view of the session for the router, which cannot await.
final Provider<SessionState> sessionControllerProvider =
    Provider<SessionState>((Ref ref) {
  return ref.watch(sessionControllerAsyncProvider).maybeWhen(
        data: (SessionState state) => state,
        orElse: () => const SessionState(),
      );
});
