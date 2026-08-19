import 'dart:convert';
import 'dart:io';

import 'package:drift/drift.dart';
import 'package:drift/native.dart';
import 'package:path/path.dart' as p;
import 'package:path_provider/path_provider.dart';

part 'local_database.g.dart';

/// Delivery state of a message on this device (§7).
///
/// `pending` and `sending` only ever exist locally: they describe a message the
/// server has not acknowledged yet. Everything from `sent` onwards mirrors
/// server state.
enum MessageStatus { pending, sending, sent, delivered, read, failed }

@DataClassName('ChatRow')
class Chats extends Table {
  TextColumn get id => text()();
  TextColumn get type => text()();
  TextColumn get title => text().withDefault(const Constant(''))();
  TextColumn get photoMediaId => text().nullable()();

  /// Highest sequence the server has for this chat.
  IntColumn get lastSeq => integer().withDefault(const Constant(0))();

  /// Highest sequence this device has stored, which may lag [lastSeq] while a
  /// backfill is in progress.
  IntColumn get syncedSeq => integer().withDefault(const Constant(0))();
  IntColumn get lastReadSeq => integer().withDefault(const Constant(0))();
  IntColumn get unreadCount => integer().withDefault(const Constant(0))();
  IntColumn get mentionCount => integer().withDefault(const Constant(0))();
  DateTimeColumn get lastMessageAt => dateTime().nullable()();
  TextColumn get draft => text().withDefault(const Constant(''))();
  BoolColumn get isPinned => boolean().withDefault(const Constant(false))();
  BoolColumn get isArchived => boolean().withDefault(const Constant(false))();
  DateTimeColumn get mutedUntil => dateTime().nullable()();
  TextColumn get role => text().withDefault(const Constant('member'))();
  IntColumn get memberCount => integer().withDefault(const Constant(0))();

  /// The other person in a one-to-one conversation, resolved by the server.
  ///
  /// A private or secret chat has no title of its own, so [title] is empty for
  /// them and the name shown comes from here. The id is also what opening an
  /// encrypted chat needs, since a secret chat is addressed to a person.
  TextColumn get peerUserId => text().nullable()();
  TextColumn get peerName => text().nullable()();
  TextColumn get peerAvatarMediaId => text().nullable()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{id};
}

