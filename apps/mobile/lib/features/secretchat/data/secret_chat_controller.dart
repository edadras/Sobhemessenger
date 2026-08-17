import 'dart:async';

import 'package:collection/collection.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';
import 'secret_chat_service.dart';

/// The state of one encrypted conversation while it is open (§24).
///
/// ## Why nothing here is written to the database
///
/// Every other conversation in SOBH is offline-first: messages are stored in
/// the local Drift database so they survive a restart. That database is not
/// encrypted at rest. Writing decrypted secret-chat text into it would move the
/// plaintext from a place the operating system protects to a file any process
/// with the device's storage can read, and the end-to-end encryption would then
/// only be protecting the message in transit — which is the one part of the
/// journey it was already safe on.
///
/// So decrypted text lives in memory, for as long as the conversation is open,
/// and nowhere else. Closing the app loses the history. That is the price of
/// the guarantee rather than an omission: the ciphertext on the server has
/// already been acknowledged and deleted, and the key material that could
/// re-open it is deliberately single-use.
///
/// The path to keeping history without giving it up is an encrypted local store
/// (SQLCipher under Drift, keyed from the platform keystore). That is a change
/// to the whole local database rather than to this feature, and it is recorded
/// in the roadmap rather than half-done here.
class SecretConversation {
  const SecretConversation({
    required this.messages,
    this.isLoading = false,
    this.error,
  });

  final List<SecretMessage> messages;
  final bool isLoading;

  /// A failure worth showing. A message that will not open is not the same as
  /// no messages, and must not be rendered as an empty conversation.
  final Object? error;

  SecretConversation copyWith({
    List<SecretMessage>? messages,
    bool? isLoading,
    Object? error,
    bool clearError = false,
  }) =>
      SecretConversation(
        messages: messages ?? this.messages,
        isLoading: isLoading ?? this.isLoading,
        error: clearError ? null : (error ?? this.error),
      );
}

/// One message in an encrypted conversation, after decryption or before
/// sending.
class SecretMessage {
  const SecretMessage({
    required this.clientMessageId,
    required this.text,
    required this.isMine,
    required this.at,
    this.isSending = false,
    this.failed = false,
  });

  final String clientMessageId;
  final String text;
  final bool isMine;
  final DateTime at;
  final bool isSending;
  final bool failed;

  SecretMessage copyWith({bool? isSending, bool? failed}) => SecretMessage(
        clientMessageId: clientMessageId,
        text: text,
        isMine: isMine,
        at: at,
        isSending: isSending ?? this.isSending,
        failed: failed ?? this.failed,
      );
}

/// Drives one encrypted conversation: fetches the inbox, decrypts, sends.
class SecretChatController extends StateNotifier<SecretConversation> {
  SecretChatController({
    required SecretChatService service,
    required SocketClient socket,
    required this.chatId,
    required this.peerUserId,
    List<DecryptedMessage> alreadyArrived = const <DecryptedMessage>[],
  })  : _service = service,
        super(
          SecretConversation(
            // Anything decrypted for this chat while it was closed. It is
            // shown from the first frame rather than after the first poll: the
            // server has already deleted those envelopes, so this is the only
            // copy.
            messages: _toMessages(alreadyArrived, chatId),
            isLoading: true,
          ),
        ) {
    // The server's announcement carries routing only — never ciphertext — so
    // arrival is a signal to go and look, not the message itself.
    _subscription = socket.events
        .where((SocketFrame frame) => frame.event == 'secret.message')
        .listen((SocketFrame frame) {
      if (frame.payload['chat_id'] == chatId) {
        unawaited(refresh());
      }
    });

    unawaited(refresh());
  }

  final SecretChatService _service;
  final String chatId;
  final String peerUserId;

  StreamSubscription<SocketFrame>? _subscription;

  /// Opens whatever is waiting in the mailbox.
  ///
  /// The inbox is device-wide rather than per-chat, so envelopes for other
  /// conversations are decrypted here too. Dropping them would be worse than
  /// useless: they are acknowledged on decryption and the server then deletes
  /// them, so a message discarded here is a message lost. They are held for
  /// their own conversation to pick up.
  Future<void> refresh() async {
    try {
      final List<DecryptedMessage> opened = await _service.receive();
      _stash(opened);

      state = state.copyWith(
        messages: _merge(state.messages, _forThisChat(opened)),
        isLoading: false,
        clearError: true,
      );
    } on Object catch (error) {
      state = state.copyWith(isLoading: false, error: error);
    }
  }

  Future<void> send(String text) async {
    final String trimmed = text.trim();
    if (trimmed.isEmpty) {
      return;
    }

    // Shown immediately with a pending mark. Unlike an ordinary message this
    // is not queued for retry: an outbox would mean holding the plaintext
    // somewhere durable, which is exactly what this conversation does not do.
    final SecretMessage pending = SecretMessage(
      clientMessageId: 'pending-${DateTime.now().microsecondsSinceEpoch}',
      text: trimmed,
      isMine: true,
      at: DateTime.now(),
      isSending: true,
    );
    state = state.copyWith(
      messages: <SecretMessage>[...state.messages, pending],
      clearError: true,
    );

    try {
      final String id = await _service.send(
        chatId: chatId,
        recipientUserId: peerUserId,
        plaintext: trimmed,
      );
      _replace(
        pending.clientMessageId,
        SecretMessage(
          clientMessageId: id,
          text: trimmed,
          isMine: true,
          at: pending.at,
        ),
      );
    } on Object catch (error) {
      _replace(
        pending.clientMessageId,
        pending.copyWith(isSending: false, failed: true),
      );
      state = state.copyWith(error: error);
    }
  }

