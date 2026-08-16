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
