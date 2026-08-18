import 'dart:convert';
import 'dart:typed_data';

import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:libsignal_protocol_dart/libsignal_protocol_dart.dart';

/// Where the ratchet's private state lives (§23, §24).
///
/// The Double Ratchet is stateful: every message advances chain keys that the
/// next one depends on. Losing that state does not merely lose history — it
/// breaks the session, and every message afterwards fails to decrypt until the
/// conversation is started again. So it is persisted, and persisted somewhere
/// the operating system protects: the iOS keychain and the Android keystore,
/// never the ordinary database alongside message text.
///
/// Nothing here is invented. The records are serialised by the library itself
/// and stored as opaque bytes; this class knows where they go, not what is in
/// them.
class SecureSignalStore implements SignalProtocolStore {
  SecureSignalStore({
    required FlutterSecureStorage storage,
    required IdentityKeyPair identity,
    required int registrationId,
  })  : _storage = storage,
        _identity = identity,
        _registrationId = registrationId;

  final FlutterSecureStorage _storage;
  final IdentityKeyPair _identity;
  final int _registrationId;

  /// Keys are namespaced so a future store sharing the keychain cannot collide
  /// with this one, and so a wipe can find everything that belongs to it.
  static const String _prefix = 'sobh.signal';
  static const String identityKeyPairKey = '$_prefix.identity';
  static const String registrationIdKey = '$_prefix.registration_id';
  static const String nextPreKeyIdKey = '$_prefix.next_prekey_id';

  String _sessionKey(SignalProtocolAddress address) =>
      '$_prefix.session.${address.getName()}.${address.getDeviceId()}';
  String _preKeyKey(int id) => '$_prefix.prekey.$id';
  String _signedPreKeyKey(int id) => '$_prefix.signed_prekey.$id';
  String _identityKeyFor(SignalProtocolAddress address) =>
      '$_prefix.peer.${address.getName()}.${address.getDeviceId()}';

  // ------------------------------------------------------------- identity

  @override
  Future<IdentityKeyPair> getIdentityKeyPair() async => _identity;

  @override
  Future<int> getLocalRegistrationId() async => _registrationId;

  @override
  Future<bool> saveIdentity(
    SignalProtocolAddress address,
    IdentityKey? identityKey,
  ) async {
    if (identityKey == null) {
      return false;
    }

    final IdentityKey? existing = await getIdentity(address);
    await _storage.write(
      key: _identityKeyFor(address),
      value: base64Encode(identityKey.serialize()),
    );
    // True means "this replaced a different key", which is what tells the UI a
    // peer's identity changed and the safety number needs checking again.
    return existing != null && existing.serialize() != identityKey.serialize();
  }

  @override
  Future<bool> isTrustedIdentity(
    SignalProtocolAddress address,
    IdentityKey? identityKey,
    Direction direction,
  ) async {
    if (identityKey == null) {
      return false;
    }

    final IdentityKey? known = await getIdentity(address);
    if (known == null) {
      // First contact: there is nothing to compare against, so it is trusted
      // on use and pinned from here. The safety number is what lets two people
      // check afterwards that nobody was in the middle at this moment.
      return true;
    }
    // A changed key is refused rather than silently accepted. Accepting it
    // would make a server-side key swap invisible, which is the one attack
    // this whole protocol exists to make detectable.
    return _sameKey(known, identityKey);
  }

  @override
  Future<IdentityKey?> getIdentity(SignalProtocolAddress address) async {
    final String? stored = await _storage.read(key: _identityKeyFor(address));
    if (stored == null) {
      return null;
    }
    return IdentityKey.fromBytes(base64Decode(stored), 0);
  }

  static bool _sameKey(IdentityKey a, IdentityKey b) {
    final Uint8List left = a.serialize();
    final Uint8List right = b.serialize();
    if (left.length != right.length) {
      return false;
    }
    for (int i = 0; i < left.length; i++) {
      if (left[i] != right[i]) {
        return false;
      }
    }
    return true;
  }

  // -------------------------------------------------------------- prekeys