  /// Re-sends a message that failed. The text is still in memory, so this does
  /// not need an outbox.
  Future<void> retry(SecretMessage message) async {
    state = state.copyWith(
      messages: state.messages
          .where((SecretMessage m) => m.clientMessageId != message.clientMessageId)
          .toList(),
    );
    await send(message.text);
  }

  void _replace(String clientMessageId, SecretMessage replacement) {
    state = state.copyWith(
      messages: <SecretMessage>[
        for (final SecretMessage message in state.messages)
          if (message.clientMessageId == clientMessageId)
            replacement
          else
            message,
      ],
    );
  }

  List<SecretMessage> _forThisChat(List<DecryptedMessage> opened) =>
      _toMessages(opened, chatId);

  static List<SecretMessage> _toMessages(
    List<DecryptedMessage> opened,
    String chatId,
  ) =>
      <SecretMessage>[
        for (final DecryptedMessage message in opened)
          if (message.envelope.chatId == chatId)
            SecretMessage(
              clientMessageId: message.envelope.clientMessageId,
              text: message.plaintext,
              isMine: false,
              at: message.envelope.createdAt,
            ),
      ];

  /// Holds messages belonging to other conversations so opening one of those
  /// later still shows them. The stash is process-wide and in memory, on the
  /// same terms as everything else here.
  void _stash(List<DecryptedMessage> opened) {
    for (final DecryptedMessage message in opened) {
      if (message.envelope.chatId == chatId) {
        continue;
      }
      pendingSecretMessages
          .putIfAbsent(message.envelope.chatId, () => <DecryptedMessage>[])
          .add(message);
    }
  }

  static List<SecretMessage> _merge(
    List<SecretMessage> existing,
    List<SecretMessage> arriving,
  ) =>
      mergeSecretMessages(existing, arriving);

  @override
  void dispose() {
    unawaited(_subscription?.cancel());
    super.dispose();
  }
}

/// Merges arriving messages into a conversation without duplicating them.
///
/// The same envelope can be seen twice: acknowledgement is a separate request,
/// so one that fails — or that the server processes after the next poll starts
/// — leaves the ciphertext in the mailbox to be handed out again. Showing the
/// message twice would be the visible symptom.
///
/// The result is ordered by time rather than by arrival. A message sent while
/// the device was offline arrives after one sent later, and threading it into
/// place is the difference between a conversation and a list.
List<SecretMessage> mergeSecretMessages(
  List<SecretMessage> existing,
  List<SecretMessage> arriving,
) {
  final Set<String> known = <String>{
    for (final SecretMessage message in existing) message.clientMessageId,
  };
  final List<SecretMessage> merged = <SecretMessage>[
    ...existing,
    for (final SecretMessage message in arriving)
      if (known.add(message.clientMessageId)) message,
  ];
  // A stable sort, so two messages sharing a timestamp keep the order they
  // were seen in rather than swapping about between rebuilds.
  mergeSort(merged, compare: (SecretMessage a, SecretMessage b) => a.at.compareTo(b.at));
  return merged;
}

/// Decrypted messages for conversations that were not open when they arrived.
///
/// The inbox hands out every envelope for the device at once, and an envelope
/// is deleted from the server as soon as it is acknowledged — so one that is
/// decrypted and then dropped is gone for good. Keyed by chat id.
final Map<String, List<DecryptedMessage>> pendingSecretMessages =
    <String, List<DecryptedMessage>>{};

/// One controller per conversation, disposed when the screen leaves.
final AutoDisposeStateNotifierProviderFamily<SecretChatController,
        SecretConversation, SecretChatArgs> secretChatControllerProvider =
    StateNotifierProvider.autoDispose
        .family<SecretChatController, SecretConversation, SecretChatArgs>(
  (Ref ref, SecretChatArgs args) => SecretChatController(
        service: ref.watch(secretChatServiceProvider),
        socket: ref.watch(socketClientProvider),
        chatId: args.chatId,
        peerUserId: args.peerUserId,
        // Removed from the stash as it is handed over: it is the only copy,
        // and leaving it behind would show every message twice on the next
        // open.
        alreadyArrived:
            pendingSecretMessages.remove(args.chatId) ?? const <DecryptedMessage>[],
      ),
);

/// Identifies a conversation. Riverpod families compare arguments by equality,
/// so this needs both fields to take part in it.
class SecretChatArgs {
  const SecretChatArgs({required this.chatId, required this.peerUserId});

  final String chatId;
  final String peerUserId;

  @override
  bool operator ==(Object other) =>
      other is SecretChatArgs &&
      other.chatId == chatId &&
      other.peerUserId == peerUserId;

  @override
  int get hashCode => Object.hash(chatId, peerUserId);
}
