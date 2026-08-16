import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One search hit. The fields differ per index, so they stay a map rather than
/// four near-identical model classes.
class SearchResult {
  const SearchResult({required this.id, required this.fields, this.highlight});

  factory SearchResult.fromJson(Map<String, dynamic> json) => SearchResult(
        id: json['id'] as String,
        fields: json['fields'] as Map<String, dynamic>? ??
            const <String, dynamic>{},
        highlight: json['highlight'] as Map<String, dynamic>?,
      );

  final String id;
  final Map<String, dynamic> fields;
  final Map<String, dynamic>? highlight;

  String get title =>
      fields['title'] as String? ??
      fields['display_name'] as String? ??
      fields['username'] as String? ??
      '';

  /// The matched snippet when the server highlighted one, and the raw content
  /// otherwise — a result with no context is hard to judge.
  String get snippet {
    final dynamic highlighted = highlight?['content'] ?? highlight?['title'];
    if (highlighted is List<dynamic> && highlighted.isNotEmpty) {
      return '${highlighted.first}';
    }
    if (highlighted is String) {
      return highlighted;
    }
    return fields['content'] as String? ?? fields['lead'] as String? ?? '';
  }

  String? get chatId => fields['chat_id'] as String?;
}

/// Everything one query matched, grouped by index.
class SearchResults {
  const SearchResults({
    required this.query,
    required this.total,
    this.messages = const <SearchResult>[],
    this.users = const <SearchResult>[],
    this.chats = const <SearchResult>[],
    this.news = const <SearchResult>[],
  });

  factory SearchResults.fromJson(Map<String, dynamic> json) => SearchResults(
        query: json['query'] as String? ?? '',
        total: (json['total'] as num?)?.toInt() ?? 0,
        messages: _group(json['messages']),
        users: _group(json['users']),
        chats: _group(json['chats']),
        news: _group(json['news']),
      );

  final String query;
  final int total;
  final List<SearchResult> messages;
  final List<SearchResult> users;
  final List<SearchResult> chats;
  final List<SearchResult> news;

  bool get isEmpty => total == 0;

  static List<SearchResult> _group(dynamic raw) => <SearchResult>[
        for (final dynamic entry in raw as List<dynamic>? ?? const <dynamic>[])
          SearchResult.fromJson(entry as Map<String, dynamic>),
      ];
}

/// Search across messages, people, chats and news (§28).
///
/// Message search is scoped to chats the caller belongs to by the server; the
/// client does not — and could not — enforce that itself.
class SearchRepository {
  SearchRepository(this._api);

  final ApiClient _api;

  Future<SearchResults> search(String query, {String scope = 'all'}) async {
    if (query.trim().isEmpty) {
      return const SearchResults(query: '', total: 0);
    }
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/search',
      query: <String, dynamic>{'q': query, 'scope': scope},
    );
    return SearchResults.fromJson(data);
  }
}

final Provider<SearchRepository> searchRepositoryProvider =
    Provider<SearchRepository>(
  (Ref ref) => SearchRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<SearchResults, String> searchProvider =
    FutureProvider.family<SearchResults, String>(
  (Ref ref, String query) => ref.watch(searchRepositoryProvider).search(query),
);
