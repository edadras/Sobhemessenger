import 'dart:async';
import 'dart:convert';

import 'package:drift/drift.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';

import '../../../core/network/api_client.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';

/// How many conversations one chat-list request asks for.
///
/// The server's own maximum. Asking for as many as it will give makes the
/// common case — a person with fewer chats than this — a complete list in one
/// request, which is what lets stale rows be pruned safely.
const int chatPageSize = 100;

/// Offline-first message store (§7, §46).
///
/// Sending is always local-first: the message is written to the database and
/// the outbox in one transaction, so it is visible immediately and survives the
/// app being killed. Delivery is then attempted over the socket, falling back
/// to REST, and retried with backoff until the server acknowledges it. The
/// `client_message_id` makes every one of those retries idempotent.
class ChatRepository {
  ChatRepository({
    required LocalDatabase database,
    required ApiClient api,
    required SocketClient socket,
  })  : _db = database,
        _api = api,
        _socket = socket;

  final LocalDatabase _db;
  final ApiClient _api;
  final SocketClient _socket;
  static const Uuid _uuid = Uuid();

  Timer? _flushTimer;
  bool _flushing = false;

  Stream<List<ChatRow>> watchChats() => _db.watchChats();

  Stream<List<MessageRow>> watchMessages(String chatId) =>
      _db.watchMessages(chatId);

  /// Resolves the one-to-one chat with someone, creating it on first contact.
  ///
  /// This needs the network: a private chat is identified by a server-assigned
  /// id, and inventing a local one would create a second conversation the
  /// moment the real id arrived.
  Future<String> openPrivateChat(String userId) async {
    final Map<String, dynamic> result = await _api.post<Map<String, dynamic>>(
      '/chats/private',
      body: <String, dynamic>{'user_id': userId},
    );
    final String chatId = result['chat_id'] as String;
    // The new chat has to reach the list, and the list is local. Without this
    // the conversation opens but never appears among the others until some
    // later refresh happens to run.
    unawaited(syncChats());
    return chatId;
  }

