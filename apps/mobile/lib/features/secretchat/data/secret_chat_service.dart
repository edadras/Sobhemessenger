import 'dart:convert';
import 'dart:typed_data';

import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_secure_storage/flutter_secure_storage.dart';
import 'package:libsignal_protocol_dart/libsignal_protocol_dart.dart';
import 'package:uuid/uuid.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';
import 'signal_store.dart';

/// Secret chats, on the device (§24).
///
/// The server is a key directory and a mailbox: it publishes public key
/// material, hands out each one-time prekey exactly once, and holds opaque
/// ciphertext until the recipient acknowledges it. Every private key, every
/// derived secret and every ratchet step is here, and never leaves.
///
/// The protocol itself — X3DH to agree a first secret, the Double Ratchet for
/// everything after — is not implemented here. §84.16-17 forbid inventing or
/// assembling it, so it is `libsignal_protocol_dart`, a port of Signal's own
/// library. This class supplies the transport, the storage and the identity;
/// it does not do cryptography.

/// How many one-time prekeys to publish at once.
///
/// Each is consumed by one new conversation. Running out does not break
/// existing sessions — a bundle without one still works, with slightly weaker
/// forward secrecy for that first message — but it is worth avoiding.
const int prekeyBatchSize = 100;

/// A device that can be talked to, as the directory describes it.
class DeviceBundle {
  const DeviceBundle({
    required this.deviceId,
    required this.userId,
    required this.registrationId,
    required this.identityKey,
    required this.signedPrekeyId,
    required this.signedPrekey,
    required this.prekeySignature,
    this.oneTimePrekeyId,
    this.oneTimePrekey,
  });

  factory DeviceBundle.fromJson(Map<String, dynamic> json) {
    final Map<String, dynamic>? oneTime =
        json['one_time_prekey'] as Map<String, dynamic>?;
    return DeviceBundle(
      deviceId: json['device_id'] as String,
      userId: json['user_id'] as String,
      registrationId: (json['registration_id'] as num).toInt(),
      identityKey: json['identity_key'] as String,
      signedPrekeyId: (json['signed_prekey_id'] as num).toInt(),
      signedPrekey: json['signed_prekey'] as String,
      prekeySignature: json['prekey_signature'] as String,
      oneTimePrekeyId: (oneTime?['key_id'] as num?)?.toInt(),
      oneTimePrekey: oneTime?['public_key'] as String?,
    );
  }

  final String deviceId;
  final String userId;
  final int registrationId;
  final String identityKey;
  final int signedPrekeyId;
  final String signedPrekey;
  final String prekeySignature;
  final int? oneTimePrekeyId;
  final String? oneTimePrekey;
}

/// One ciphertext waiting in the mailbox.
class SecretEnvelope {
  const SecretEnvelope({
    required this.id,
    required this.chatId,
    required this.senderDeviceId,
    required this.ciphertext,
    required this.messageType,
    required this.clientMessageId,
    required this.createdAt,
  });

  factory SecretEnvelope.fromJson(Map<String, dynamic> json) => SecretEnvelope(
        id: json['id'] as String,
        chatId: json['chat_id'] as String,
        senderDeviceId: json['sender_device_id'] as String,
        ciphertext: json['ciphertext'] as String,
        messageType: (json['message_type'] as num).toInt(),
        clientMessageId: json['client_message_id'] as String,
        createdAt: DateTime.parse(json['created_at'] as String).toLocal(),
      );

  final String id;
  final String chatId;
  final String senderDeviceId;
  final String ciphertext;

  /// 3 is a pre-key message, which starts a session; 1 is an ordinary one.
  /// These are the protocol's own numbers, not ours.
  final int messageType;
  final String clientMessageId;
  final DateTime createdAt;
}

/// A decrypted message, with the envelope it arrived in.
class DecryptedMessage {
  const DecryptedMessage({
    required this.envelope,
    required this.plaintext,
  });

  final SecretEnvelope envelope;
  final String plaintext;
}

/// Raised when a message cannot be decrypted.
///
/// It is deliberately not silent. A message that will not open is either a
/// broken session or someone tampering, and both are things the person in the
/// conversation should be told about rather than shown a blank.
class UndecryptableMessage implements Exception {
  const UndecryptableMessage(this.envelope, this.reason);

  final SecretEnvelope envelope;
  final Object reason;

  @override
  String toString() => 'UndecryptableMessage(${envelope.id}): $reason';
}

class SecretChatService {
  SecretChatService({
    required ApiClient api,
    required FlutterSecureStorage storage,
  })  : _api = api,
        _storage = storage;

  final ApiClient _api;
  final FlutterSecureStorage _storage;
  final Uuid _uuid = const Uuid();