@DataClassName('MessageRow')
class Messages extends Table {
  /// Server id once acknowledged; before that, the client message id.
  TextColumn get id => text()();
  TextColumn get chatId =>
      text().references(Chats, #id, onDelete: KeyAction.cascade)();

  /// Server sequence. Null while the message is still in the outbox — it has no
  /// position in the chat until the server assigns one.
  IntColumn get seq => integer().nullable()();

  TextColumn get clientMessageId => text()();
  TextColumn get senderId => text().nullable()();
  TextColumn get type => text().withDefault(const Constant('text'))();
  TextColumn get content => text().withDefault(const Constant(''))();
  TextColumn get entitiesJson => text().nullable()();
  TextColumn get payloadJson => text().nullable()();
  TextColumn get replyToId => text().nullable()();
  TextColumn get attachmentsJson => text().nullable()();
  TextColumn get reactionsJson => text().nullable()();

  /// The inline keyboard a bot attached, as the server sent it. Kept whole
  /// rather than parsed into columns: it is a bot's structure, and the app's
  /// job is to draw it, not to have an opinion about it.
  TextColumn get replyMarkupJson => text().nullable()();

  IntColumn get status => intEnum<MessageStatus>()();
  BoolColumn get isPinned => boolean().withDefault(const Constant(false))();
  DateTimeColumn get createdAt => dateTime()();
  DateTimeColumn get editedAt => dateTime().nullable()();
  DateTimeColumn get deletedAt => dateTime().nullable()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{id};

  @override
  List<Set<Column<Object>>> get uniqueKeys => <Set<Column<Object>>>[
        // The idempotency key, mirroring the server's constraint: a retry can
        // never produce a second local row either.
        <Column<Object>>{chatId, clientMessageId},
      ];
}

/// Messages waiting to reach the server (§7).
///
/// The outbox is separate from [Messages] so a queued send survives the message
/// row being re-fetched, and so retry bookkeeping never touches the data the UI
/// renders.
@DataClassName('OutboxRow')
class Outbox extends Table {
  TextColumn get clientMessageId => text()();
  TextColumn get chatId => text()();
  TextColumn get operation => text()();
  TextColumn get payloadJson => text()();
  IntColumn get attempts => integer().withDefault(const Constant(0))();
  TextColumn get lastError => text().nullable()();
  DateTimeColumn get createdAt => dateTime()();
  DateTimeColumn get nextAttemptAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{clientMessageId};
}

@DataClassName('UserRow')
class Users extends Table {
  TextColumn get id => text()();
  TextColumn get username => text().nullable()();
  TextColumn get displayName => text().withDefault(const Constant(''))();
  TextColumn get avatarMediaId => text().nullable()();
  BoolColumn get isOnline => boolean().withDefault(const Constant(false))();
  DateTimeColumn get lastSeenAt => dateTime().nullable()();
  DateTimeColumn get updatedAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{id};
}

/// Cached media metadata. The bytes live in the file cache; this table only
/// records what is where (§47).
@DataClassName('MediaRow')
class MediaCache extends Table {
  TextColumn get mediaId => text()();
  TextColumn get localPath => text().nullable()();
  TextColumn get mimeType => text().withDefault(const Constant(''))();
  IntColumn get sizeBytes => integer().withDefault(const Constant(0))();
  IntColumn get width => integer().nullable()();
  IntColumn get height => integer().nullable()();
  IntColumn get durationMs => integer().nullable()();
  TextColumn get blurhash => text().nullable()();
  DateTimeColumn get cachedAt => dateTime()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{mediaId};
}

/// Single-row table holding this device's sync cursor (§9).
@DataClassName('SyncStateRow')
class SyncState extends Table {
  IntColumn get id => integer().withDefault(const Constant(1))();
  IntColumn get cursor => integer().withDefault(const Constant(0))();
  DateTimeColumn get lastSyncedAt => dateTime().nullable()();

  @override
  Set<Column<Object>> get primaryKey => <Column<Object>>{id};
}

@DriftDatabase(
  tables: <Type>[Chats, Messages, Outbox, Users, MediaCache, SyncState],
)
class LocalDatabase extends _$LocalDatabase {
  LocalDatabase() : super(_open());

  /// Used by tests, which pass an in-memory executor.
  LocalDatabase.forTesting(super.executor);

  @override
  int get schemaVersion => 3;

  @override
  MigrationStrategy get migration => MigrationStrategy(
        onCreate: (Migrator m) async {
          await m.createAll();
          await into(syncState).insert(
            SyncStateCompanion.insert(id: const Value<int>(1)),
          );
        },
        onUpgrade: (Migrator m, int from, int to) async {
          // 2: inline keyboards. Existing rows simply have none, so the column
          // is added rather than the cache being thrown away — a rebuild would
          // cost every user their offline history for a feature they may never
          // meet.
          if (from < 2) {
            await m.addColumn(messages, messages.replyMarkupJson);
          }
          // 3: the resolved peer of a one-to-one chat. Existing rows get null
          // and are filled in by the next chat-list refresh, which is a blank
          // name for a moment rather than a discarded cache.
          if (from < 3) {
            await m.addColumn(chats, chats.peerUserId);
            await m.addColumn(chats, chats.peerName);
            await m.addColumn(chats, chats.peerAvatarMediaId);
          }
        },
        beforeOpen: (OpeningDetails details) async {
          // Cascades depend on foreign keys, which SQLite leaves off by default.
          await customStatement('PRAGMA foreign_keys = ON');
        },
      );

  /// The chat list, ordered exactly as the UI renders it.
  Stream<List<ChatRow>> watchChats() {
    return (select(chats)
          ..orderBy(<OrderClauseGenerator<$ChatsTable>>[
            (t) =>
                OrderingTerm(expression: t.isPinned, mode: OrderingMode.desc),
            (t) => OrderingTerm(
                  expression: t.lastMessageAt,
                  mode: OrderingMode.desc,
                ),
          ]))
        .watch();
  }

