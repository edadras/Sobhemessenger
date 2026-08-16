import 'dart:convert';

import 'package:crypto/crypto.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One entry in the address book.
class Contact {
  const Contact({
    required this.userId,
    required this.displayName,
    this.firstName = '',
    this.lastName = '',
    this.username,
    this.isFavorite = false,
    this.isMutual = false,
    this.lastSeen,
  });

  factory Contact.fromJson(Map<String, dynamic> json) => Contact(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        firstName: json['first_name'] as String? ?? '',
        lastName: json['last_name'] as String? ?? '',
        username: json['username'] as String?,
        isFavorite: json['is_favorite'] as bool? ?? false,
        isMutual: json['is_mutual'] as bool? ?? false,
        lastSeen: json['last_seen'] == null
            ? null
            : DateTime.parse(json['last_seen'] as String).toLocal(),
      );

  final String userId;
  final String displayName;
  final String firstName;
  final String lastName;
  final String? username;
  final bool isFavorite;
  final bool isMutual;
  final DateTime? lastSeen;

  /// The name to show: what the user called them locally wins over the name
  /// they chose for themselves, which is what an address book is for.
  String get label {
    final String local = '$firstName $lastName'.trim();
    return local.isNotEmpty ? local : displayName;
  }
}

/// How the server wants phone numbers hashed before they are uploaded.
class DiscoveryParameters {
  const DiscoveryParameters({
    required this.algorithm,
    required this.pepper,
    required this.maxBatchSize,
  });

  factory DiscoveryParameters.fromJson(Map<String, dynamic> json) =>
      DiscoveryParameters(
        algorithm: json['algorithm'] as String,
        pepper: json['pepper'] as String,
        maxBatchSize: (json['max_batch_size'] as num).toInt(),
      );

  final String algorithm;
  final String pepper;
  final int maxBatchSize;
}

/// One local address-book entry, before hashing.
class LocalContact {
  const LocalContact({
    required this.phone,
    this.firstName = '',
    this.lastName = '',
  });

  final String phone;
  final String firstName;
  final String lastName;
}

/// Address book and discovery (§54).
///
/// A phone number never leaves the device. Each one is hashed with HMAC-SHA256
/// under a pepper the server publishes, and only the digest is uploaded — so a
/// captured request, or a log of one, contains no numbers.
class ContactsRepository {
  ContactsRepository(this._api);

  final ApiClient _api;

  Future<List<Contact>> list() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/contacts');
    return _contacts(data['contacts']);
  }

  Future<List<Contact>> blocked() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/contacts/blocked');
    return _contacts(data['blocked']);
  }

  Future<DiscoveryParameters> discoveryParameters() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/contacts/discovery-parameters');
    return DiscoveryParameters.fromJson(data);
  }

  /// Uploads hashed numbers and returns how many resolved to accounts.
  ///
  /// The book is uploaded in batches of the size the server asks for, and only
  /// the first batch replaces: the rest add, or each batch would erase the one
  /// before it.
  Future<int> sync(List<LocalContact> book) async {
    if (book.isEmpty) {
      return 0;
    }

    final DiscoveryParameters params = await discoveryParameters();
    final Hmac hmac = Hmac(sha256, utf8.encode(params.pepper));

    int matched = 0;
    bool replace = true;
    for (int offset = 0; offset < book.length; offset += params.maxBatchSize) {
      final List<LocalContact> batch = book.sublist(
        offset,
        (offset + params.maxBatchSize).clamp(0, book.length),
      );

      final Map<String, dynamic> result = await _api.post<Map<String, dynamic>>(
        '/contacts/sync',
        body: <String, dynamic>{
          'replace': replace,
          'entries': <Map<String, dynamic>>[
            for (final LocalContact entry in batch)
              <String, dynamic>{
                'digest': hmac.convert(utf8.encode(entry.phone)).toString(),
                'first_name': entry.firstName,
                'last_name': entry.lastName,
              },
          ],
        },
      );
      matched +=
          (result['matched'] as List<dynamic>? ?? const <dynamic>[]).length;
      replace = false;
    }
    return matched;
  }

  Future<void> add(
    String userId, {
    String firstName = '',
    String lastName = '',
  }) =>
      _api.post<Map<String, dynamic>>(
        '/contacts',
        body: <String, dynamic>{
          'user_id': userId,
          'first_name': firstName,
          'last_name': lastName,
        },
      );

  Future<void> remove(String userId) =>
      _api.delete<Map<String, dynamic>>('/contacts/$userId');

  Future<void> setFavorite(String userId, bool favorite) =>
      _api.put<Map<String, dynamic>>(
        '/contacts/$userId/favorite',
        body: <String, dynamic>{'favorite': favorite},
      );

  Future<void> block(String userId, {String reason = ''}) =>
      _api.post<Map<String, dynamic>>(
        '/contacts/blocked',
        body: <String, dynamic>{
          'user_id': userId,
          'reason': reason,
        },
      );

  Future<void> unblock(String userId) =>
      _api.delete<Map<String, dynamic>>('/contacts/blocked/$userId');

  static List<Contact> _contacts(dynamic raw) => <Contact>[
        for (final dynamic entry in raw as List<dynamic>? ?? const <dynamic>[])
          Contact.fromJson(entry as Map<String, dynamic>),
      ];
}

final Provider<ContactsRepository> contactsRepositoryProvider =
    Provider<ContactsRepository>(
  (Ref ref) => ContactsRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<Contact>> contactListProvider =
    FutureProvider<List<Contact>>(
  (Ref ref) => ref.watch(contactsRepositoryProvider).list(),
);

final FutureProvider<List<Contact>> blockedContactsProvider =
    FutureProvider<List<Contact>>(
  (Ref ref) => ref.watch(contactsRepositoryProvider).blocked(),
);
