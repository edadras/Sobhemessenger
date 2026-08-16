import 'dart:async';

import 'package:dio/dio.dart';

import '../config/app_config.dart';
import '../storage/token_store.dart';
import 'api_exception.dart';

/// The single HTTP entry point to the SOBH backend.
///
/// It unwraps the response envelope (§66) so callers only ever see `data` or an
/// [ApiException], and it owns access-token refresh: a 401 triggers exactly one
/// refresh, and every request that was in flight waits for that one result
/// rather than each firing its own refresh.
class ApiClient {
  ApiClient({
    required AppConfig config,
    required TokenStore tokenStore,
    Dio? dio,
  })  : _config = config,
        _tokenStore = tokenStore,
        _dio = dio ?? Dio() {
    _dio.options = BaseOptions(
      baseUrl: '${_config.apiBaseUrl}/api/v1',
      connectTimeout: const Duration(seconds: 10),
      receiveTimeout: const Duration(seconds: 30),
      sendTimeout: const Duration(seconds: 30),
      headers: <String, String>{
        'Content-Type': 'application/json',
        'X-Client-Version': _config.appVersion,
        'X-Protocol-Version': '${AppConfig.protocolVersion}',
      },
      // Envelope errors are handled here, not thrown by Dio, so any status is
      // "valid" as far as the transport is concerned.
      validateStatus: (int? status) => status != null && status < 500,
    );

    _dio.interceptors.add(
      InterceptorsWrapper(
        onRequest: _attachAccessToken,
        onError: _onError,
      ),
    );
  }

  final AppConfig _config;
  final TokenStore _tokenStore;
  final Dio _dio;

  /// Guards the refresh so concurrent 401s collapse into one network call.
  Future<bool>? _refreshInFlight;

  /// Emits when refresh fails and the user must sign in again (§56).
  final StreamController<void> _sessionExpired =
      StreamController<void>.broadcast();
  Stream<void> get onSessionExpired => _sessionExpired.stream;

  Future<void> _attachAccessToken(
    RequestOptions options,
    RequestInterceptorHandler handler,
  ) async {
    if (options.extra['skipAuth'] != true) {
      final String? token = await _tokenStore.readAccessToken();
      if (token != null) {
        options.headers['Authorization'] = 'Bearer $token';
      }
    }
    handler.next(options);
  }

  Future<void> _onError(
    DioException error,
    ErrorInterceptorHandler handler,
  ) async {
    final Response<dynamic>? response = error.response;
    final bool isAuthFailure = response?.statusCode == 401;
    final bool alreadyRetried = error.requestOptions.extra['retried'] == true;
    final bool skipsAuth = error.requestOptions.extra['skipAuth'] == true;

    if (!isAuthFailure || alreadyRetried || skipsAuth) {
      handler.next(error);
      return;
    }

    final bool refreshed = await _refreshTokens();
    if (!refreshed) {
      handler.next(error);
      return;
    }

    // Replay the original request once, with the new token.
    error.requestOptions.extra['retried'] = true;
    try {
      final Response<dynamic> retried =
          await _dio.fetch<dynamic>(error.requestOptions);
      handler.resolve(retried);
    } on DioException catch (retryError) {
      handler.next(retryError);
    }
  }

  Future<bool> _refreshTokens() {
    // A refresh already running: join it instead of starting another.
    return _refreshInFlight ??= _performRefresh().whenComplete(() {
      _refreshInFlight = null;
    });
  }