  /// Fetches the conversation list from the server into the local database.
  ///
  /// The list the user sees streams from local storage so it survives being
  /// offline, and this is the only thing that puts anything in it. Chats
  /// created on another device, groups the user was added to, and the names of
  /// one-to-one conversations all arrive here.
  ///
  /// Failures are the caller's to report: the list is already on screen from
  /// the cache, so a refresh that cannot reach the server is a stale list
  /// rather than a broken one.
  Future<void> syncChats() async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats',
      query: <String, dynamic>{'limit': chatPageSize},
    );
    final List<dynamic> raw =
        data['chats'] as List<dynamic>? ?? const <dynamic>[];

    final List<ChatsCompanion> rows = <ChatsCompanion>[];
    final List<String> ids = <String>[];

    for (final dynamic entry in raw) {
      final Map<String, dynamic> chat = entry as Map<String, dynamic>;
      final Map<String, dynamic> membership =
          chat['membership'] as Map<String, dynamic>? ?? const <String, dynamic>{};
      final Map<String, dynamic>? peer =
          chat['peer'] as Map<String, dynamic>?;

      final String id = chat['id'] as String;
      ids.add(id);
      rows.add(
        ChatsCompanion.insert(
          id: id,
          type: chat['type'] as String,
          title: Value<String>(chat['title'] as String? ?? ''),
          photoMediaId: Value<String?>(chat['photo_media_id'] as String?),
          lastSeq: Value<int>((chat['last_seq'] as num?)?.toInt() ?? 0),
          lastReadSeq:
              Value<int>((membership['last_read_seq'] as num?)?.toInt() ?? 0),
          unreadCount:
              Value<int>((membership['unread_count'] as num?)?.toInt() ?? 0),
          mentionCount:
              Value<int>((membership['mention_count'] as num?)?.toInt() ?? 0),
          lastMessageAt: Value<DateTime?>(
            chat['last_message_at'] == null
                ? null
                : DateTime.parse(chat['last_message_at'] as String).toLocal(),
          ),
          isPinned: Value<bool>(membership['is_pinned'] as bool? ?? false),
          isArchived: Value<bool>(membership['is_archived'] as bool? ?? false),
          mutedUntil: Value<DateTime?>(
            membership['muted_until'] == null
                ? null
                : DateTime.parse(membership['muted_until'] as String).toLocal(),
          ),
          role: Value<String>(membership['role'] as String? ?? 'member'),
          memberCount: Value<int>((chat['member_count'] as num?)?.toInt() ?? 0),
          // Present for a private or secret chat, absent for a group. It is
          // what names the row, since such a chat has no title of its own.
          peerUserId: Value<String?>(peer?['user_id'] as String?),
          peerName: Value<String?>(peer?['display_name'] as String?),
          peerAvatarMediaId:
              Value<String?>(peer?['avatar_media_id'] as String?),
        ),
      );
    }

    await _db.upsertChats(rows);

    // Pruning removes conversations left or deleted on another device, which
    // would otherwise sit in the list for ever. It is only safe when this page
    // is provably the whole list: a full page means there may be more, and
    // deleting everything the server did not mention would throw away real
    // conversations. Erring towards a stale row rather than a lost one.
    if (raw.length < chatPageSize) {
      await _db.pruneChatsNotIn(ids);
    }
  }

  /// Queues a message. Returns as soon as it is stored locally — the UI never
  /// waits on the network to show what the user just typed.
  Future<String> sendText({
    required String chatId,
    required String content,
    String? replyToId,
  }) =>
      _send(
        chatId: chatId,
        type: 'text',
        content: content,
        replyToId: replyToId,
      );

  /// Queues a message carrying already-uploaded media.
  ///
  /// The bytes are uploaded first and the message references `media_id` only,
  /// so a send that reaches the outbox is small and retrying it never
  /// re-uploads the file (§13).
  Future<String> sendMedia({
    required String chatId,
    required String type,
    required List<String> mediaIds,
    String content = '',
    String? replyToId,
  }) =>
      _send(
        chatId: chatId,
        type: type,
        content: content,
        replyToId: replyToId,
        attachments: <Map<String, dynamic>>[
          for (int i = 0; i < mediaIds.length; i++)
            <String, dynamic>{'media_id': mediaIds[i], 'position': i},
        ],
      );

  /// Queues a message carrying a typed payload — a location or a contact.
  ///
  /// The payload goes through the outbox like anything else, so sharing a
  /// location with no signal queues it rather than failing.
  Future<String> sendTyped({
    required String chatId,
    required String type,
    required Map<String, dynamic> payload,
    String? replyToId,
  }) =>
      _send(
        chatId: chatId,
        type: type,
        content: '',
        replyToId: replyToId,
        typedPayload: payload,
      );

  Future<String> _send({
    required String chatId,
    required String type,
    required String content,
    String? replyToId,
    List<Map<String, dynamic>> attachments = const <Map<String, dynamic>>[],
    Map<String, dynamic>? typedPayload,
  }) async {
    final String clientMessageId = _uuid.v4();
    final DateTime now = DateTime.now().toUtc();

    final Map<String, dynamic> payload = <String, dynamic>{
      'client_message_id': clientMessageId,
      'chat_id': chatId,
      'type': type,
      'content': content,
      if (replyToId != null) 'reply_to_id': replyToId,
      if (attachments.isNotEmpty) 'attachments': attachments,
      if (typedPayload != null) 'payload': typedPayload,
    };

    await _db.transaction(() async {
      await _db.upsertMessage(
        MessagesCompanion.insert(
          // Until the server assigns an id, the client id stands in for it.
          id: clientMessageId,
          chatId: chatId,
          clientMessageId: clientMessageId,
          type: Value<String>(type),
          content: Value<String>(content),
          replyToId: Value<String?>(replyToId),
          attachmentsJson: Value<String?>(
            attachments.isEmpty ? null : jsonEncode(attachments),
          ),
          payloadJson: Value<String?>(
            typedPayload == null ? null : jsonEncode(typedPayload),
          ),
          status: MessageStatus.pending,
          createdAt: now,
        ),
      );
      await _db.enqueue(
        OutboxCompanion.insert(
          clientMessageId: clientMessageId,
          chatId: chatId,
          operation: 'message.send',
          payloadJson: jsonEncode(payload),
          createdAt: now,
          nextAttemptAt: now,
        ),
      );
    });

    unawaited(flushOutbox());
    return clientMessageId;
  }

  /// Attempts every due outbox entry once.
  ///
  /// Safe to call concurrently: a flush already in progress simply returns, and
  /// the next tick picks up whatever is still queued.
  Future<void> flushOutbox() async {
    if (_flushing) {
      return;
    }
    _flushing = true;

    try {
      final List<OutboxRow> due = await _db.dueOutboxEntries();
      for (final OutboxRow entry in due) {
        await _attempt(entry);
      }
    } finally {
      _flushing = false;
    }
  }

  Future<void> _attempt(OutboxRow entry) async {
    await _db.markMessageStatus(entry.clientMessageId, MessageStatus.sending);
    final Map<String, dynamic> payload =
        jsonDecode(entry.payloadJson) as Map<String, dynamic>;

    try {
      // The socket is the fast path; REST is the fallback when it is down.
      if (_socket.currentStatus == SocketStatus.connected) {
        final SocketFrame ack = await _socket.request('message.send', payload);
        await _confirm(entry.clientMessageId, ack.payload);
      } else {
        final Map<String, dynamic> message =
            await _api.post<Map<String, dynamic>>(
          '/chats/${entry.chatId}/messages',
          body: payload..remove('chat_id'),
        );
        await _confirm(entry.clientMessageId, <String, dynamic>{
          'message_id': message['id'],
          'seq': message['seq'],
          'created_at': message['created_at'],
        });
      }
    } on ApiException catch (error) {
      await _handleFailure(entry, error.isRetryable, error.message);
    } on SocketException catch (error) {
      // A socket failure is always worth retrying: the message may simply have
      // outlived the connection.
      await _handleFailure(entry, true, error.message);
    }
  }

  Future<void> _confirm(
    String clientMessageId,
    Map<String, dynamic> ack,
  ) async {
    await _db.confirmMessage(
      clientMessageId: clientMessageId,
      serverId: ack['message_id'] as String,
      seq: (ack['seq'] as num).toInt(),
      createdAt: DateTime.parse(ack['created_at'] as String).toUtc(),
    );
  }

  Future<void> _handleFailure(
    OutboxRow entry,
    bool retryable,
    String reason,
  ) async {
    if (!retryable) {
      // A rejection will never succeed on retry — surface it to the user
      // instead of looping forever.
      await _db.markMessageStatus(entry.clientMessageId, MessageStatus.failed);
      await _db.removeOutboxEntry(entry.clientMessageId);
      return;
    }
    await _db.markMessageStatus(entry.clientMessageId, MessageStatus.pending);
    await _db.deferOutboxEntry(
      entry.clientMessageId,
      entry.attempts + 1,
      reason,
    );
  }

  /// Starts the periodic flush that drains the outbox once connectivity
  /// returns, and drains it immediately whenever the socket reconnects.
  void startOutboxWorker() {
    _flushTimer?.cancel();
    _flushTimer =
        Timer.periodic(const Duration(seconds: 10), (_) => flushOutbox());
    _socket.status.listen((SocketStatus status) {
      if (status == SocketStatus.connected) {
        unawaited(flushOutbox());
      }
    });
  }

  void dispose() => _flushTimer?.cancel();
}

final Provider<ChatRepository> chatRepositoryProvider =
    Provider<ChatRepository>((Ref ref) {
  final ChatRepository repository = ChatRepository(
    database: ref.watch(localDatabaseProvider),
    api: ref.watch(apiClientProvider),
    socket: ref.watch(socketClientProvider),
  );
  repository.startOutboxWorker();
  ref.onDispose(repository.dispose);
  return repository;
});

final StreamProvider<List<ChatRow>> chatListProvider =
    StreamProvider<List<ChatRow>>(
  (Ref ref) => ref.watch(chatRepositoryProvider).watchChats(),
);

final StreamProviderFamily<List<MessageRow>, String> chatMessagesProvider =
    StreamProvider.family<List<MessageRow>, String>(
  (Ref ref, String chatId) =>
      ref.watch(chatRepositoryProvider).watchMessages(chatId),
);