  SecureSignalStore? _store;

  /// Prepares this device: loads its identity or creates one, and publishes
  /// the public half.
  ///
  /// Safe to call on every start. The identity is generated once and kept; a
  /// device that regenerated it would appear to every contact as a new,
  /// unverified party each time it launched.
  Future<void> ensureRegistered() async {
    final IdentityKeyPair identity = await _loadOrCreateIdentity();
    final int registrationId = await _loadOrCreateRegistrationId();

    _store = SecureSignalStore(
      storage: _storage,
      identity: identity,
      registrationId: registrationId,
    );

    // The signed prekey is generated once here and rotated by
    // [rotateSignedPrekey]; publishing a new one on every start would make
    // every stored bundle stale for no benefit.
    final int signedPrekeyId = await _currentSignedPrekeyId() ?? 1;
    if (!await _store!.containsSignedPreKey(signedPrekeyId)) {
      final SignedPreKeyRecord signed =
          generateSignedPreKey(identity, signedPrekeyId);
      await _store!.storeSignedPreKey(signedPrekeyId, signed);
      await _storage.write(
        key: '${SecureSignalStore.identityKeyPairKey}.signed_id',
        value: '$signedPrekeyId',
      );
      await _publishBundle(signedPrekeyId, withPrekeys: true);
    } else {
      await topUpPrekeys();
    }
  }

  /// Publishes more one-time prekeys when the server says they are running low.
  Future<void> topUpPrekeys() async {
    final Map<String, dynamic> status =
        await _api.get<Map<String, dynamic>>('/secret/keys/status');
    // The server's field is `needs_upload`. Reading a name it does not send
    // would always find null, default to false, and quietly never top up —
    // the device would run out of one-time prekeys and every new conversation
    // with it would fall back to the weaker signed-prekey-only handshake.
    final bool needsUpload = status['needs_upload'] as bool? ?? false;
    if (!needsUpload) {
      return;
    }

    final int signedPrekeyId = await _currentSignedPrekeyId() ?? 1;
    await _publishBundle(signedPrekeyId, withPrekeys: true);
  }

  /// Generates a fresh signed prekey and publishes it.
  ///
  /// Rotating limits how much past traffic a compromised key exposes; the old
  /// one is kept in the store so messages already in flight against it can
  /// still be opened.
  Future<void> rotateSignedPrekey() async {
    final SecureSignalStore store = _requireStore();
    final IdentityKeyPair identity = await store.getIdentityKeyPair();

    final int nextId = (await _currentSignedPrekeyId() ?? 1) + 1;
    final SignedPreKeyRecord signed = generateSignedPreKey(identity, nextId);
    await store.storeSignedPreKey(nextId, signed);
    await _storage.write(
      key: '${SecureSignalStore.identityKeyPairKey}.signed_id',
      value: '$nextId',
    );
    await _publishBundle(nextId, withPrekeys: false);
  }

  Future<void> _publishBundle(
    int signedPrekeyId, {
    required bool withPrekeys,
  }) async {
    final SecureSignalStore store = _requireStore();
    final IdentityKeyPair identity = await store.getIdentityKeyPair();
    final SignedPreKeyRecord signed = await store.loadSignedPreKey(signedPrekeyId);

    final List<Map<String, dynamic>> oneTime = <Map<String, dynamic>>[];
    if (withPrekeys) {
      final int start = await _nextPrekeyId();
      final List<PreKeyRecord> generated =
          generatePreKeys(start, prekeyBatchSize);
      for (final PreKeyRecord record in generated) {
        await store.storePreKey(record.id, record);
        oneTime.add(<String, dynamic>{
          'key_id': record.id,
          'public_key': base64Encode(record.getKeyPair().publicKey.serialize()),
        });
      }
      await _storage.write(
        key: SecureSignalStore.nextPreKeyIdKey,
        value: '${start + prekeyBatchSize}',
      );
    }

    await _api.post<Map<String, dynamic>>(
      '/secret/keys',
      body: <String, dynamic>{
        'registration_id': await store.getLocalRegistrationId(),
        'identity_key': base64Encode(identity.getPublicKey().serialize()),
        'signed_prekey_id': signedPrekeyId,
        'signed_prekey':
            base64Encode(signed.getKeyPair().publicKey.serialize()),
        'prekey_signature': base64Encode(signed.signature),
        'one_time_prekeys': oneTime,
      },
    );
  }

  // ---------------------------------------------------------------- chats

