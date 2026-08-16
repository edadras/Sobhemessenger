import 'package:flutter_secure_storage/flutter_secure_storage.dart';

/// Stores credentials in the platform keychain / keystore (§23, §33).
///
/// Tokens never touch shared preferences or the app database, so a filesystem
/// backup or a rooted-device dump of app data does not expose a session.
class TokenStore {
  TokenStore({FlutterSecureStorage? storage})
      : _storage = storage ??
            const FlutterSecureStorage(
              aOptions: AndroidOptions(encryptedSharedPreferences: true),
              iOptions: IOSOptions(
                accessibility: KeychainAccessibility.first_unlock_this_device,
              ),
            );

  final FlutterSecureStorage _storage;

  static const String _accessTokenKey = 'sobh.access_token';
  static const String _refreshTokenKey = 'sobh.refresh_token';
  static const String _userIdKey = 'sobh.user_id';
  static const String _deviceIdKey = 'sobh.device_id';

  Future<String?> readAccessToken() => _storage.read(key: _accessTokenKey);

  Future<String?> readRefreshToken() => _storage.read(key: _refreshTokenKey);

  Future<String?> readUserId() => _storage.read(key: _userIdKey);

  Future<String?> readDeviceId() => _storage.read(key: _deviceIdKey);

  Future<bool> get hasSession async => await readRefreshToken() != null;

  Future<void> save({
    required String accessToken,
    required String refreshToken,
    required String userId,
    required String deviceId,
  }) async {
    await Future.wait<void>(<Future<void>>[
      _storage.write(key: _accessTokenKey, value: accessToken),
      _storage.write(key: _refreshTokenKey, value: refreshToken),
      _storage.write(key: _userIdKey, value: userId),
      _storage.write(key: _deviceIdKey, value: deviceId),
    ]);
  }

  /// Wipes the session. Called on sign-out and when a refresh is rejected.
  Future<void> clear() async {
    await Future.wait<void>(<Future<void>>[
      _storage.delete(key: _accessTokenKey),
      _storage.delete(key: _refreshTokenKey),
      _storage.delete(key: _userIdKey),
      _storage.delete(key: _deviceIdKey),
    ]);
  }
}
