import 'dart:async';
import 'dart:convert';

import 'package:drift/drift.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';

/// Receiving messages (§21, §49).
///
/// The outbox in [ChatRepository] is the sending half. This is the other one:
/// history that was written before this device existed, messages that arrive
/// while it is connected, and the reconciliation that catches up whatever it
/// missed while it was not.
///
/// Everything lands in the local database, and the UI only ever watches that.
/// The screen therefore does not care whether a message came from the socket,
/// from a backfill or from this device's own outbox — which is what makes the
/// app work offline rather than merely survive it.

/// The realtime events that change what a chat looks like.
const String eventMessageNew = 'message.new';
const String eventMessageEdited = 'message.edited';
const String eventMessageDeleted = 'message.deleted';
const String eventMessagePinned = 'message.pinned';
const String eventLocationUpdated = 'message.location_updated';

class InboundSync {
  InboundSync({
    required LocalDatabase database,
    required ApiClient api,
    required SocketClient socket,
  })  : _db = database,
        _api = api,
        _socket = socket;

  final LocalDatabase _db;
  final ApiClient _api;
  final SocketClient _socket;

  StreamSubscription<SocketFrame>? _frames;
  StreamSubscription<SocketStatus>? _statuses;

  /// Starts listening. Safe to call twice; the second call replaces the first.
  void start() {
    _frames?.cancel();
    _statuses?.cancel();

    _frames = _socket.events.listen(_onFrame);
    _statuses = _socket.status.listen((SocketStatus status) {
      // A reconnection is exactly when this device is most likely to be
      // behind, so the catch-up runs then rather than on a timer.
      if (status == SocketStatus.connected) {
        unawaited(catchUp());
      }
    });
  }

  Future<void> dispose() async {
    await _frames?.cancel();
    await _statuses?.cancel();
  }

  // ------------------------------------------------------------- realtime

  void _onFrame(SocketFrame frame) {
    switch (frame.event) {
      case eventMessageNew:
      case eventLocationUpdated:
        final Map<String, dynamic>? message =
            frame.payload['message'] as Map<String, dynamic>?;
        if (message != null) {
          unawaited(storeMessage(message));
        }

      case eventMessageEdited:
        final Map<String, dynamic>? message =
            frame.payload['message'] as Map<String, dynamic>?;
        if (message != null) {
          unawaited(storeMessage(message));
        }

      case eventMessageDeleted:
        final String? id = frame.payload['message_id'] as String?;
        if (id != null) {
          unawaited(_db.markMessageDeleted(id));
        }

      case eventMessagePinned:
        final String? id = frame.payload['message_id'] as String?;
        final bool pinned = frame.payload['pinned'] as bool? ?? false;
        if (id != null) {
          unawaited(_db.markMessagePinned(id, pinned));
        }
    }

    // The cursor moves only for events that carried one. A frame broadcast on
    // the chat subject has no per-user sequence, and treating its absence as
    // zero would rewind the cursor and replay everything.
    if (frame.syncSeq != null) {
      unawaited(_advanceCursor(frame.syncSeq!));
    }
  }

  // -------------------------------------------------------------- history

