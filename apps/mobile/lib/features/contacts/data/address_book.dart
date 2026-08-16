import 'package:flutter_contacts/flutter_contacts.dart' as device;
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'contacts_repository.dart';

/// Reads the device address book (§54).
///
/// It is a separate object from [ContactsRepository] so the discovery flow can
/// be exercised without a device: the repository takes a plain list of numbers
/// and does not care where they came from.
class AddressBook {
  const AddressBook();

  /// Requests permission and reads every phone number on the device.
  ///
  /// Returns null when the user refuses. That is a decision to respect and
  /// report, not an error to retry.
  Future<List<LocalContact>?> read() async {
    if (!await device.FlutterContacts.requestPermission(readonly: true)) {
      return null;
    }

    final List<device.Contact> entries =
        await device.FlutterContacts.getContacts(withProperties: true);

    final List<LocalContact> book = <LocalContact>[];
    for (final device.Contact entry in entries) {
      for (final device.Phone phone in entry.phones) {
        final String normalized = normalizeNumber(phone.number);
        if (normalized.isEmpty) {
          continue;
        }
        book.add(
          LocalContact(
            phone: normalized,
            firstName: entry.name.first,
            lastName: entry.name.last,
          ),
        );
      }
    }
    return book;
  }

  /// Strips the formatting people put in phone numbers so the digest matches
  /// what the server computed at registration.
  ///
  /// The server stores E.164, so anything that is not already in that form and
  /// cannot be converted is dropped rather than guessed at: a wrong guess would
  /// silently fail to match, which is harder to diagnose than no match at all.
  static String normalizeNumber(String raw) {
    final StringBuffer digits = StringBuffer();
    bool leadingPlus = false;

    for (int i = 0; i < raw.length; i++) {
      final String char = raw[i];
      if (char == '+' && digits.isEmpty) {
        leadingPlus = true;
        continue;
      }
      final int code = char.codeUnitAt(0);
      // Latin, Persian and Arabic-Indic digits all appear in Iranian address
      // books, often in the same entry.
      if (code >= 0x30 && code <= 0x39) {
        digits.write(char);
      } else if (code >= 0x06F0 && code <= 0x06F9) {
        digits.write(String.fromCharCode(code - 0x06F0 + 0x30));
      } else if (code >= 0x0660 && code <= 0x0669) {
        digits.write(String.fromCharCode(code - 0x0660 + 0x30));
      }
    }

    final String number = digits.toString();
    if (number.length < 8) {
      return '';
    }
    if (leadingPlus) {
      return '+$number';
    }
    // A local Iranian number: 0912… is +98912….
    if (number.startsWith('00')) {
      return '+${number.substring(2)}';
    }
    if (number.startsWith('0')) {
      return '+98${number.substring(1)}';
    }
    return '';
  }
}

final Provider<AddressBook> addressBookProvider =
    Provider<AddressBook>((Ref ref) => const AddressBook());
