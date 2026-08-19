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

LocalDatabase _database() => LocalDatabase.forTesting(NativeDatabase.memory());

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


/// Drafts (§7).
///
/// The one column where this device can be ahead of the server: somebody may
/// be typing right now, and the row that arrived was fetched a moment ago. The
/// rule is that the server's draft fills an empty box and never overwrites a
/// full one — which is what lets a sentence started on another device turn up
/// here, without a refresh destroying one in progress.
void _draftTests() {
  group('drafts', () {
    late LocalDatabase db;

    setUp(() async {
      db = _database();
      await db.upsertChats(<ChatsCompanion>[_chat('c1')]);
    });

    tearDown(() => db.close());

    test('a draft from elsewhere fills an empty box', () async {
      await db.adoptDraft('c1', 'از دستگاه دیگر');
      final ChatRow row = (await db.chatById('c1'))!;
      expect(row.draft, 'از دستگاه دیگر');
    });

    test('it never overwrites what is being typed here', () async {
      await db.saveDraft('c1', 'در حال تایپ');
      await db.adoptDraft('c1', 'از دستگاه دیگر');

      final ChatRow row = (await db.chatById('c1'))!;
      expect(
        row.draft,
        'در حال تایپ',
        reason: 'a refresh must not destroy a sentence in progress',
      );
    });

    test('an empty draft from the server clears nothing', () async {
      await db.saveDraft('c1', 'در حال تایپ');
      await db.adoptDraft('c1', '');

      final ChatRow row = (await db.chatById('c1'))!;
      expect(row.draft, 'در حال تایپ');
    });

    test('refreshing the chat list leaves the draft alone', () async {
      await db.saveDraft('c1', 'در حال تایپ');
      // The same chat coming back from the server, as a sync would write it.
      await db.upsertChats(<ChatsCompanion>[_chat('c1', title: 'renamed')]);

      final ChatRow row = (await db.chatById('c1'))!;
      expect(row.title, 'renamed');
      expect(
        row.draft,
        'در حال تایپ',
        reason: 'the upsert must not carry a draft of its own',
      );
    });
  });
}

void main() {
  _draftTests();
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

  group('markChatRead', () {
    test('clears the unread badge', () async {
      // Without this the badge never goes: the count is server-side state that
      // looking at the conversation does not change on its own.
      await db.upsertChats(<ChatsCompanion>[_chat('c1', unread: 7)]);

      await db.markChatRead('c1', 42);

      final ChatRow row = (await db.watchChats().first).single;
      expect(row.unreadCount, 0);
      expect(row.mentionCount, 0);
      expect(row.lastReadSeq, 42);
    });

    test('leaves other chats alone', () async {
      await db.upsertChats(<ChatsCompanion>[
        _chat('c1', unread: 3),
        _chat('c2', unread: 5),
      ]);

      await db.markChatRead('c1', 10);

      final List<ChatRow> rows = await db.watchChats().first;
      final ChatRow other = rows.firstWhere((ChatRow r) => r.id == 'c2');
      expect(other.unreadCount, 5);
    });

    test('a later refresh does not resurrect the count', () async {
      // The server is told before the local row is written, so by the time the
      // next chat-list sync runs the server already agrees it is read. This
      // pins the ordering that makes that true.
      await db.upsertChats(<ChatsCompanion>[_chat('c1', unread: 4)]);
      await db.markChatRead('c1', 12);

      // What the server would now return for that chat.
      await db.upsertChats(<ChatsCompanion>[_chat('c1', unread: 0)]);

      expect((await db.watchChats().first).single.unreadCount, 0);
    });
  });

  group('applyEdit', () {
    test('replaces the text and records when it was edited', () async {
      // The chat has to exist first: messages carry a real foreign key to it,
      // which is what makes leaving a conversation take its messages with it.
      await db.upsertChats(<ChatsCompanion>[_chat('c1')]);
      await db.upsertMessage(
        MessagesCompanion.insert(
          id: 'm1',
          chatId: 'c1',
          clientMessageId: 'cm1',
          status: MessageStatus.sent,
          content: const Value<String>('before'),
          createdAt: DateTime.utc(2026, 1, 1),
        ),
      );

      final DateTime at = DateTime.utc(2026, 1, 2);
      await db.applyEdit('m1', content: 'after', editedAt: at);

      final MessageRow row = (await db.watchMessages('c1').first).single;
      expect(row.content, 'after');
      // Compared as an instant rather than for equality: the column stores a
      // Unix timestamp, so what comes back is the same moment expressed in the
      // device's local zone, not the UTC value that went in.
      expect(row.editedAt!.isAtSameMomentAs(at), isTrue);
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
