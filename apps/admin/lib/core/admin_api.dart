import 'package:dio/dio.dart';

/// Typed client for the SOBH admin API.
///
/// The panel never talks to the database or to any service directly: it is a
/// consumer of the same REST API as any other client, so every permission
/// check and audit entry happens server-side (§31, §32).
class AdminApi {
  AdminApi({required String baseUrl, Dio? dio}) : _dio = dio ?? Dio() {
    _dio.options = BaseOptions(
      baseUrl: '$baseUrl/api/v1',
      connectTimeout: const Duration(seconds: 10),
      receiveTimeout: const Duration(seconds: 30),
      headers: const <String, String>{'Content-Type': 'application/json'},
      validateStatus: (int? status) => status != null && status < 500,
    );
  }

  final Dio _dio;
  String? _accessToken;

  void setAccessToken(String? token) => _accessToken = token;

  Options get _options => Options(
        headers: <String, String>{
          if (_accessToken != null) 'Authorization': 'Bearer $_accessToken',
        },
      );

  Future<Map<String, dynamic>> dashboard() async =>
      _get<Map<String, dynamic>>('/admin/dashboard');

  Future<List<dynamic>> timeSeries(String metric, {int days = 30}) async {
    final Map<String, dynamic> data =
        await _get<Map<String, dynamic>>('/admin/analytics/$metric?days=$days');
    return data['points'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<List<dynamic>> users({String query = '', String status = ''}) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/admin/users?q=${Uri.encodeQueryComponent(query)}'
      '&status=${Uri.encodeQueryComponent(status)}',
    );
    return data['users'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<void> setUserStatus(String userId, String status, String reason) =>
      _send(
        '/admin/users/$userId/status',
        'PUT',
        <String, String>{'status': status, 'reason': reason},
      );

  Future<List<dynamic>> reports({String status = 'open'}) async {
    final Map<String, dynamic> data =
        await _get<Map<String, dynamic>>('/admin/reports?status=$status');
    return data['reports'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<void> resolveReport(
    String reportId,
    String status,
    String resolution,
  ) =>
      _send(
        '/admin/reports/$reportId',
        'PUT',
        <String, String>{'status': status, 'resolution': resolution},
      );

  Future<List<dynamic>> featureFlags() async {
    final Map<String, dynamic> data =
        await _get<Map<String, dynamic>>('/feature-flags');
    return data['flags'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<void> setFeatureFlag(String key, bool enabled, int rollout) => _send(
        '/admin/feature-flags/$key',
        'PUT',
        <String, dynamic>{'enabled': enabled, 'rollout_percent': rollout},
      );

  Future<List<dynamic>> auditLog({String action = ''}) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/admin/audit-log?action=${Uri.encodeQueryComponent(action)}',
    );
    return data['entries'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<List<dynamic>> editorialArticles({String status = ''}) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/editorial/articles?status=${Uri.encodeQueryComponent(status)}',
    );
    return data['articles'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<void> setArticleStatus(String articleId, String status) => _send(
        '/editorial/articles/$articleId/status',
        'POST',
        <String, String>{'status': status},
      );

  // ----------------------------------------------------------------- bans

  Future<List<dynamic>> bans({int limit = 50, int offset = 0}) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/admin/bans?limit=$limit&offset=$offset',
    );
    return data['bans'] as List<dynamic>? ?? const <dynamic>[];
  }

  /// Bans an account, a chat, or an account within one chat.
  ///
  /// [expiresAt] absent means indefinite. The distinction is the whole point
  /// of the field: a ban with an end date is a suspension, one without is
  /// permanent, and the panel should not have to guess which it just issued.
  Future<String> createBan({
    required String scope,
    required String reason,
    String? userId,
    String? chatId,
    DateTime? expiresAt,
  }) async {
    final Map<String, dynamic> data = await _post<Map<String, dynamic>>(
      '/admin/bans',
      <String, dynamic>{
        'scope': scope,
        'reason': reason,
        if (userId != null) 'user_id': userId,
        if (chatId != null) 'chat_id': chatId,
        if (expiresAt != null)
          'expires_at': expiresAt.toUtc().toIso8601String(),
      },
    );
    return data['ban_id'] as String? ?? '';
  }

  Future<void> liftBan(String banId) =>
      _send('/admin/bans/$banId', 'DELETE', const <String, dynamic>{});

  // --------------------------------------------------------- operator roles

  /// Grants an operator role. What the role may do is the role's business —
  /// the panel names it, the server holds the permission list.
  Future<void> grantRole(String userId, String role) => _send(
        '/admin/users/$userId/roles',
        'POST',
        <String, String>{'role': role},
      );

  Future<void> revokeRole(String userId, String roleKey) => _send(
        '/admin/users/$userId/roles/$roleKey',
        'DELETE',
        const <String, dynamic>{},
      );

  // --------------------------------------------------------- search indices

  /// Rebuilds one search index from PostgreSQL.
  ///
  /// The database is the source of truth and the index is a derived copy, so
  /// this is the repair for a copy that has drifted — after a restore, or a
  /// spell when the indexer was down. It is slow and it is meant to be: it
  /// walks the table.
  Future<void> reindex(String index) => _send(
        '/admin/search/reindex/$index',
        'POST',
        const <String, dynamic>{},
      );

  // ------------------------------------------------------------- newsroom

  Future<List<dynamic>> authors() async {
    final Map<String, dynamic> data =
        await _get<Map<String, dynamic>>('/editorial/authors');
    return data['authors'] as List<dynamic>? ?? const <dynamic>[];
  }

  /// Creates an author, or edits one when [id] is supplied.
  ///
  /// A byline is not the same thing as an account: a wire service or a desk
  /// has no user to attach, so [userId] is optional.
  Future<void> saveAuthor({
    required String displayName,
    String? id,
    String? userId,
    String bio = '',
    bool isActive = true,
  }) =>
      _send('/editorial/authors', 'PUT', <String, dynamic>{
        if (id != null) 'id': id,
        if (userId != null) 'user_id': userId,
        'display_name': displayName,
        'bio': bio,
        'is_active': isActive,
      });

  Future<List<dynamic>> articleGallery(String articleId) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/editorial/articles/$articleId/gallery',
    );
    return data['items'] as List<dynamic>? ?? const <dynamic>[];
  }

  /// Replaces the whole gallery, which is what the endpoint does: the order of
  /// the list is the order of the images, so sending a subset would delete the
  /// rest rather than leave them in place.
  Future<void> replaceGallery(
    String articleId,
    List<Map<String, dynamic>> items,
  ) =>
      _send(
        '/editorial/articles/$articleId/gallery',
        'PUT',
        <String, dynamic>{'items': items},
      );

  Future<List<dynamic>> translations(String articleId) async {
    final Map<String, dynamic> data = await _get<Map<String, dynamic>>(
      '/editorial/articles/$articleId/translations',
    );
    return data['translations'] as List<dynamic>? ?? const <dynamic>[];
  }

  Future<void> saveTranslation(
    String articleId,
    String locale, {
    required String title,
    String subtitle = '',
    String body = '',
    String source = 'human',
  }) =>
      _send(
        '/editorial/articles/$articleId/translations/$locale',
        'PUT',
        <String, dynamic>{
          'title': title,
          'subtitle': subtitle,
          'body': body,
          'source': source,
        },
      );

  /// Signs off a translation.
  ///
  /// A machine translation nobody has read is not the same thing as a
  /// translated article, and this is the difference between the two.
  Future<void> approveTranslation(String articleId, String locale) => _send(
        '/editorial/articles/$articleId/translations/$locale/approve',
        'POST',
        const <String, dynamic>{},
      );

  Future<void> deleteTranslation(String articleId, String locale) => _send(
        '/editorial/articles/$articleId/translations/$locale',
        'DELETE',
        const <String, dynamic>{},
      );

  Future<List<dynamic>> categories() async {
    final Map<String, dynamic> data =
        await _get<Map<String, dynamic>>('/editorial/categories');
    return data['categories'] as List<dynamic>? ?? const <dynamic>[];
  }

  /// Creates or edits a category.
  ///
  /// The upsert keys on the slug, and the names are per locale — a category is
  /// one thing with a name in each language the app ships, not one category
  /// per translation.
  Future<void> saveCategory({
    required String slug,
    required Map<String, String> names,
    int position = 0,
    bool isActive = true,
  }) =>
      _send('/editorial/categories', 'PUT', <String, dynamic>{
        'slug': slug,
        'names': names,
        'position': position,
        'is_active': isActive,
      });

  /// Deletes a category.
  ///
  /// The server refuses one that still has articles filed under it, so this
  /// cannot orphan a story — the refusal is the answer, and the panel shows
  /// it rather than pre-empting it with a rule of its own that could drift.
  Future<void> deleteCategory(String categoryId) => _send(
        '/editorial/categories/$categoryId',
        'DELETE',
        const <String, dynamic>{},
      );

  // ------------------------------------------------------------- anti-spam

  /// The spam score held against one account.
  ///
  /// An account with no score is not an error — it is the ordinary state of
  /// almost everyone — so the server answers with an empty score rather than a
  /// 404, and this returns it unchanged.
  Future<Map<String, dynamic>> spamScore(String userId) =>
      _get<Map<String, dynamic>>('/admin/spam-scores/$userId');

  /// Lifts a restriction. Moderation is auditable server-side, so nothing is
  /// recorded here beyond making the call.
  Future<void> liftSpamScore(String userId) =>
      _send('/admin/spam-scores/$userId', 'DELETE', const <String, dynamic>{});

  /// A POST whose answer the caller needs. `_send` discards the body, which
  /// is right for the calls that only report success and wrong for one that
  /// hands back an id.
  Future<T> _post<T>(String path, Object body) async {
    final Response<dynamic> response = await _dio.post<dynamic>(
      path,
      data: body,
      options: _options,
    );
    return _unwrap<T>(response);
  }

  Future<T> _get<T>(String path) async {
    final Response<dynamic> response =
        await _dio.get<dynamic>(path, options: _options);
    return _unwrap<T>(response);
  }

  Future<void> _send(String path, String method, Object body) async {
    final Response<dynamic> response = await _dio.request<dynamic>(
      path,
      data: body,
      options: Options(
        method: method,
        headers: _options.headers,
        validateStatus: (int? status) => status != null && status < 500,
      ),
    );
    _unwrap<dynamic>(response);
  }

  /// Unwraps the shared response envelope, turning a failure into an
  /// [AdminApiException] carrying the server's stable error code (§66).
  T _unwrap<T>(Response<dynamic> response) {
    final Map<String, dynamic> body = response.data is Map<String, dynamic>
        ? response.data as Map<String, dynamic>
        : <String, dynamic>{};

    if (body['success'] == true) {
      return body['data'] as T;
    }

    final Map<String, dynamic> error =
        body['error'] as Map<String, dynamic>? ?? <String, dynamic>{};
    throw AdminApiException(
      code: error['code'] as String? ?? 'UNKNOWN_ERROR',
      message: error['message'] as String? ?? 'Request failed',
      statusCode: response.statusCode ?? 0,
    );
  }
}

class AdminApiException implements Exception {
  const AdminApiException({
    required this.code,
    required this.message,
    required this.statusCode,
  });

  final String code;
  final String message;
  final int statusCode;

  bool get isForbidden => code == 'FORBIDDEN' || statusCode == 403;
  bool get needsSignIn => statusCode == 401;

  @override
  String toString() => 'AdminApiException($code): $message';
}
