import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A bot the caller owns (§13).
///
/// A bot is an ordinary account carrying `is_bot`, so [userId] is both its
/// identity as a bot and its identity in any chat it belongs to.
class Bot {
  const Bot({
    required this.userId,
    required this.ownerId,
    required this.displayName,
    this.username,
    this.description = '',
    this.about = '',
    this.canJoinGroups = true,
    this.privacyMode = true,
    this.inlineEnabled = false,
    this.inlinePlaceholder = '',
    this.isActive = true,
    required this.createdAt,
  });

  factory Bot.fromJson(Map<String, dynamic> json) => Bot(
        userId: json['user_id'] as String,
        ownerId: json['owner_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        username: json['username'] as String?,
        description: json['description'] as String? ?? '',
        about: json['about'] as String? ?? '',
        canJoinGroups: json['can_join_groups'] as bool? ?? true,
        privacyMode: json['privacy_mode'] as bool? ?? true,
        inlineEnabled: json['inline_enabled'] as bool? ?? false,
        inlinePlaceholder: json['inline_placeholder'] as String? ?? '',
        isActive: json['is_active'] as bool? ?? true,
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
      );

  final String userId;
  final String ownerId;
  final String displayName;
  final String? username;
  final String description;
  final String about;
  final bool canJoinGroups;
  final bool privacyMode;
  final bool inlineEnabled;
  final String inlinePlaceholder;
  final bool isActive;
  final DateTime createdAt;

  String get handle => username == null ? '' : '@$username';
}

/// A live token, as it is listed: the secret is not part of it.
class BotToken {
  const BotToken({
    required this.id,
    required this.prefix,
    this.label = '',
    required this.createdAt,
    this.lastUsedAt,
  });

  factory BotToken.fromJson(Map<String, dynamic> json) => BotToken(
        id: json['id'] as String,
        prefix: json['prefix'] as String? ?? '',
        label: json['label'] as String? ?? '',
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
        lastUsedAt: json['last_used_at'] == null
            ? null
            : DateTime.parse(json['last_used_at'] as String).toLocal(),
      );

  final String id;

  /// Enough to tell two tokens apart in a list, and useless as a credential.
  final String prefix;
  final String label;
  final DateTime createdAt;
  final DateTime? lastUsedAt;
}

/// A token as it comes back from being issued.
///
/// [secret] exists in this object and nowhere else, ever. The server keeps only
/// a hash, so it cannot be shown again — which is why the screen that receives
/// one has to make the user copy it before moving on.
class IssuedToken {
  const IssuedToken({
    required this.id,
    required this.secret,
    required this.prefix,
    this.label = '',
  });

  factory IssuedToken.fromJson(Map<String, dynamic> json) => IssuedToken(
        id: json['id'] as String,
        secret: json['token'] as String? ?? '',
        prefix: json['prefix'] as String? ?? '',
        label: json['label'] as String? ?? '',
      );

  final String id;
  final String secret;
  final String prefix;
  final String label;
}

/// One command a bot advertises.
class BotCommand {
  const BotCommand({
    required this.command,
    required this.description,
    this.position = 0,
    this.locale,
  });

  factory BotCommand.fromJson(Map<String, dynamic> json) => BotCommand(
        command: json['command'] as String? ?? '',
        description: json['description'] as String? ?? '',
        position: (json['position'] as num?)?.toInt() ?? 0,
        locale: json['locale'] as String?,
      );

  final String command;
  final String description;
  final int position;
  final String? locale;

  Map<String, dynamic> toJson() => <String, dynamic>{
        'command': command,
        'description': description,
        'position': position,
        if (locale != null) 'locale': locale,
      };
}

/// A webhook registration, never including its signing secret.
class BotWebhook {
  const BotWebhook({
    required this.url,
    this.maxConnections = 40,
    this.allowedUpdates = const <String>[],
    this.lastError = '',
    this.lastDeliveryAt,
  });

  factory BotWebhook.fromJson(Map<String, dynamic> json) => BotWebhook(
        url: json['url'] as String? ?? '',
        maxConnections: (json['max_connections'] as num?)?.toInt() ?? 40,
        allowedUpdates: <String>[
          for (final dynamic entry
              in json['allowed_updates'] as List<dynamic>? ?? const <dynamic>[])
            entry as String,
        ],
        lastError: json['last_error'] as String? ?? '',
        lastDeliveryAt: json['last_delivery_at'] == null
            ? null
            : DateTime.parse(json['last_delivery_at'] as String).toLocal(),
      );

  final String url;
  final int maxConnections;
  final List<String> allowedUpdates;
  final String lastError;
  final DateTime? lastDeliveryAt;
}

/// Registering and managing bots (§13).
///
/// This is the owner's side. What a bot itself calls lives under `/bot` and is
/// authenticated by a bot token, which this app never holds.
class BotsRepository {
  BotsRepository(this._api);