  /// Loads a page of history, oldest-first into the local database.
  ///
  /// Called when a chat is opened and when the user scrolls back. It is safe to
  /// re-run over messages already stored: the upsert is keyed on the message
  /// id, so a re-fetch corrects a stale copy rather than duplicating it.
  Future<int> loadHistory(
    String chatId, {
    int? beforeSeq,
    int limit = 50,
  }) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats/$chatId/messages',
      query: <String, dynamic>{
        'limit': limit,
        if (beforeSeq != null) 'before_seq': beforeSeq,
      },
    );

    final List<dynamic> messages =
        data['messages'] as List<dynamic>? ?? const <dynamic>[];
    for (final dynamic entry in messages) {
      await storeMessage(entry as Map<String, dynamic>);
    }
    return messages.length;
  }

  /// Reconciles everything this device missed.
  ///
  /// The cursor is per user and durable on the server, so this returns exactly
  /// the events between where the device got to and now — however long it was
  /// away, and whether it missed them by being offline or by losing frames on
  /// a node that died.
  Future<void> catchUp() async {
    int cursor = await _db.syncCursor();

    // Bounded rather than "until drained": a device that has been off for a
    // month should come back usable, not spend a minute replaying before it
    // draws anything.
    for (int page = 0; page < 20; page++) {
      final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
        '/sync',
        query: <String, dynamic>{'cursor': cursor, 'limit': 200},
      );

      final List<dynamic> events =
          data['events'] as List<dynamic>? ?? const <dynamic>[];
      for (final dynamic raw in events) {
        final Map<String, dynamic> event = raw as Map<String, dynamic>;
        final Map<String, dynamic> payload =
            event['payload'] as Map<String, dynamic>? ??
                const <String, dynamic>{};

        switch (event['type'] as String?) {
          case eventMessageNew:
          case eventMessageEdited:
            final Map<String, dynamic>? message =
                payload['message'] as Map<String, dynamic>?;
            if (message != null) {
              await storeMessage(message);
            }
          case eventMessageDeleted:
            final String? id = payload['message_id'] as String?;
            if (id != null) {
              await _db.markMessageDeleted(id);
            }
        }
      }

      final int latest = (data['latest'] as num?)?.toInt() ?? cursor;
      if (latest > cursor) {
        cursor = latest;
        await _advanceCursor(cursor);
      }
      if (events.length < 200) {
        break; // caught up
      }
    }

    _socket.acknowledgeSync(cursor);
  }

  Future<void> _advanceCursor(int cursor) => _db.advanceSyncCursor(cursor);

  // --------------------------------------------------------------- storage

  /// Writes one server message into the local database.
  ///
  /// Server state wins over the optimistic local copy, which is why this is a
  /// full upsert: a message this device sent and then re-received arrives with
  /// its real id and sequence and replaces the placeholder.
  Future<void> storeMessage(Map<String, dynamic> message) async {
    final String? id = message['id'] as String?;
    final String? chatId = message['chat_id'] as String?;
    if (id == null || chatId == null) {
      return;
    }

    final dynamic attachments = message['attachments'];
    final dynamic reactions = message['reactions'];
    final dynamic payload = message['payload'];
    final dynamic replyMarkup = message['reply_markup'];

    await _db.upsertMessage(
      MessagesCompanion.insert(
        id: id,
        chatId: chatId,
        seq: Value<int?>((message['seq'] as num?)?.toInt()),
        clientMessageId: message['client_message_id'] as String? ?? id,
        senderId: Value<String?>(message['sender_id'] as String?),
        type: Value<String>(message['type'] as String? ?? 'text'),
        content: Value<String>(message['content'] as String? ?? ''),
        entitiesJson: Value<String?>(_encode(message['entities'])),
        payloadJson: Value<String?>(_encode(payload)),
        replyToId: Value<String?>(message['reply_to_id'] as String?),
        attachmentsJson: Value<String?>(_encode(attachments)),
        reactionsJson: Value<String?>(_encode(reactions)),
        replyMarkupJson: Value<String?>(_encode(replyMarkup)),
        // Anything the server has handed back is, by definition, sent.
        status: MessageStatus.sent,
        isPinned: Value<bool>(message['is_pinned'] as bool? ?? false),
        createdAt: DateTime.parse(message['created_at'] as String).toLocal(),
        editedAt: Value<DateTime?>(
          message['edited_at'] == null
              ? null
              : DateTime.parse(message['edited_at'] as String).toLocal(),
        ),
        deletedAt: Value<DateTime?>(
          message['deleted_at'] == null
              ? null
              : DateTime.parse(message['deleted_at'] as String).toLocal(),
        ),
      ),
    );
  }

  /// Encodes a structure for storage, treating an absent or empty one as null
  /// so the column means "there is none" rather than "there is an empty one".
  static String? _encode(dynamic value) {
    if (value == null) {
      return null;
    }
    if (value is List && value.isEmpty) {
      return null;
    }
    if (value is Map && value.isEmpty) {
      return null;
    }
    return jsonEncode(value);
  }
}

final Provider<InboundSync> inboundSyncProvider =
    Provider<InboundSync>((Ref ref) {
  final InboundSync sync = InboundSync(
    database: ref.watch(localDatabaseProvider),
    api: ref.watch(apiClientProvider),
    socket: ref.watch(socketClientProvider),
  );
  sync.start();
  ref.onDispose(sync.dispose);
  return sync;
});
