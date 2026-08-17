/// Error codes returned by the backend (§66). The client switches on these,
/// never on the human-readable message, which is free to change.
abstract final class ApiErrorCode {
  const ApiErrorCode._();

  // Transport-level, produced by the client itself.
  static const String network = 'NETWORK_ERROR';
  static const String timeout = 'TIMEOUT';
  static const String cancelled = 'CANCELLED';
  static const String serverError = 'INTERNAL_ERROR';
  static const String unknown = 'UNKNOWN_ERROR';

  // Authentication.
  static const String unauthorized = 'UNAUTHORIZED';
  static const String tokenExpired = 'TOKEN_EXPIRED';
  static const String sessionRevoked = 'SESSION_REVOKED';
  static const String invalidOtp = 'INVALID_OTP';
  static const String otpExpired = 'OTP_EXPIRED';
  static const String otpAttemptsExceeded = 'OTP_ATTEMPTS_EXCEEDED';
  static const String twoStepRequired = 'TWO_STEP_REQUIRED';
  static const String invalidPassword = 'INVALID_PASSWORD';
  static const String accountBanned = 'ACCOUNT_BANNED';
  static const String tooManyDevices = 'TOO_MANY_DEVICES';

  // Chats and messages.
  static const String notChatMember = 'NOT_CHAT_MEMBER';
  static const String chatNotFound = 'CHAT_NOT_FOUND';
  static const String messageNotFound = 'MESSAGE_NOT_FOUND';
  static const String messageTooOld = 'MESSAGE_TOO_OLD_TO_EDIT';
  static const String permissionDenied = 'CHAT_PERMISSION_DENIED';
  static const String slowMode = 'SLOW_MODE_ACTIVE';

  /// The generic not-found code, used where a resource has no more specific
  /// one — a user with no encrypted-chat devices, for instance.
  static const String notFound = 'NOT_FOUND';

  static const String rateLimited = 'RATE_LIMITED';
  static const String validationFailed = 'VALIDATION_FAILED';
  static const String featureDisabled = 'FEATURE_DISABLED';
}

/// A failed API call.
class ApiException implements Exception {
  const ApiException({
    required this.code,
    required this.message,
    required this.statusCode,
    this.fields,
    this.retryAfter,
  });

  final String code;
  final String message;
  final int statusCode;

  /// Per-field validation detail, when the server supplied it.
  final Map<String, String>? fields;

  /// How long to wait before retrying, from the `Retry-After` header.
  final Duration? retryAfter;

  /// Whether retrying the same request could plausibly succeed. The outbox
  /// uses this to decide between retrying a send and marking it failed (§7).
  bool get isRetryable => switch (code) {
        ApiErrorCode.network ||
        ApiErrorCode.timeout ||
        ApiErrorCode.serverError ||
        ApiErrorCode.rateLimited =>
          true,
        _ => false,
      };

  /// Whether the user has to sign in again.
  bool get requiresReauthentication => switch (code) {
        ApiErrorCode.unauthorized ||
        ApiErrorCode.tokenExpired ||
        ApiErrorCode.sessionRevoked =>
          true,
        _ => false,
      };

  /// True when the device is offline rather than the request being rejected.
  bool get isOffline =>
      code == ApiErrorCode.network || code == ApiErrorCode.timeout;

  @override
  String toString() => 'ApiException($code, status: $statusCode): $message';
}
