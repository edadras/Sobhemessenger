import 'dart:async';
import 'dart:io';

import 'package:firebase_messaging/firebase_messaging.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// Registers this device for push and keeps the token current (§30).
///
/// The token is registered after sign-in and unregistered on sign-out, so a
/// device that has been signed out stops receiving another account's
/// notifications. Rotation is handled by listening to the refresh stream
/// rather than polling: FCM decides when a token changes, not us.
class PushRegistration {
  PushRegistration({required ApiClient api}) : _api = api;

  final ApiClient _api;

  final FlutterLocalNotificationsPlugin _local =
      FlutterLocalNotificationsPlugin();

  StreamSubscription<String>? _refreshes;
  StreamSubscription<RemoteMessage>? _foreground;
  String? _registered;

  /// Asks for permission, registers the token and starts following rotations.
  ///
  /// Every step is best effort: push is a convenience, and a device that
  /// cannot register must still be able to use the app.
  Future<void> start() async {
    if (kIsWeb) {
      // The web build receives realtime over the socket it already holds open.
      return;
    }

    try {
      final NotificationSettings permission =
          await FirebaseMessaging.instance.requestPermission();
      if (permission.authorizationStatus == AuthorizationStatus.denied) {
        return;
      }

      await _configureLocalNotifications();

      final String? token = await FirebaseMessaging.instance.getToken();
      if (token != null) {
        await _register(token);
      }

      _refreshes ??=
          FirebaseMessaging.instance.onTokenRefresh.listen(_register);
      _foreground ??= FirebaseMessaging.onMessage.listen(_showForeground);
    } on Object {
      // A missing Firebase configuration, a denied permission or an offline
      // device all land here. None of them is worth failing sign-in over.
      return;
    }
  }

  Future<void> _configureLocalNotifications() async {
    // A push that arrives while the app is open does not raise a system
    // notification by itself, so one is posted locally.
    await _local.initialize(
      const InitializationSettings(
        android: AndroidInitializationSettings('@mipmap/ic_launcher'),
        iOS: DarwinInitializationSettings(),
      ),
    );
  }

  Future<void> _register(String token) async {
    if (token == _registered) {
      return;
    }
    await _api.post<Map<String, dynamic>>(
      '/notifications/tokens',
      body: <String, dynamic>{
        'provider': Platform.isIOS ? 'apns' : 'fcm',
        'token': token,
        'locale': Platform.localeName.split('_').first,
      },
    );
    _registered = token;
  }

  Future<void> _showForeground(RemoteMessage message) async {
    final RemoteNotification? notification = message.notification;
    if (notification == null) {
      return;
    }
    await _local.show(
      notification.hashCode,
      notification.title,
      notification.body,
      const NotificationDetails(
        android: AndroidNotificationDetails(
          'sobh_messages',
          'Messages',
          importance: Importance.high,
          priority: Priority.high,
        ),
        iOS: DarwinNotificationDetails(),
      ),
    );
  }

  /// Retires the token so the server stops sending to a signed-out device.
  Future<void> stop() async {
    final String? token = _registered;
    _registered = null;

    await _refreshes?.cancel();
    await _foreground?.cancel();
    _refreshes = null;
    _foreground = null;

    if (token == null) {
      return;
    }
    try {
      await _api.delete<Map<String, dynamic>>('/notifications/tokens');
    } on Object {
      // The server also retires a token when the provider reports it invalid,
      // so a failure here is recovered from without the client's help.
      return;
    }
  }
}

final Provider<PushRegistration> pushRegistrationProvider =
    Provider<PushRegistration>(
  (Ref ref) => PushRegistration(api: ref.watch(apiClientProvider)),
);
