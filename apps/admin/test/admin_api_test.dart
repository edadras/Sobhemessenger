import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_admin/core/admin_api.dart';

void main() {
  group('AdminApiException', () {
    test('recognises a permission failure', () {
      const AdminApiException error = AdminApiException(
        code: 'FORBIDDEN',
        message: 'You do not have permission',
        statusCode: 403,
      );
      expect(error.isForbidden, isTrue);
      expect(error.needsSignIn, isFalse);
    });

    test('recognises an expired session', () {
      const AdminApiException error = AdminApiException(
        code: 'TOKEN_EXPIRED',
        message: 'Access token has expired',
        statusCode: 401,
      );
      expect(error.needsSignIn, isTrue);
      expect(error.isForbidden, isFalse);
    });

    test('a plain failure is neither', () {
      const AdminApiException error = AdminApiException(
        code: 'VALIDATION_FAILED',
        message: 'Bad request',
        statusCode: 422,
      );
      expect(error.isForbidden, isFalse);
      expect(error.needsSignIn, isFalse);
    });
  });
}
