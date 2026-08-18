import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/chat/data/folders_repository.dart';
import 'package:sobh_app/features/chat/data/topics_repository.dart';
import 'package:sobh_app/features/contacts/data/contact_requests_repository.dart';
import 'package:sobh_app/features/groups/data/comments_repository.dart';
import 'package:sobh_app/features/groups/data/groups_repository.dart';
import 'package:sobh_app/features/settings/data/account_repository.dart';

/// The models behind chat folders, forum topics, comments, contact requests
/// and email recovery.
///
/// These are the seams where a field the server renamed, or one the client
/// forgot to read, turns into a screen that silently shows the wrong thing.
/// Each test below is a claim about what a particular field means rather than
/// a restatement of the JSON.

void main() {
  group('ChatFolder', () {
    test('carries the two lists that override the rules', () {
      final ChatFolder folder = ChatFolder.fromJson(<String, dynamic>{
        'id': 'f1',
        'title': 'Work',
        'include_groups': true,
        'included_chat_ids': <String>['a'],
        'excluded_chat_ids': <String>['b'],
        'unread_count': 4,
        'chat_count': 9,
      });

      expect(folder.includeGroups, isTrue);
      expect(folder.includedChatIds, <String>['a']);
      expect(folder.excludedChatIds, <String>['b']);
      expect(folder.unreadCount, 4);
    });

    test('defaults to hiding archived chats', () {
      // The server's default too. A folder that showed archived conversations
      // by default would undo archiving for anyone who made one.
      final ChatFolder folder =
          ChatFolder.fromJson(<String, dynamic>{'id': 'f1', 'title': 'Work'});
      expect(folder.excludeArchived, isTrue);
    });

    test('sends the whole definition, because that is how it is edited', () {
      const ChatFolder folder = ChatFolder(
        id: 'f1',
        title: 'Work',
        includeGroups: true,
        excludeRead: true,
      );
      final Map<String, dynamic> body = folder.toJson();

      expect(body['title'], 'Work');
      expect(body['include_groups'], isTrue);
      expect(body['exclude_read'], isTrue);
      // Every flag is present, including the ones left off: a filter is
      // replaced rather than patched.
      expect(body.containsKey('include_bots'), isTrue);
    });
  });

  group('ForumTopic', () {
    test('distinguishes the General topic, which cannot be deleted', () {
      final ForumTopic general = ForumTopic.fromJson(<String, dynamic>{
        'id': 't1',
        'chat_id': 'c1',
        'title': 'General',
        'is_general': true,
        'last_message_at': '2026-01-01T00:00:00Z',
      });
      expect(general.isGeneral, isTrue);
      expect(general.isClosed, isFalse);
    });

    test('reads the caller\'s own unread count, not the topic\'s length', () {
      final ForumTopic topic = ForumTopic.fromJson(<String, dynamic>{
        'id': 't2',
        'chat_id': 'c1',
        'title': 'Releases',
        'message_count': 40,
        'unread_count': 3,
        'last_message_at': '2026-01-01T00:00:00Z',
      });
      expect(topic.messageCount, 40);
      expect(topic.unreadCount, 3);
    });
  });

  group('CommentThread', () {
    test('names the discussion chat and the mirrored post', () {
      // A comment is a reply to the mirror, so both ids are needed to read or
      // write one; a comment id of its own would not exist.
      final CommentThread thread = CommentThread.fromJson(<String, dynamic>{
        'post_message_id': 'p1',
        'discussion_chat_id': 'g1',
        'root_message_id': 'm1',
        'comment_count': 7,
      });

      expect(thread.discussionChatId, 'g1');
      expect(thread.rootMessageId, 'm1');
      expect(thread.commentCount, 7);
    });

    test('a deleted comment is recognisable as one', () {
      final Comment comment = Comment.fromJson(<String, dynamic>{
        'id': 'c1',
        'seq': 5,
        'content': '',
        'created_at': '2026-01-01T00:00:00Z',
        'deleted_at': '2026-01-02T00:00:00Z',
      });
      expect(comment.deleted, isTrue);
    });
  });

  group('ContactRequest', () {
    test('a pending request is the only kind that can be acted on', () {
      final ContactRequest pending = ContactRequest.fromJson(<String, dynamic>{
        'id': 'r1',
        'requester_id': 'u1',
        'target_id': 'u2',
        'status': 'pending',
        'created_at': '2026-01-01T00:00:00Z',
        'display_name': 'Alice',
      });
      final ContactRequest rejected = ContactRequest.fromJson(<String, dynamic>{
        'id': 'r2',
        'requester_id': 'u1',
        'target_id': 'u2',
        'status': 'rejected',
        'created_at': '2026-01-01T00:00:00Z',
        'resolved_at': '2026-01-02T00:00:00Z',
      });

      expect(pending.isPending, isTrue);
      expect(pending.displayName, 'Alice');
      expect(rejected.isPending, isFalse);
      expect(rejected.resolvedAt, isNotNull);
    });
  });

  group('RecoveryEmail', () {
    test('an address that exists is not the same as one that is proved', () {
      final RecoveryEmail enrolled = RecoveryEmail.fromJson(<String, dynamic>{
        'email': 'a***a@example.com',
        'verified': false,
        'available': true,
      });

      expect(enrolled.isSet, isTrue);
      // The distinction the whole flow turns on: an unverified address does
      // nothing, and the screen has to be able to say so.
      expect(enrolled.verified, isFalse);
    });

    test('reports when the server has no mail transport at all', () {
      final RecoveryEmail unavailable =
          RecoveryEmail.fromJson(<String, dynamic>{'available': false});
      expect(unavailable.available, isFalse);
      expect(unavailable.isSet, isFalse);
    });
  });

  group('ChatSettings', () {
    test('omits the type-specific settings it was not given', () {
      // This is what stops an older client from switching off commenting on
      // every channel it touches by not mentioning it.
      final ChatSettings group = ChatSettings.fromJson(<String, dynamic>{
        'slow_mode_seconds': 30,
        'sticker_set': 'team_pack',
        'is_broadcast': false,
      });

      expect(group.signatureEnabled, isNull);
      expect(group.commentsEnabled, isNull);
      final Map<String, dynamic> body = group.toJson();
      expect(body.containsKey('signature_enabled'), isFalse);
      expect(body.containsKey('comments_enabled'), isFalse);
      expect(body['sticker_set'], 'team_pack');
    });

    test('sends a channel setting once it has one to send', () {
      final ChatSettings channel = ChatSettings.fromJson(<String, dynamic>{
        'signature_enabled': false,
        'comments_enabled': true,
      });

      final Map<String, dynamic> body =
          channel.copyWith(signatureEnabled: true).toJson();
      expect(body['signature_enabled'], isTrue);
      expect(body['comments_enabled'], isTrue);
      // And still nothing about a group's sticker set.
      expect(body.containsKey('sticker_set'), isFalse);
    });
  });
}