  /// Opens the encrypted chat with someone, creating it on first contact.
  ///
  /// Unlike an ordinary chat this cannot be created locally and reconciled
  /// later: the chat id is what every envelope is addressed to, and a
  /// locally-invented one would leave the ciphertext unroutable. It is also
  /// idempotent server-side, so calling it again returns the same conversation
  /// rather than a second one.
  Future<String> openChat(String peerUserId) async {
    final Map<String, dynamic> result = await _api.post<Map<String, dynamic>>(
      '/secret/chats',
      body: <String, dynamic>{'user_id': peerUserId},
    );
    return result['chat_id'] as String;
  }

  // ------------------------------------------------------------- sessions

  /// Reads the devices a user can be reached on.
  ///
  /// Every device gets its own session: a message is encrypted once per device
  /// rather than once per person, which is what makes reading on a second
  /// device possible without sharing a key between them.
  Future<List<DeviceBundle>> bundlesFor(String userId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/secret/keys/$userId');
    return <DeviceBundle>[
      for (final dynamic entry
          in data['bundles'] as List<dynamic>? ?? const <dynamic>[])
        DeviceBundle.fromJson(entry as Map<String, dynamic>),
    ];
  }

  /// Establishes a session with one device from its published bundle.
  ///
  /// This is X3DH: the library combines the identity, signed and one-time keys
  /// into a shared secret without either side being online at the same time.
  Future<void> startSessionWith(DeviceBundle bundle) async {
    final SecureSignalStore store = _requireStore();
    final SignalProtocolAddress address = _addressOf(bundle.deviceId);

    final PreKeyBundle preKeyBundle = PreKeyBundle(
      bundle.registrationId,
      // The address carries the device; this field is the protocol's own
      // device number, and one session per address is what we want.
      1,
      bundle.oneTimePrekeyId,
      bundle.oneTimePrekey == null
          ? null
          : Curve.decodePoint(base64Decode(bundle.oneTimePrekey!), 0),
      bundle.signedPrekeyId,
      Curve.decodePoint(base64Decode(bundle.signedPrekey), 0),
      base64Decode(bundle.prekeySignature),
      IdentityKey.fromBytes(base64Decode(bundle.identityKey), 0),
    );

    final SessionBuilder builder = SessionBuilder(
      store, store, store, store, address,
    );
    // This verifies the prekey signature against the identity key. A directory
    // that swapped a key would fail here rather than quietly succeeding, which
    // is the point of signing it.
    await builder.processPreKeyBundle(preKeyBundle);
  }

  /// Whether there is already a session with a device.
  Future<bool> hasSessionWith(String deviceId) =>
      _requireStore().containsSession(_addressOf(deviceId));

  // ------------------------------------------------------------- messages

  /// Encrypts one message for every device of the recipient and posts them.
  ///
  /// Returns the client message id, which is the same across every device's
  /// copy — so the conversation shows one message rather than one per device
  /// the other person happens to own.
  Future<String> send({
    required String chatId,
    required String recipientUserId,
    required String plaintext,
  }) async {
    final SecureSignalStore store = _requireStore();
    final String clientMessageId = _uuid.v4();

    final List<DeviceBundle> devices = await bundlesFor(recipientUserId);
    if (devices.isEmpty) {
      throw StateError('that user has no device that can receive secret chats');
    }

    for (final DeviceBundle device in devices) {
      if (!await hasSessionWith(device.deviceId)) {
        await startSessionWith(device);
      }

      final SessionCipher cipher = SessionCipher(
        store, store, store, store, _addressOf(device.deviceId),
      );
      final CiphertextMessage encrypted = await cipher.encrypt(
        Uint8List.fromList(utf8.encode(plaintext)),
      );

      await _api.post<Map<String, dynamic>>(
        '/secret/messages',
        body: <String, dynamic>{
          'chat_id': chatId,
          'recipient_device_id': device.deviceId,
          'ciphertext': base64Encode(encrypted.serialize()),
          'message_type': encrypted.getType(),
          'client_message_id': clientMessageId,
        },
      );
    }

    return clientMessageId;
  }