  Future<bool> _performRefresh() async {
    final String? refreshToken = await _tokenStore.readRefreshToken();
    if (refreshToken == null) {
      return false;
    }

    try {
      final Response<dynamic> response = await _dio.post<dynamic>(
        '/auth/refresh',
        data: <String, String>{'refresh_token': refreshToken},
        options: Options(
          extra: <String, dynamic>{'skipAuth': true, 'retried': true},
        ),
      );

      final Map<String, dynamic> body = _asMap(response.data);
      if (body['success'] != true) {
        // The refresh token was rotated away or revoked: this session is over.
        await _tokenStore.clear();
        _sessionExpired.add(null);
        return false;
      }

      final Map<String, dynamic> data = _asMap(body['data']);
      await _tokenStore.save(
        accessToken: data['access_token'] as String,
        refreshToken: data['refresh_token'] as String,
        userId: data['user_id'] as String,
        deviceId: data['device_id'] as String,
      );
      return true;
    } on DioException {
      // A network failure is not a revoked session; keep the tokens so the
      // next attempt can succeed once connectivity returns.
      return false;
    }
  }

  Future<T> get<T>(
    String path, {
    Map<String, dynamic>? query,
    bool authenticated = true,
  }) =>
      _send<T>(
        () => _dio.get<dynamic>(
          path,
          queryParameters: query,
          options: _options(authenticated),
        ),
      );

  Future<T> post<T>(
    String path, {
    Object? body,
    bool authenticated = true,
  }) =>
      _send<T>(
        () => _dio.post<dynamic>(
          path,
          data: body,
          options: _options(authenticated),
        ),
      );

  Future<T> patch<T>(String path, {Object? body}) => _send<T>(
        () => _dio.patch<dynamic>(path, data: body, options: _options(true)),
      );

  Future<T> put<T>(String path, {Object? body}) => _send<T>(
        () => _dio.put<dynamic>(path, data: body, options: _options(true)),
      );

  Future<T> delete<T>(String path) =>
      _send<T>(() => _dio.delete<dynamic>(path, options: _options(true)));

  Options _options(bool authenticated) =>
      Options(extra: <String, dynamic>{'skipAuth': !authenticated});

  /// Runs a request and unwraps the envelope.
  Future<T> _send<T>(Future<Response<dynamic>> Function() request) async {
    late final Response<dynamic> response;
    try {
      response = await request();
    } on DioException catch (error) {
      throw _toApiException(error);
    }

    final Map<String, dynamic> body = _asMap(response.data);
    if (body['success'] == true) {
      return body['data'] as T;
    }

    final Map<String, dynamic> error = _asMap(body['error']);
    throw ApiException(
      code: error['code'] as String? ?? ApiErrorCode.unknown,
      message: error['message'] as String? ?? 'Request failed',
      statusCode: response.statusCode ?? 0,
      fields: (error['fields'] as Map<String, dynamic>?)?.map(
        (String key, dynamic value) => MapEntry<String, String>(key, '$value'),
      ),
      retryAfter: _retryAfter(response),
    );
  }

  ApiException _toApiException(DioException error) {
    final Response<dynamic>? response = error.response;
    if (response != null) {
      // A 5xx never carries a usable envelope.
      return ApiException(
        code: ApiErrorCode.serverError,
        message: 'The server could not handle this request',
        statusCode: response.statusCode ?? 0,
      );
    }

    return switch (error.type) {
      DioExceptionType.connectionTimeout ||
      DioExceptionType.sendTimeout ||
      DioExceptionType.receiveTimeout =>
        const ApiException(
          code: ApiErrorCode.timeout,
          message: 'The request timed out',
          statusCode: 0,
        ),
      DioExceptionType.cancel => const ApiException(
          code: ApiErrorCode.cancelled,
          message: 'The request was cancelled',
          statusCode: 0,
        ),
      _ => const ApiException(
          code: ApiErrorCode.network,
          message: 'Could not reach the server',
          statusCode: 0,
        ),
    };
  }

  Duration? _retryAfter(Response<dynamic> response) {
    final String? header = response.headers.value('retry-after');
    final int? seconds = header == null ? null : int.tryParse(header);
    return seconds == null ? null : Duration(seconds: seconds);
  }

  static Map<String, dynamic> _asMap(dynamic value) =>
      value is Map<String, dynamic> ? value : const <String, dynamic>{};

  void dispose() {
    _dio.close(force: true);
    unawaited(_sessionExpired.close());
  }
}
