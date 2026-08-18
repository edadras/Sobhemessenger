import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/secretchat/data/secret_chat_controller.dart';

/// Merging an encrypted conversation (§24).
///
/// This is where a message is most likely to be silently lost or silently
/// doubled. The inbox hands out every envelope for the device at once and
/// deletes each one as soon as it is acknowledged, so a message dropped here is
/// gone for good — there is no server-side history to re-read it from, unlike
/// every other conversation in the app.

SecretMessage _message(
  String id, {
  required int minute,
  bool isMine = false,
  String? text,
}) =>
    SecretMessage(
      clientMessageId: id,
      text: text ?? 'message $id',
      isMine: isMine,
      at: DateTime.utc(2026, 1, 1, 12, minute),
    );

List<String> _ids(List<SecretMessage> messages) => <String>[
      for (final SecretMessage message in messages) message.clientMessageId,
    ];

void main() {
  group('mergeSecretMessages', () {
    test('keeps both sides of the conversation', () {
      final List<SecretMessage> merged = mergeSecretMessages(
        <SecretMessage>[_message('a', minute: 1, isMine: true)],
        <SecretMessage>[_message('b', minute: 2)],
      );

      expect(_ids(merged), <String>['a', 'b']);
    });

    test('does not show a redelivered envelope twice', () {
      // Acknowledgement is a separate request. One that fails, or that the
      // server handles after the next poll begins, leaves the ciphertext in the
      // mailbox to be handed out again.
      final SecretMessage seen = _message('a', minute: 1);

      final List<SecretMessage> merged = mergeSecretMessages(
        <SecretMessage>[seen],
        <SecretMessage>[_message('a', minute: 1), _message('b', minute: 2)],
      );

      expect(_ids(merged), <String>['a', 'b']);
    });

    test('threads a late message into place rather than appending it', () {
      // A message sent while the device was offline arrives after one sent
      // later. Appending would put the reply above the thing it replied to.
      final List<SecretMessage> merged = mergeSecretMessages(
        <SecretMessage>[_message('later', minute: 30)],
        <SecretMessage>[_message('earlier', minute: 5)],
      );

      expect(_ids(merged), <String>['earlier', 'later']);
    });

    test('keeps the order things were seen in when times are equal', () {
      // Two messages can share a timestamp to the second. An unstable sort
      // would let them swap places between rebuilds, which reads as the
      // conversation rearranging itself while being looked at.
      final List<SecretMessage> first = mergeSecretMessages(
        const <SecretMessage>[],
        <SecretMessage>[
          _message('a', minute: 7),
          _message('b', minute: 7),
          _message('c', minute: 7),
        ],
      );
      final List<SecretMessage> again =
          mergeSecretMessages(first, const <SecretMessage>[]);

      expect(_ids(first), <String>['a', 'b', 'c']);
      expect(_ids(again), <String>['a', 'b', 'c']);
    });

    test('does not mutate the list it was given', () {
      // The conversation's state list is handed straight to the widget tree;
      // sorting it in place would change what is on screen without a rebuild.
      final List<SecretMessage> existing = <SecretMessage>[
        _message('later', minute: 30),
      ];
      mergeSecretMessages(
        existing,
        <SecretMessage>[_message('earlier', minute: 5)],
      );

      expect(_ids(existing), <String>['later']);
    });

    test('an empty arrival changes nothing', () {
      final List<SecretMessage> existing = <SecretMessage>[
        _message('a', minute: 1),
        _message('b', minute: 2),
      ];

      expect(
        _ids(mergeSecretMessages(existing, const <SecretMessage>[])),
        <String>['a', 'b'],
      );
    });
  });

  group('SecretConversation', () {
    test('clearError removes an error rather than keeping the old one', () {
      // copyWith cannot tell "no new error" from "clear the error" by null
      // alone, which is why the flag exists. Getting this wrong leaves a
      // failure banner on screen after a retry has succeeded.
      const SecretConversation failed = SecretConversation(
        messages: <SecretMessage>[],
        error: 'network',
      );

      expect(failed.copyWith(clearError: true).error, isNull);
      expect(failed.copyWith(isLoading: true).error, 'network');
    });
  });

  group('SecretChatArgs', () {
    test('two references to the same conversation are one provider', () {
      // Riverpod families key on argument equality. Without it, every rebuild
      // would construct a fresh controller, refetch the inbox, and lose the
      // messages already decrypted into the old one.
      const SecretChatArgs a = SecretChatArgs(chatId: 'c', peerUserId: 'u');
      const SecretChatArgs b = SecretChatArgs(chatId: 'c', peerUserId: 'u');
      const SecretChatArgs other = SecretChatArgs(chatId: 'c', peerUserId: 'v');

      expect(a, b);
      expect(a.hashCode, b.hashCode);
      expect(a, isNot(other));
    });
  });
}
