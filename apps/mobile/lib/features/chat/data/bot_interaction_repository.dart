import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One thing a bot offers in answer to an inline query.
class InlineResult {
  const InlineResult({
    required this.id,
    required this.type,
    required this.title,
    this.description = '',
    this.content = '',
    this.mediaId,
    this.thumbnailMediaId,
  });

  factory InlineResult.fromJson(Map<String, dynamic> json) => InlineResult(
        id: json['id'] as String,
        type: json['type'] as String? ?? 'article',
        title: json['title'] as String? ?? '',
        description: json['description'] as String? ?? '',
        content: json['content'] as String? ?? '',
        mediaId: json['media_id'] as String?,
        thumbnailMediaId: json['thumbnail_media_id'] as String?,
      );

  final String id;
  final String type;
  final String title;
  final String description;
  final String content;
  final String? mediaId;
  final String? thumbnailMediaId;
}

/// An inline query in progress.
class InlineQuery {
  const InlineQuery({required this.id, required this.query});

  factory InlineQuery.fromJson(Map<String, dynamic> json) => InlineQuery(
        id: json['id'] as String,
        query: json['query'] as String? ?? '',
      );

  final String id;
  final String query;
}

/// Talking to bots from the chat screen (§13).
///
/// Distinct from [BotsRepository], which is about managing bots you own. This
/// is about using someone else's: tapping its buttons and asking it for
/// results inline.
class BotInteractionRepository {
  BotInteractionRepository(this._api);

  final ApiClient _api;

  /// Presses a button under a bot's message.
  ///
  /// The bot's answer arrives as an ordinary message or an edit to the one the
  /// button is on, so there is nothing to await here beyond the tap being
  /// accepted.
  Future<void> tap({required String messageId, required String data}) =>
      _api.post<Map<String, dynamic>>(
        '/inline/callbacks',
        body: <String, dynamic>{'message_id': messageId, 'data': data},
      );

  /// Opens an inline query as the user types `@bot …`.
  Future<InlineQuery> openQuery({
    required String bot,
    required String query,
    String offset = '',
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/inline/queries',
      body: <String, dynamic>{
        'bot': bot,
        'query': query,
        'offset': offset,
      },
    );
    return InlineQuery.fromJson(data);
  }

  /// Reads what the bot offered. Empty while it is still thinking.
  Future<List<InlineResult>> results(String queryId) async {
    final Map<String, dynamic> data = await _api
        .get<Map<String, dynamic>>('/inline/queries/$queryId/results');
    return <InlineResult>[
      for (final dynamic entry
          in data['results'] as List<dynamic>? ?? const <dynamic>[])
        InlineResult.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Sends the result the user picked.
  ///
  /// The message is sent by the user, not the bot, and goes through the
  /// ordinary send path — so it lands only where they could have posted it
  /// themselves.
  Future<void> choose({
    required String queryId,
    required String chatId,
    required String resultId,
  }) =>
      _api.post<Map<String, dynamic>>(
        '/inline/queries/$queryId/choose',
        body: <String, dynamic>{'chat_id': chatId, 'result_id': resultId},
      );
}

final Provider<BotInteractionRepository> botInteractionRepositoryProvider =
    Provider<BotInteractionRepository>(
  (Ref ref) => BotInteractionRepository(ref.watch(apiClientProvider)),
);