  /// A chat's messages, newest last. Outbox messages have no sequence yet, so
  /// they sort by creation time and appear at the bottom where the user expects.
  Stream<List<MessageRow>> watchMessages(String chatId, {int limit = 100}) {
    return (select(messages)
          ..where(($MessagesTable t) => t.chatId.equals(chatId))
          ..orderBy(<OrderClauseGenerator<$MessagesTable>>[
            (t) =>
                OrderingTerm(expression: t.createdAt, mode: OrderingMode.desc),
          ])
          ..limit(limit))
        .watch()
        .map((List<MessageRow> rows) => rows.reversed.toList());
  }

  /// Stores the chat list the server returned.
  ///
  /// Written in one transaction so the list never renders half-updated, and as
  /// an upsert that leaves [Chats.syncedSeq] and [Chats.draft] alone: those are
  /// this device's own state, and the server's view of the chat knows nothing
  /// about how far this device has backfilled or what is half-typed in it.
  Future<void> upsertChats(List<ChatsCompanion> rows) async {
    if (rows.isEmpty) {
      return;
    }
    await batch((Batch batch) {
      for (final ChatsCompanion row in rows) {
        batch.insert(
          chats,
          // The draft is decided separately, by adoptDraft — an upsert that
          // wrote it wholesale would overwrite a sentence being typed with a
          // row fetched a moment earlier.
          row.copyWith(draft: const Value<String>.absent()),
          onConflict: DoUpdate(
            (_) => row.copyWith(draft: const Value<String>.absent()),
            target: <Column<Object>>[chats.id],
          ),
        );
      }
    });
  }

  /// Takes the server's draft for a chat, but only into an empty box.
  ///
  /// A draft is the one piece of chat state where this device can be ahead of
  /// the server: somebody may be typing right now, and the row that arrived
  /// was fetched a moment ago. Filling only an empty box is what lets a draft
  /// started on another device turn up here, without a refresh ever destroying
  /// a sentence in progress.
  Future<void> adoptDraft(String chatId, String draft) {
    if (draft.isEmpty) {
      return Future<void>.value();
    }
    return (update(chats)
          ..where(
            ($ChatsTable t) => t.id.equals(chatId) & t.draft.equals(''),
          ))
        .write(ChatsCompanion(draft: Value<String>(draft)));
  }

  /// Removes local chats the server no longer lists.
  ///
  /// A conversation the user left or deleted on another device would otherwise
  /// sit in the list for ever, since nothing else ever removes a row. Only
  /// called with a complete first page, never with a paged fragment — pruning
  /// against a partial list would delete everything below the fold.
  Future<int> pruneChatsNotIn(List<String> keep) {
    if (keep.isEmpty) {
      return delete(chats).go();
    }
    return (delete(chats)..where(($ChatsTable t) => t.id.isNotIn(keep))).go();
  }

  /// Inserts or replaces a message. Server state always wins over the local
  /// optimistic copy, which is why this is a full upsert on the idempotency key.
  Future<void> upsertMessage(MessagesCompanion message) {
    return into(messages).insertOnConflictUpdate(message);
  }

  /// Marks a message deleted, keeping the row.
  ///
  /// A tombstone rather than a delete: the message stays in its place in the
  /// conversation showing that something was removed, which is what the server
  /// does too — a gap in the sequence would be worse than a marker.
  Future<void> markMessageDeleted(String messageId) {
    return (update(messages)
          ..where(($MessagesTable t) => t.id.equals(messageId)))
        .write(MessagesCompanion(deletedAt: Value<DateTime>(DateTime.now())));
  }

  /// Applies an edit the server has accepted.
  Future<void> applyEdit(
    String messageId, {
    required String content,
    required DateTime editedAt,
  }) {
    return (update(messages)
          ..where(($MessagesTable t) => t.id.equals(messageId)))
        .write(
      MessagesCompanion(
        content: Value<String>(content),
        editedAt: Value<DateTime>(editedAt),
      ),
    );
  }

