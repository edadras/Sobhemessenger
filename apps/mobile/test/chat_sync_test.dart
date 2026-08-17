import 'package:drift/drift.dart';
import 'package:drift/native.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/core/storage/local_database.dart';

/// The chat list's local store (§7, §48).
///
/// The list the user sees streams from this database, so anything wrong here is
/// wrong on the first screen of the app. Two properties matter most: a refresh
/// must not lose what this device knows that the server does not, and pruning
/// must never remove a conversation that still exists.

LocalDatabase _database() =>
    LocalDatabase.forTesting(NativeDatabase.memory());

ChatsCompanion _chat(
  String id, {
  String type = 'private',
  String title = '',
  String? peerUserId,
  String? peerName,
  int unread = 0,
}) =>
    ChatsCompanion.insert(
      id: id,
      type: type,
      title: Value<String>(title),
      unreadCount: Value<int>(unread),
      peerUserId: Value<String?>(peerUserId),
      peerName: Value<String?>(peerName),
    );

void main() {
  late LocalDatabase db;

  setUp(() => db = _database());
  tearDown(() => db.close());

  group('upsertChats', () {
    test('stores the peer that names a one-to-one conversation', () async {
      // A private chat has no title of its own. Without the peer the row is
      // nameless, which is the whole chat list for most people.
      await db.upsertChats(<ChatsCompanion>[
        _chat('c1', peerUserId: 'u-bob', peerName: 'باب'),
      ]);

      final List<ChatRow> rows = await db.watchChats().first;
      expect(rows, hasLength(1));
      expect(rows.single.peerName, 'باب');
      expect(rows.single.peerUserId, 'u-bob');
    });

    test('a refresh updates a chat rather than duplicating it', () async {
      await db.upsertChats(<ChatsCompanion>[_chat('c1', unread: 3)]);
      await db.upsertChats(<ChatsCompanion>[_chat('c1', unread: 0)]);

      final List<ChatRow> rows = await db.watchChats().first;
      expect(rows, hasLength(1));
      expect(rows.single.unreadCount, 0);
    });

    test('an empty list is a no-op rather than an error', () async {
      await db.upsertChats(<ChatsCompanion>[]);
      expect(await db.watchChats().first, isEmpty);
    });
  });

  group('pruneChatsNotIn', () {
    test('removes a conversation the server no longer lists', () async {
      // Left or deleted on another device. Nothing else ever removes a row, so
      // without this it stays in the list for ever.
      await db.upsertChats(<ChatsCompanion>[_chat('c1'), _chat('c2')]);

      await db.pruneChatsNotIn(<String>['c1']);

      final List<ChatRow> rows = await db.watchChats().first;
      expect(<String>[for (final ChatRow row in rows) row.id], <String>['c1']);
    });

    test('keeps every chat the server still lists', () async {
      await db.upsertChats(<ChatsCompanion>[_chat('c1'), _chat('c2')]);

      await db.pruneChatsNotIn(<String>['c1', 'c2']);

      expect(await db.watchChats().first, hasLength(2));
    });

    test('an empty keep-list clears the cache', () async {
      // Reached only when the server returns no chats at all, which is a real
      // state for a new account.
      await db.upsertChats(<ChatsCompanion>[_chat('c1')]);

      await db.pruneChatsNotIn(<String>[]);

      expect(await db.watchChats().first, isEmpty);
    });
  });

  group('secret chats', () {
    test('are stored with the type and peer needed to open them', () async {
      // The type is what routes the tap to the encrypted screen instead of the
      // ordinary one, and the peer id is what the encryption is addressed to.
      // A secret chat missing either is unopenable.
      await db.upsertChats(<ChatsCompanion>[
        _chat('s1', type: 'secret', peerUserId: 'u-bob', peerName: 'باب'),
      ]);

      final ChatRow row = (await db.watchChats().first).single;
      expect(row.type, 'secret');
      expect(row.peerUserId, 'u-bob');
    });
  });

  group('schema', () {
    test('is at the version the peer columns were added in', () async {
      // The columns are added by an upgrade step rather than a rebuild, so the
      // version must move with them: leaving it behind means an existing
      // install never runs the step and every query mentioning the columns
      // fails.
      expect(db.schemaVersion, 3);
    });

    test('a fresh database has the peer columns', () async {
      // onCreate builds from the current schema, so this catches a table
      // definition and a migration that have drifted apart.
      await db.upsertChats(<ChatsCompanion>[
        _chat('c1', peerUserId: 'u', peerName: 'n'),
      ]);
      expect((await db.watchChats().first).single.peerUserId, 'u');
    });
  });
}
