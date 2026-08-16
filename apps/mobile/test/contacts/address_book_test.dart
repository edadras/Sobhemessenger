import 'dart:convert';

import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/contacts/data/address_book.dart';

void main() {
  group('AddressBook.normalizeNumber', () {
    test('keeps a number that is already E.164', () {
      expect(AddressBook.normalizeNumber('+989121234567'), '+989121234567');
    });

    test('strips the punctuation people put in phone numbers', () {
      expect(
        AddressBook.normalizeNumber('+98 (912) 123-4567'),
        '+989121234567',
      );
    });

    // Iranian address books are full of Persian and Arabic-Indic digits, often
    // mixed with Latin ones in the same entry.
    test('folds Persian digits to Latin', () {
      expect(AddressBook.normalizeNumber('۰۹۱۲۱۲۳۴۵۶۷'), '+989121234567');
    });

    test('folds Arabic-Indic digits to Latin', () {
      expect(AddressBook.normalizeNumber('٠٩١٢١٢٣٤٥٦٧'), '+989121234567');
    });

    test('converts a local number to the Iranian country code', () {
      expect(AddressBook.normalizeNumber('09121234567'), '+989121234567');
    });

    test('converts the 00 international prefix to +', () {
      expect(AddressBook.normalizeNumber('00989121234567'), '+989121234567');
    });

    // A number that cannot be resolved to E.164 is dropped rather than
    // guessed at: a wrong guess silently fails to match, which is harder to
    // diagnose than no match at all.
    test('drops a number with no country information', () {
      expect(AddressBook.normalizeNumber('9121234567'), '');
    });

    test('drops a number that is too short to be one', () {
      expect(AddressBook.normalizeNumber('12345'), '');
      expect(AddressBook.normalizeNumber(''), '');
    });

    test('drops letters and other noise entirely', () {
      expect(AddressBook.normalizeNumber('call me'), '');
    });
  });

  // The digest the client uploads must be exactly HMAC-SHA256 over the
  // normalised number under the server's pepper. If this drifts, discovery
  // silently stops matching anyone.
  group('discovery digest', () {
    test('is HMAC-SHA256 of the normalised number under the pepper', () {
      const String pepper = 'test-pepper-value';
      final String normalized = AddressBook.normalizeNumber('۰۹۱۲۱۲۳۴۵۶۷');

      final String digest = Hmac(sha256, utf8.encode(pepper))
          .convert(utf8.encode(normalized))
          .toString();

      expect(normalized, '+989121234567');
      expect(digest.length, 64, reason: 'SHA-256 is 32 bytes, hex encoded');
      // The same number formatted differently must produce the same digest,
      // or the same person would be discovered twice.
      final String reformatted =
          AddressBook.normalizeNumber('+98 912 123 4567');
      expect(
        Hmac(sha256, utf8.encode(pepper))
            .convert(utf8.encode(reformatted))
            .toString(),
        digest,
      );
    });
  });
}