  final ApiClient _api;

  Future<List<Bot>> list() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/bots');
    return <Bot>[
      for (final dynamic entry
          in data['bots'] as List<dynamic>? ?? const <dynamic>[])
        Bot.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<Bot> get(String botId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/bots/$botId');
    return Bot.fromJson(data);
  }

  /// Registers a bot and returns it with its first token.
  ///
  /// The token is returned once and cannot be fetched again.
  Future<(Bot, IssuedToken)> register({
    required String username,
    required String displayName,
    String description = '',
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/bots',
      body: <String, dynamic>{
        'username': username,
        'display_name': displayName,
        'description': description,
      },
    );
    return (
      Bot.fromJson(data['bot'] as Map<String, dynamic>),
      IssuedToken.fromJson(data['token'] as Map<String, dynamic>),
    );
  }

  Future<Bot> updateSettings(
    String botId, {
    String? displayName,
    String? description,
    String? about,
    bool? canJoinGroups,
    bool? privacyMode,
    bool? inlineEnabled,
    String? inlinePlaceholder,
    bool? isActive,
  }) async {
    final Map<String, dynamic> data = await _api.patch<Map<String, dynamic>>(
      '/bots/$botId',
      body: <String, dynamic>{
        if (displayName != null) 'display_name': displayName,
        if (description != null) 'description': description,
        if (about != null) 'about': about,
        if (canJoinGroups != null) 'can_join_groups': canJoinGroups,
        if (privacyMode != null) 'privacy_mode': privacyMode,
        if (inlineEnabled != null) 'inline_enabled': inlineEnabled,
        if (inlinePlaceholder != null) 'inline_placeholder': inlinePlaceholder,
        if (isActive != null) 'is_active': isActive,
      },
    );
    return Bot.fromJson(data);
  }

  Future<List<BotToken>> tokens(String botId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/bots/$botId/tokens');
    return <BotToken>[
      for (final dynamic entry
          in data['tokens'] as List<dynamic>? ?? const <dynamic>[])
        BotToken.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<IssuedToken> issueToken(String botId, {String label = ''}) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/bots/$botId/tokens',
      body: <String, dynamic>{'label': label},
    );
    return IssuedToken.fromJson(data);
  }

  Future<void> revokeToken(String botId, String tokenId) =>
      _api.delete<dynamic>('/bots/$botId/tokens/$tokenId');

  Future<List<BotCommand>> commands(String botId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/bots/$botId/commands');
    return <BotCommand>[
      for (final dynamic entry
          in data['commands'] as List<dynamic>? ?? const <dynamic>[])
        BotCommand.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Replaces the whole list, so what is sent is exactly what users will see.
  Future<void> setCommands(String botId, List<BotCommand> commands) =>
      _api.put<dynamic>(
        '/bots/$botId/commands',
        body: <String, dynamic>{
          'commands': <Map<String, dynamic>>[
            for (final BotCommand command in commands) command.toJson(),
          ],
        },
      );

  Future<BotWebhook?> webhook(String botId) async {
    final Map<String, dynamic>? data =
        await _api.get<Map<String, dynamic>?>('/bots/$botId/webhook');
    if (data == null || (data['url'] as String? ?? '').isEmpty) {
      return null;
    }
    return BotWebhook.fromJson(data);
  }

  /// Registers a webhook and returns its signing secret, shown once.
  Future<String> setWebhook(
    String botId, {
    required String url,
    int maxConnections = 40,
    List<String> allowedUpdates = const <String>[],
  }) async {
    final Map<String, dynamic> data = await _api.put<Map<String, dynamic>>(
      '/bots/$botId/webhook',
      body: <String, dynamic>{
        'url': url,
        'max_connections': maxConnections,
        'allowed_updates': allowedUpdates,
      },
    );
    return data['secret'] as String? ?? '';
  }

  Future<void> deleteWebhook(String botId) =>
      _api.delete<dynamic>('/bots/$botId/webhook');
}

final Provider<BotsRepository> botsRepositoryProvider =
    Provider<BotsRepository>(
  (Ref ref) => BotsRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<Bot>> botListProvider = FutureProvider<List<Bot>>(
  (Ref ref) => ref.watch(botsRepositoryProvider).list(),
);

final FutureProviderFamily<List<BotToken>, String> botTokensProvider =
    FutureProvider.family<List<BotToken>, String>(
  (Ref ref, String botId) => ref.watch(botsRepositoryProvider).tokens(botId),
);

final FutureProviderFamily<BotWebhook?, String> botWebhookProvider =
    FutureProvider.family<BotWebhook?, String>(
  (Ref ref, String botId) => ref.watch(botsRepositoryProvider).webhook(botId),
);
