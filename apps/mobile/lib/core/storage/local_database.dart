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
  int get schemaVersion => 1;

  @override
  MigrationStrategy get migration => MigrationStrategy(
        onCreate: (Migrator m) async {
          await m.createAll();
          await into(syncState).insert(
            SyncStateCompanion.insert(id: const Value<int>(1)),
          );
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

  /// Inserts or replaces a message. Server state always wins over the local
  /// optimistic copy, which is why this is a full upsert on the idempotency key.
  Future<void> upsertMessage(MessagesCompanion message) {
    return into(messages).insertOnConflictUpdate(message);
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