  /// One chat, or null if this device has not seen it.
  Future<ChatRow?> chatById(String chatId) =>
      (select(chats)..where(($ChatsTable t) => t.id.equals(chatId)))
          .getSingleOrNull();

  /// Clears the unread badge locally, without waiting for the next sync.
  ///
  /// The counter is set to zero rather than decremented: the read cursor moving
  /// to [seq] means everything up to it has been read, and arithmetic on a
  /// count that the server also owns would drift.
  Future<void> markChatRead(String chatId, int seq) {
    return (update(chats)..where(($ChatsTable t) => t.id.equals(chatId))).write(
      ChatsCompanion(
        lastReadSeq: Value<int>(seq),
        unreadCount: const Value<int>(0),
        mentionCount: const Value<int>(0),
      ),
    );
  }

  Future<void> markMessagePinned(String messageId, bool pinned) {
    return (update(messages)
          ..where(($MessagesTable t) => t.id.equals(messageId)))
        .write(MessagesCompanion(isPinned: Value<bool>(pinned)));
  }

  /// Where this device has got to in the per-user event log.
  Future<int> syncCursor() async {
    final SyncStateRow? state = await (select(syncState)
          ..where(($SyncStateTable t) => t.id.equals(1)))
        .getSingleOrNull();
    return state?.cursor ?? 0;
  }

  /// Moves the cursor forward, never back.
  ///
  /// An out-of-order frame carrying an older sequence must not rewind it, or
  /// the next catch-up would replay everything in between.
  Future<void> advanceSyncCursor(int cursor) async {
    await (update(syncState)
          ..where(
            ($SyncStateTable t) =>
                t.id.equals(1) & t.cursor.isSmallerThanValue(cursor),
          ))
        .write(
      SyncStateCompanion(
        cursor: Value<int>(cursor),
        lastSyncedAt: Value<DateTime>(DateTime.now()),
      ),
    );
  }

  Future<void> markMessageStatus(String clientMessageId, MessageStatus status) {
    return (update(messages)
          ..where(
            ($MessagesTable t) => t.clientMessageId.equals(clientMessageId),
          ))
        .write(MessagesCompanion(status: Value<MessageStatus>(status)));
  }

  /// Promotes an acknowledged message: the temporary local id becomes the
  /// server id and the assigned sequence lands.
  Future<void> confirmMessage({
    required String clientMessageId,
    required String serverId,
    required int seq,
    required DateTime createdAt,
    String? payloadJson,
  }) async {
    await transaction(() async {
      await (update(messages)
            ..where(
              ($MessagesTable t) => t.clientMessageId.equals(clientMessageId),
            ))
          .write(
        MessagesCompanion(
          id: Value<String>(serverId),
          seq: Value<int>(seq),
          createdAt: Value<DateTime>(createdAt),
          status: const Value<MessageStatus>(MessageStatus.sent),
          // The server completes some payloads — a live location comes back
          // with the deadline it computed, which the device could not know.
          // Keeping the sent version would leave a share that renders as
          // already over. Absent when the server sent nothing, so the column
          // is left alone rather than nulled.
          payloadJson: payloadJson == null
              ? const Value<String>.absent()
              : Value<String>(payloadJson),
        ),
      );
      await (delete(outbox)
            ..where(
              ($OutboxTable t) => t.clientMessageId.equals(clientMessageId),
            ))
          .go();
    });
  }

  /// Queued operations that are due for another attempt.
  Future<List<OutboxRow>> dueOutboxEntries({int limit = 20}) {
    return (select(outbox)
          ..where(
            ($OutboxTable t) =>
                t.nextAttemptAt.isSmallerOrEqualValue(DateTime.now()),
          )
          ..orderBy(<OrderClauseGenerator<$OutboxTable>>[
            (t) => OrderingTerm(expression: t.createdAt),
          ])
          ..limit(limit))
        .get();
  }

