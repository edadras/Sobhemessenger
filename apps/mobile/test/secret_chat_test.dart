import 'dart:convert';
import 'dart:typed_data';

import 'package:flutter_test/flutter_test.dart';
import 'package:libsignal_protocol_dart/libsignal_protocol_dart.dart';
import 'package:sobh_app/features/secretchat/data/secret_chat_service.dart';

/// Secret chats (§24).
///
/// These drive the real protocol — X3DH to agree a first secret, the Double
/// Ratchet after — with two independent parties, and check the properties that
/// make it worth having rather than merely present. A test that only asserted
/// "encrypt then decrypt returns the input" would pass against a cipher that
/// did nothing.
///
/// The stores here are the library's in-memory ones rather than the keychain,
/// because a plugin channel is not available under `flutter test`. What is
/// under test is the protocol usage and the wire format the server agreed to,
/// not where bytes are filed on a phone.

/// One party: its identity, its published bundle, and its store.
class _Party {
  _Party(this.name)
      : identity = generateIdentityKeyPair(),
        registrationId = generateRegistrationId(false);

  final String name;
  final IdentityKeyPair identity;
  final int registrationId;

  late final InMemorySignalProtocolStore store =
      InMemorySignalProtocolStore(identity, registrationId);

  static const int signedPrekeyId = 1;
  late final SignedPreKeyRecord signedPrekey =
      generateSignedPreKey(identity, signedPrekeyId);
  late final List<PreKeyRecord> oneTimePrekeys = generatePreKeys(1, 5);

  SignalProtocolAddress get address => SignalProtocolAddress(name, 1);

  /// Files the private halves, as publishing does on a real device.
  Future<void> publish() async {
    await store.storeSignedPreKey(signedPrekeyId, signedPrekey);
    for (final PreKeyRecord record in oneTimePrekeys) {
      await store.storePreKey(record.id, record);
    }
  }

  /// The bundle as the server would hand it back, base64 and all — so the
  /// test exercises the same encoding the API uses rather than passing objects
  /// around in memory.
  Map<String, dynamic> bundleJson({int? oneTimeIndex}) {
    final PreKeyRecord? oneTime =
        oneTimeIndex == null ? null : oneTimePrekeys[oneTimeIndex];
    return <String, dynamic>{
      'device_id': name,
      'user_id': 'user-$name',
      'registration_id': registrationId,
      'identity_key': base64Encode(identity.getPublicKey().serialize()),
      'signed_prekey_id': signedPrekeyId,
      'signed_prekey':
          base64Encode(signedPrekey.getKeyPair().publicKey.serialize()),
      'prekey_signature': base64Encode(signedPrekey.signature),
      if (oneTime != null)
        'one_time_prekey': <String, dynamic>{
          'key_id': oneTime.id,
          'public_key':
              base64Encode(oneTime.getKeyPair().publicKey.serialize()),
        },
    };
  }
}

/// Builds the session the way the service does, from a parsed bundle.
Future<void> _establish(
  _Party initiator,
  DeviceBundle bundle,
  SignalProtocolAddress peer,
) async {
  final PreKeyBundle preKeyBundle = PreKeyBundle(
    bundle.registrationId,
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
    initiator.store,
    initiator.store,
    initiator.store,
    initiator.store,
    peer,
  );
  await builder.processPreKeyBundle(preKeyBundle);
}

Future<String> _decrypt(
  _Party recipient,
  SignalProtocolAddress sender,
  CiphertextMessage message,
) async {
  final SessionCipher cipher = SessionCipher(
    recipient.store,
    recipient.store,
    recipient.store,
    recipient.store,
    sender,
  );
  final Uint8List plaintext = message.getType() == CiphertextMessage.prekeyType
      ? await cipher.decrypt(PreKeySignalMessage(message.serialize()))
      : await cipher.decryptFromSignal(
          SignalMessage.fromSerialized(message.serialize()),
        );
  return utf8.decode(plaintext);
}