  /// Fetches and decrypts whatever is waiting, then acknowledges it.
  ///
  /// Acknowledgement deletes the ciphertext on the server, so it happens only
  /// for envelopes that actually opened: acknowledging one that failed would
  /// destroy the only copy of a message that a later fix might have recovered.
  Future<List<DecryptedMessage>> receive() async {
    final SecureSignalStore store = _requireStore();

    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/secret/inbox');
    final List<dynamic> raw =
        data['envelopes'] as List<dynamic>? ?? const <dynamic>[];

    final List<DecryptedMessage> opened = <DecryptedMessage>[];
    final List<String> acknowledge = <String>[];
    final List<UndecryptableMessage> failures = <UndecryptableMessage>[];

    for (final dynamic entry in raw) {
      final SecretEnvelope envelope =
          SecretEnvelope.fromJson(entry as Map<String, dynamic>);

      try {
        final SessionCipher cipher = SessionCipher(
          store, store, store, store, _addressOf(envelope.senderDeviceId),
        );
        final Uint8List bytes = base64Decode(envelope.ciphertext);

        final Uint8List plaintext = switch (envelope.messageType) {
          // A pre-key message carries the material that starts the session, so
          // receiving one is how the other side of X3DH completes.
          CiphertextMessage.prekeyType =>
            await cipher.decrypt(PreKeySignalMessage(bytes)),
          _ => await cipher.decryptFromSignal(SignalMessage.fromSerialized(bytes)),
        };

        opened.add(
          DecryptedMessage(
            envelope: envelope,
            plaintext: utf8.decode(plaintext),
          ),
        );
        acknowledge.add(envelope.id);
      } on Object catch (error) {
        // One bad envelope must not stop the rest of the inbox from opening.
        failures.add(UndecryptableMessage(envelope, error));
      }
    }

    if (acknowledge.isNotEmpty) {
      await _api.post<Map<String, dynamic>>(
        '/secret/inbox/ack',
        body: <String, dynamic>{'ids': acknowledge},
      );
    }

    if (failures.isNotEmpty && opened.isEmpty) {
      // Nothing opened at all: that is worth surfacing rather than looking
      // like an empty inbox.
      throw failures.first;
    }
    return opened;
  }

  // ---------------------------------------------------------- verification

  /// The safety number two people compare to check nobody is in the middle.
  ///
  /// It is derived from both identity keys, so it matches on both devices only
  /// if each is talking to the party it believes it is. Comparing it out of
  /// band — aloud, in person — is the one check the server cannot forge.
  Future<String> safetyNumber({
    required String localUserId,
    required String remoteUserId,
    required String remoteDeviceId,
  }) async {
    final SecureSignalStore store = _requireStore();
    final IdentityKeyPair local = await store.getIdentityKeyPair();
    final IdentityKey? remote = await store.getIdentity(
      _addressOf(remoteDeviceId),
    );
    if (remote == null) {
      throw StateError('no identity is known for that device yet');
    }

    final NumericFingerprintGenerator generator =
        NumericFingerprintGenerator(5200);
    final Fingerprint fingerprint = generator.createFor(
      1,
      Uint8List.fromList(utf8.encode(localUserId)),
      local.getPublicKey(),
      Uint8List.fromList(utf8.encode(remoteUserId)),
      remote,
    );
    return fingerprint.displayableFingerprint.getDisplayText();
  }

  /// Removes every private key and session on this device.
  Future<void> wipe() => _requireStore().wipe();

  // ------------------------------------------------------------- internals

  SecureSignalStore _requireStore() {
    final SecureSignalStore? store = _store;
    if (store == null) {
      throw StateError('secret chats are not set up on this device yet');
    }
    return store;
  }

  /// One address per device, so each device holds its own ratchet.
  SignalProtocolAddress _addressOf(String deviceId) =>
      SignalProtocolAddress(deviceId, 1);

  Future<IdentityKeyPair> _loadOrCreateIdentity() async {
    final String? stored =
        await _storage.read(key: SecureSignalStore.identityKeyPairKey);
    if (stored != null) {
      return IdentityKeyPair.fromSerialized(base64Decode(stored));
    }

    final IdentityKeyPair generated = generateIdentityKeyPair();
    await _storage.write(
      key: SecureSignalStore.identityKeyPairKey,
      value: base64Encode(generated.serialize()),
    );
    return generated;
  }

  Future<int> _loadOrCreateRegistrationId() async {
    final String? stored =
        await _storage.read(key: SecureSignalStore.registrationIdKey);
    if (stored != null) {
      final int? parsed = int.tryParse(stored);
      if (parsed != null) {
        return parsed;
      }
    }

    final int generated = generateRegistrationId(false);
    await _storage.write(
      key: SecureSignalStore.registrationIdKey,
      value: '$generated',
    );
    return generated;
  }

  Future<int?> _currentSignedPrekeyId() async {
    final String? stored = await _storage.read(
      key: '${SecureSignalStore.identityKeyPairKey}.signed_id',
    );
    return stored == null ? null : int.tryParse(stored);
  }

  /// Prekey ids never restart from zero, because reusing one would let a
  /// stored bundle name a key that now means something else.
  Future<int> _nextPrekeyId() async {
    final String? stored = await _storage.read(
      key: SecureSignalStore.nextPreKeyIdKey,
    );
    return int.tryParse(stored ?? '') ?? 1;
  }
}

final Provider<SecretChatService> secretChatServiceProvider =
    Provider<SecretChatService>(
  (Ref ref) => SecretChatService(
    api: ref.watch(apiClientProvider),
    storage: ref.watch(secureStorageProvider),
  ),
);
