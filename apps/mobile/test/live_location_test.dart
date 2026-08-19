import 'dart:convert';
import 'dart:io';

import 'package:drift/drift.dart';
import 'package:drift/native.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/core/storage/local_database.dart';
import 'package:sobh_app/features/groups/data/chat_permissions.dart';

/// Live location on the device (§12).
///
/// A live share is the one message that keeps changing after it is sent, and
/// the device is what keeps it changing. What matters here is that the local
/// store can tell a share that is still running from one that has finished —
/// the controller does nothing else to decide what to send.

LocalDatabase _database() => LocalDatabase.forTesting(NativeDatabase.memory());

MessagesCompanion _location(
  String id, {
  required DateTime liveUntil,
  String chatId = 'chat-1',
  MessageStatus status = MessageStatus.sent,
}) =>
    MessagesCompanion.insert(
      id: id,
      chatId: chatId,
      clientMessageId: 'client-$id',
      type: const Value<String>('location'),
      status: status,
      createdAt: DateTime.now().toUtc(),
      payloadJson: Value<String>(
        jsonEncode(<String, dynamic>{
          'latitude': 35.7,
          'longitude': 51.4,
          'live_period_seconds': 3600,
          'live_until': liveUntil.toUtc().toIso8601String(),
        }),
      ),
    );

void main() {
  group('live location', () {
    late LocalDatabase db;

    setUp(() async {
      db = _database();
      await db.upsertChats(<ChatsCompanion>[
        ChatsCompanion.insert(id: 'chat-1', type: 'private'),
      ]);
    });

    tearDown(() => db.close());

    test('a finished share stops being reported as live', () async {
      final DateTime future = DateTime.now().add(const Duration(hours: 1));
      await db.upsertMessage(_location('m1', liveUntil: future));

      List<MessageRow> rows = await db.liveLocationMessages();
      expect(rows, hasLength(1));

      await db.endLiveLocation('m1');

      rows = await db.liveLocationMessages();
      final Map<String, dynamic> payload =
          jsonDecode(rows.single.payloadJson!) as Map<String, dynamic>;
      final DateTime until =
          DateTime.parse(payload['live_until'] as String).toLocal();
      expect(
        until.isAfter(DateTime.now()),
        isFalse,
        reason: 'ending a share must move its deadline into the past',
      );
    });

    test('ending a share keeps the last point', () async {
      await db.upsertMessage(
        _location('m1', liveUntil: DateTime.now().add(const Duration(hours: 1))),
      );
      await db.endLiveLocation('m1');

      final MessageRow row = (await db.liveLocationMessages()).single;
      final Map<String, dynamic> payload =
          jsonDecode(row.payloadJson!) as Map<String, dynamic>;
      // The conversation should still show where the person was. Dropping the
      // coordinates would turn a finished share into an empty bubble.
      expect(payload['latitude'], 35.7);
      expect(payload['longitude'], 51.4);
    });

    test('a message the server has not acknowledged is not reported',
        () async {
      await db.upsertMessage(
        _location(
          'm2',
          liveUntil: DateTime.now().add(const Duration(hours: 1)),
          status: MessageStatus.pending,
        ),
      );
      // There is no server id to move yet, so there is nothing to update.
      expect(await db.liveLocationMessages(), isEmpty);
    });

    test('confirming a send takes the payload the server completed', () async {
      await db.upsertMessage(
        MessagesCompanion.insert(
          id: 'client-m3',
          chatId: 'chat-1',
          clientMessageId: 'client-m3',
          type: const Value<String>('location'),
          status: MessageStatus.pending,
          createdAt: DateTime.now().toUtc(),
          payloadJson: Value<String>(
            jsonEncode(<String, dynamic>{
              'latitude': 35.7,
              'longitude': 51.4,
              'live_period_seconds': 3600,
            }),
          ),
        ),
      );

      final String until = DateTime.now()
          .toUtc()
          .add(const Duration(hours: 1))
          .toIso8601String();
      await db.confirmMessage(
        clientMessageId: 'client-m3',
        serverId: 'server-m3',
        seq: 4,
        createdAt: DateTime.now().toUtc(),
        payloadJson: jsonEncode(<String, dynamic>{
          'latitude': 35.7,
          'longitude': 51.4,
          'live_period_seconds': 3600,
          'live_until': until,
        }),
      );

      // Without the server's deadline the share would render as already over,
      // because the device has no way to compute it.
      final MessageRow row = (await db.liveLocationMessages()).single;
      final Map<String, dynamic> payload =
          jsonDecode(row.payloadJson!) as Map<String, dynamic>;
      expect(payload['live_until'], until);
    });

    test('confirming a plain send leaves the payload alone', () async {
      await db.upsertMessage(
        MessagesCompanion.insert(
          id: 'client-m4',
          chatId: 'chat-1',
          clientMessageId: 'client-m4',
          type: const Value<String>('location'),
          status: MessageStatus.pending,
          createdAt: DateTime.now().toUtc(),
          payloadJson: const Value<String>('{"latitude":1,"longitude":2}'),
        ),
      );
      await db.confirmMessage(
        clientMessageId: 'client-m4',
        serverId: 'server-m4',
        seq: 5,
        createdAt: DateTime.now().toUtc(),
      );

      final MessageRow row = (await db.liveLocationMessages()).single;
      expect(row.payloadJson, '{"latitude":1,"longitude":2}');
    });
  });

  group('permission vocabulary', () {
    /// The app, the Go server and the schema each hold a copy of the
    /// permission keys. The server already asserts its copy against the
    /// schema; this asserts the app's against the server, so a key added in
    /// one place cannot silently go missing from the editor.
    test('matches the server, key for key', () {
      final File source =
          File('../../backend/internal/messaging/models.go');
      if (!source.existsSync()) {
        // The app can be built from its own directory alone. When the whole
        // repository is present — which is the case in CI — the comparison
        // runs.
        return;
      }

      final String body = source.readAsStringSync();
      final int start = body.indexOf('var PermissionKeys = map[string]string{');
      expect(
        start,
        greaterThan(-1),
        reason: 'the server no longer declares PermissionKeys as expected',
      );
      final int end = body.indexOf('\n}', start);
      final String block = body.substring(start, end);

      final Map<String, String> server = <String, String>{
        for (final RegExpMatch match
            in RegExp(r'"([a-z_]+)":\s*"(both|group|channel)"').allMatches(block))
          match.group(1)!: match.group(2)!,
      };
      expect(server, isNotEmpty);

      final Map<String, String> app = <String, String>{
        for (final ChatPermission permission in ChatPermission.values)
          permission.key: permission.appliesTo('group')
              ? (permission.appliesTo('channel') ? 'both' : 'group')
              : 'channel',
      };

      expect(app, server);
    });
  });
}