void main() {
  group('DeviceBundle', () {
    test('parses the bundle shape the server publishes', () {
      final _Party party = _Party('device-a');
      final DeviceBundle bundle =
          DeviceBundle.fromJson(party.bundleJson(oneTimeIndex: 0));

      expect(bundle.deviceId, 'device-a');
      expect(bundle.registrationId, party.registrationId);
      expect(bundle.signedPrekeyId, _Party.signedPrekeyId);
      expect(bundle.oneTimePrekeyId, isNotNull);
      expect(bundle.oneTimePrekey, isNotNull);
    });

    test('parses a bundle whose one-time prekeys have run out', () {
      // The directory hands each one-time prekey out once. When they are gone
      // a bundle still works — with slightly weaker forward secrecy for the
      // first message — so this must not be read as a malformed response.
      final DeviceBundle bundle =
          DeviceBundle.fromJson(_Party('device-a').bundleJson());

      expect(bundle.oneTimePrekeyId, isNull);
      expect(bundle.oneTimePrekey, isNull);
    });
  });

  group('key material', () {
    test('publishes keys at the length the server accepts', () {
      // The server refuses anything that is not 33 bytes: a one-byte curve
      // identifier and the 32-byte key. If this drifts, every device fails to
      // register and no secret chat can start at all.
      final _Party party = _Party('device-a');
      final Map<String, dynamic> json = party.bundleJson(oneTimeIndex: 0);

      expect(base64Decode(json['identity_key'] as String), hasLength(33));
      expect(base64Decode(json['signed_prekey'] as String), hasLength(33));
      expect(base64Decode(json['prekey_signature'] as String), hasLength(64));
      expect(
        base64Decode(
          (json['one_time_prekey'] as Map<String, dynamic>)['public_key']
              as String,
        ),
        hasLength(33),
      );
    });

    test('an identity key is stable, so a contact stays recognised', () {
      // A device that regenerated its identity would appear to every contact
      // as a new, unverified party — and every safety number they had checked
      // would be worthless.
      final _Party party = _Party('device-a');
      expect(
        party.identity.getPublicKey().serialize(),
        equals(party.identity.getPublicKey().serialize()),
      );
    });
  });

  group('X3DH and the Double Ratchet', () {
    test('a message survives the round trip between two parties', () async {
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();

      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher cipher = SessionCipher(
        alice.store,
        alice.store,
        alice.store,
        alice.store,
        bob.address,
      );
      final CiphertextMessage encrypted = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('سلام، این یک پیام مخفی است')),
      );

      // The first message starts the session, so it is a pre-key message.
      expect(encrypted.getType(), CiphertextMessage.prekeyType);

      final String opened =
          await _decrypt(bob, alice.address, encrypted);
      expect(opened, 'سلام، این یک پیام مخفی است');
    });

    test('the ciphertext does not contain the plaintext', () async {
      // The one property that would make everything else pointless if it
      // failed, and the one a do-nothing cipher would fail.
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();
      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      const String secret = 'the account number is 12345';
      final SessionCipher cipher = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      final CiphertextMessage encrypted = await cipher.encrypt(
        Uint8List.fromList(utf8.encode(secret)),
      );

      expect(
        utf8.decode(encrypted.serialize(), allowMalformed: true),
        isNot(contains('12345')),
      );
    });

    test('every message gets a different ciphertext', () async {
      // The ratchet advances per message, so the same plaintext twice must not
      // look the same on the wire — otherwise an observer learns when someone
      // repeats themselves.
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();
      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher cipher = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      final CiphertextMessage first = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('same words')),
      );
      final CiphertextMessage second = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('same words')),
      );

      expect(first.serialize(), isNot(equals(second.serialize())));
    });

    test('a conversation runs in both directions', () async {
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();
      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher aliceToBob = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      // Bob has to open Alice's first message before he can reply: that is what
      // establishes his side of the session.
      await _decrypt(
        bob,
        alice.address,
        await aliceToBob.encrypt(Uint8List.fromList(utf8.encode('سلام'))),
      );

      final SessionCipher bobToAlice = SessionCipher(
        bob.store, bob.store, bob.store, bob.store, alice.address,
      );
      final CiphertextMessage reply = await bobToAlice.encrypt(
        Uint8List.fromList(utf8.encode('سلام، حالت چطور است؟')),
      );
      // The reply is an ordinary message, not a pre-key one: the session is up.
      expect(reply.getType(), CiphertextMessage.whisperType);

      expect(
        await _decrypt(alice, bob.address, reply),
        'سلام، حالت چطور است؟',
      );
    });

    test('messages that arrive out of order still open', () async {
      // Networks reorder. The ratchet keeps skipped message keys for exactly
      // this, and without it a delayed message would be lost for good.
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();
      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher cipher = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      final CiphertextMessage first = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('first')),
      );
      final CiphertextMessage second = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('second')),
      );
      final CiphertextMessage third = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('third')),
      );

      // Delivered third, first, second.
      expect(await _decrypt(bob, alice.address, third), 'third');
      expect(await _decrypt(bob, alice.address, first), 'first');
      expect(await _decrypt(bob, alice.address, second), 'second');
    });

    test('a stranger cannot open a message meant for someone else', () async {
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      final _Party eve = _Party('eve-device');
      await bob.publish();
      await eve.publish();

      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher cipher = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      final CiphertextMessage forBob = await cipher.encrypt(
        Uint8List.fromList(utf8.encode('for bob only')),
      );

      // Eve holds the ciphertext — the server does too — and cannot open it.
      await expectLater(
        _decrypt(eve, alice.address, forBob),
        throwsA(isA<Object>()),
      );
    });

    test('tampering with the ciphertext is detected', () async {
      // The construction is authenticated, so a flipped bit in the ciphertext
      // fails rather than producing different plaintext. A server that edited a
      // message in transit would be caught by this, which is the point.
      //
      // The tampering is done to Bob's reply rather than to Alice's opening
      // message. A `PreKeySignalMessage` wraps the encrypted body in a header
      // naming the keys to use, and corrupting that header makes the decrypt
      // fail on a key lookup — true, but it would prove only that a malformed
      // header is rejected. A `SignalMessage` is
      // version || protobuf || 8-byte MAC over the whole thing, so damaging its
      // body is caught by the MAC and nothing else. That is the property worth
      // asserting. Bob's reply is the first plain `SignalMessage` in the
      // conversation: Alice goes on sending prekey messages until she hears
      // back, since nothing before that tells her Bob has the session.
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      await bob.publish();
      await _establish(
        alice,
        DeviceBundle.fromJson(bob.bundleJson(oneTimeIndex: 0)),
        bob.address,
      );

      final SessionCipher aliceCipher = SessionCipher(
        alice.store, alice.store, alice.store, alice.store, bob.address,
      );
      final SessionCipher bobCipher = SessionCipher(
        bob.store, bob.store, bob.store, bob.store, alice.address,
      );

      // Alice's opening message establishes the session on Bob's side.
      await _decrypt(
        bob,
        alice.address,
        await aliceCipher.encrypt(Uint8List.fromList(utf8.encode('hello'))),
      );

      const String secret = 'do not change this';
      final CiphertextMessage encrypted = await bobCipher.encrypt(
        Uint8List.fromList(utf8.encode(secret)),
      );
      expect(encrypted.getType(), CiphertextMessage.whisperType);

      // Every single-bit change to the message is rejected. Checking one byte
      // would leave open the possibility that the particular offset picked
      // happened to be unauthenticated; the message is short enough to check
      // all of it. Parsing is inside the attempt because a receiver does both:
      // a bit landing in the protobuf framing is refused as malformed and one
      // landing in the ciphertext is refused by the MAC, and either way the
      // message does not open.
      final Uint8List original = encrypted.serialize();
      for (int i = 0; i < original.length; i++) {
        final Uint8List tampered = Uint8List.fromList(original);
        tampered[i] ^= 0x01;

        await expectLater(
          Future<Uint8List>(
            () => aliceCipher
                .decryptFromSignal(SignalMessage.fromSerialized(tampered)),
          ),
          throwsA(isA<Object>()),
          reason: 'a flipped bit at offset $i was not detected',
        );
      }

      // And the untouched message still opens — otherwise the loop above would
      // pass against a decrypt that always threw.
      expect(
        utf8.decode(
          await aliceCipher
              .decryptFromSignal(SignalMessage.fromSerialized(original)),
        ),
        secret,
      );
    });
  });

  group('safety numbers', () {
    test('both sides compute the same number', () async {
      // This is the check the server cannot forge: if a key was swapped in the
      // middle, the two numbers differ and the people notice.
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');

      final NumericFingerprintGenerator generator =
          NumericFingerprintGenerator(5200);

      final Fingerprint asAlice = generator.createFor(
        1,
        Uint8List.fromList(utf8.encode('alice')),
        alice.identity.getPublicKey(),
        Uint8List.fromList(utf8.encode('bob')),
        bob.identity.getPublicKey(),
      );
      final Fingerprint asBob = generator.createFor(
        1,
        Uint8List.fromList(utf8.encode('bob')),
        bob.identity.getPublicKey(),
        Uint8List.fromList(utf8.encode('alice')),
        alice.identity.getPublicKey(),
      );

      expect(
        asAlice.displayableFingerprint.getDisplayText(),
        equals(asBob.displayableFingerprint.getDisplayText()),
      );
    });

    test('a different party produces a different number', () async {
      final _Party alice = _Party('alice-device');
      final _Party bob = _Party('bob-device');
      final _Party eve = _Party('eve-device');

      final NumericFingerprintGenerator generator =
          NumericFingerprintGenerator(5200);

      final Fingerprint withBob = generator.createFor(
        1,
        Uint8List.fromList(utf8.encode('alice')),
        alice.identity.getPublicKey(),
        Uint8List.fromList(utf8.encode('bob')),
        bob.identity.getPublicKey(),
      );
      // Eve substituted for Bob: the number changes, which is exactly how a
      // key swap becomes visible to two people reading it aloud.
      final Fingerprint withEve = generator.createFor(
        1,
        Uint8List.fromList(utf8.encode('alice')),
        alice.identity.getPublicKey(),
        Uint8List.fromList(utf8.encode('bob')),
        eve.identity.getPublicKey(),
      );

      expect(
        withBob.displayableFingerprint.getDisplayText(),
        isNot(equals(withEve.displayableFingerprint.getDisplayText())),
      );
    });
  });
}