  @override
  Future<PreKeyRecord> loadPreKey(int preKeyId) async {
    final String? stored = await _storage.read(key: _preKeyKey(preKeyId));
    if (stored == null) {
      throw InvalidKeyIdException('no such prekey: $preKeyId');
    }
    return PreKeyRecord.fromBuffer(base64Decode(stored));
  }

  @override
  Future<void> storePreKey(int preKeyId, PreKeyRecord record) => _storage.write(
        key: _preKeyKey(preKeyId),
        value: base64Encode(record.serialize()),
      );

  @override
  Future<bool> containsPreKey(int preKeyId) =>
      _storage.containsKey(key: _preKeyKey(preKeyId));

  @override
  Future<void> removePreKey(int preKeyId) =>
      _storage.delete(key: _preKeyKey(preKeyId));

  // -------------------------------------------------------- signed prekeys

  @override
  Future<SignedPreKeyRecord> loadSignedPreKey(int signedPreKeyId) async {
    final String? stored =
        await _storage.read(key: _signedPreKeyKey(signedPreKeyId));
    if (stored == null) {
      throw InvalidKeyIdException('no such signed prekey: $signedPreKeyId');
    }
    return SignedPreKeyRecord.fromSerialized(base64Decode(stored));
  }

  @override
  Future<List<SignedPreKeyRecord>> loadSignedPreKeys() async {
    final Map<String, String> all = await _storage.readAll();
    return <SignedPreKeyRecord>[
      for (final MapEntry<String, String> entry in all.entries)
        if (entry.key.startsWith('$_prefix.signed_prekey.'))
          SignedPreKeyRecord.fromSerialized(base64Decode(entry.value)),
    ];
  }

  @override
  Future<void> storeSignedPreKey(
    int signedPreKeyId,
    SignedPreKeyRecord record,
  ) =>
      _storage.write(
        key: _signedPreKeyKey(signedPreKeyId),
        value: base64Encode(record.serialize()),
      );

  @override
  Future<bool> containsSignedPreKey(int signedPreKeyId) =>
      _storage.containsKey(key: _signedPreKeyKey(signedPreKeyId));

  @override
  Future<void> removeSignedPreKey(int signedPreKeyId) =>
      _storage.delete(key: _signedPreKeyKey(signedPreKeyId));

  // ------------------------------------------------------------- sessions

  @override
  Future<SessionRecord> loadSession(SignalProtocolAddress address) async {
    final String? stored = await _storage.read(key: _sessionKey(address));
    if (stored == null) {
      // A fresh record rather than an error: the library treats an empty
      // session as "no session yet", which is what starting a conversation is.
      return SessionRecord();
    }
    return SessionRecord.fromSerialized(base64Decode(stored));
  }

  @override
  Future<List<int>> getSubDeviceSessions(String name) async {
    final Map<String, String> all = await _storage.readAll();
    final String prefix = '$_prefix.session.$name.';

    return <int>[
      for (final String key in all.keys)
        if (key.startsWith(prefix))
          if (int.tryParse(key.substring(prefix.length)) case final int id) id,
    ];
  }

  @override
  Future<void> storeSession(
    SignalProtocolAddress address,
    SessionRecord record,
  ) =>
      _storage.write(
        key: _sessionKey(address),
        value: base64Encode(record.serialize()),
      );

  @override
  Future<bool> containsSession(SignalProtocolAddress address) =>
      _storage.containsKey(key: _sessionKey(address));

  @override
  Future<void> deleteSession(SignalProtocolAddress address) =>
      _storage.delete(key: _sessionKey(address));

  @override
  Future<void> deleteAllSessions(String name) async {
    final Map<String, String> all = await _storage.readAll();
    final String prefix = '$_prefix.session.$name.';
    for (final String key in all.keys) {
      if (key.startsWith(prefix)) {
        await _storage.delete(key: key);
      }
    }
  }

  // ----------------------------------------------------------------- wipe

  /// Removes every key and session this store owns.
  ///
  /// Signing out must leave nothing behind that could decrypt the messages
  /// still cached on the device — otherwise "sign out" is a UI state rather
  /// than a security boundary.
  Future<void> wipe() async {
    final Map<String, String> all = await _storage.readAll();
    for (final String key in all.keys) {
      if (key.startsWith(_prefix)) {
        await _storage.delete(key: key);
      }
    }
  }
}