  Future<void> enqueue(OutboxCompanion entry) =>
      into(outbox).insertOnConflictUpdate(entry);

  /// Records a failed attempt and schedules the next one with backoff.
  Future<void> deferOutboxEntry(
    String clientMessageId,
    int attempts,
    String error,
  ) {
    final Duration delay = Duration(seconds: 1 << attempts.clamp(0, 8));
    return (update(outbox)
          ..where(
            ($OutboxTable t) => t.clientMessageId.equals(clientMessageId),
          ))
        .write(
      OutboxCompanion(
        attempts: Value<int>(attempts),
        lastError: Value<String>(error),
        nextAttemptAt: Value<DateTime>(DateTime.now().add(delay)),
      ),
    );
  }

  Future<void> removeOutboxEntry(String clientMessageId) => (delete(outbox)
        ..where(($OutboxTable t) => t.clientMessageId.equals(clientMessageId)))
      .go();

  Future<int> readSyncCursor() async {
    final SyncStateRow? row = await (select(syncState)
          ..where(($SyncStateTable t) => t.id.equals(1)))
        .getSingleOrNull();
    return row?.cursor ?? 0;
  }

  Future<void> writeSyncCursor(int cursor) {
    return into(syncState).insertOnConflictUpdate(
      SyncStateCompanion.insert(
        id: const Value<int>(1),
        cursor: Value<int>(cursor),
        lastSyncedAt: Value<DateTime>(DateTime.now()),
      ),
    );
  }

  /// Stores what is half-typed in a chat.
  ///
  /// This device's own state, which is why [upsertChats] leaves the column
  /// alone: the server's view of a chat knows nothing about what is sitting
  /// unsent in its compose box on this phone.
  Future<void> saveDraft(String chatId, String draft) {
    return (update(chats)..where(($ChatsTable t) => t.id.equals(chatId)))
        .write(ChatsCompanion(draft: Value<String>(draft)));
  }

  /// Every location message this device holds that is still being shared.
  ///
  /// Confirmed messages only: a share still in the outbox has no server id to
  /// move yet, and it will be picked up on the next pass once it has one.
  Future<List<MessageRow>> liveLocationMessages() {
    return (select(messages)
          ..where(
            ($MessagesTable t) =>
                t.type.equals('location') &
                t.deletedAt.isNull() &
                t.payloadJson.isNotNull() &
                t.status.equalsValue(MessageStatus.sent),
          ))
        .get();
  }

  /// Marks a share as finished in the local copy.
  ///
  /// The row keeps its last point — the conversation should still show where
  /// the person was — but stops claiming to be live, which is what the server
  /// does when sharing ends.
  Future<void> endLiveLocation(String messageId) async {
    final MessageRow? row = await (select(messages)
          ..where(($MessagesTable t) => t.id.equals(messageId))
          ..limit(1))
        .getSingleOrNull();
    final String? raw = row?.payloadJson;
    if (raw == null) {
      return;
    }

    final Object? decoded = jsonDecode(raw);
    if (decoded is! Map<String, dynamic>) {
      return;
    }
    decoded['live_until'] = DateTime.now().toUtc().toIso8601String();

    await (update(messages)..where(($MessagesTable t) => t.id.equals(messageId)))
        .write(
      MessagesCompanion(payloadJson: Value<String>(jsonEncode(decoded))),
    );
  }

  /// Wipes every local trace of the account on sign-out.
  Future<void> wipe() async {
    await transaction(() async {
      await delete(outbox).go();
      await delete(messages).go();
      await delete(chats).go();
      await delete(users).go();
      await delete(mediaCache).go();
      await delete(syncState).go();
      await into(syncState)
          .insert(SyncStateCompanion.insert(id: const Value<int>(1)));
    });
  }
}

LazyDatabase _open() {
  return LazyDatabase(() async {
    final Directory directory = await getApplicationDocumentsDirectory();
    final File file = File(p.join(directory.path, 'sobh.sqlite'));
    return NativeDatabase.createInBackground(file);
  });
}
