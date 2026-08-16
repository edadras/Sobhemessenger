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

  Stream<List<MessageRow>> watchMessages(String chatId) => _db.watchMessages(chatId);

  /// Queues a message. Returns as soon as it is stored locally — the UI never
  /// waits on the network to show what the user just typed.
  Future<String> sendText({
    required String chatId,
    required String content,
    String? replyToId,
  }) async {
    final String clientMessageId = _uuid.v4();
    final DateTime now = DateTime.now().toUtc();

    final Map<String, dynamic> payload = <String, dynamic>{
      'client_message_id': clientMessageId,
      'chat_id': chatId,
      'type': 'text',
      'content': content,
      if (replyToId != null) 'reply_to_id': replyToId,
    };

    await _db.transaction(() async {
      await _db.upsertMessage(
        MessagesCompanion.insert(
          // Until the server assigns an id, the client id stands in for it.
          id: clientMessageId,
          chatId: chatId,
          clientMessageId: clientMessageId,
          type: const Value<String>('text'),
          content: Value<String>(content),
          replyToId: Value<String?>(replyToId),
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
        final Map<String, dynamic> message = await _api.post<Map<String, dynamic>>(
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

  Future<void> _confirm(String clientMessageId, Map<String, dynamic> ack) async {
    await _db.confirmMessage(
      clientMessageId: clientMessageId,
      serverId: ack['message_id'] as String,
      seq: (ack['seq'] as num).toInt(),
      createdAt: DateTime.parse(ack['created_at'] as String).toUtc(),
    );
  }

  Future<void> _handleFailure(OutboxRow entry, bool retryable, String reason) async {
    if (!retryable) {
      // A rejection will never succeed on retry — surface it to the user
      // instead of looping forever.
      await _db.markMessageStatus(entry.clientMessageId, MessageStatus.failed);
      await _db.removeOutboxEntry(entry.clientMessageId);
      return;
    }
    await _db.markMessageStatus(entry.clientMessageId, MessageStatus.pending);
    await _db.deferOutboxEntry(entry.clientMessageId, entry.attempts + 1, reason);
  }

  /// Starts the periodic flush that drains the outbox once connectivity
  /// returns, and drains it immediately whenever the socket reconnects.
  void startOutboxWorker() {
    _flushTimer?.cancel();
    _flushTimer = Timer.periodic(const Duration(seconds: 10), (_) => flushOutbox());
    _socket.status.listen((SocketStatus status) {
      if (status == SocketStatus.connected) {
        unawaited(flushOutbox());
      }
    });
  }

  void dispose() => _flushTimer?.cancel();
}

final Provider<ChatRepository> chatRepositoryProvider = Provider<ChatRepository>((Ref ref) {
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
    StreamProvider<List<ChatRow>>((Ref ref) => ref.watch(chatRepositoryProvider).watchChats());

final StreamProviderFamily<List<MessageRow>, String> chatMessagesProvider =
    StreamProvider.family<List<MessageRow>, String>(
  (Ref ref, String chatId) => ref.watch(chatRepositoryProvider).watchMessages(chatId),
);
