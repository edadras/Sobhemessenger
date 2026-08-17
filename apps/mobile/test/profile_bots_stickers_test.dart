import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/bots/data/bots_repository.dart';
import 'package:sobh_app/features/chat/data/organise_repository.dart';
import 'package:sobh_app/features/profile/data/profile_repository.dart';
import 'package:sobh_app/features/stickers/data/stickers_repository.dart';

/// Parsing tests for the four surfaces added alongside the bot platform.
///
/// These are about what the server can legitimately leave out. §55 says a field
/// the viewer may not see is *absent*, not blank, so every model has to
/// distinguish "not sent" from "empty" without crashing on either.
void main() {
  group('UserProfile', () {
    test('treats an absent username as no handle rather than an empty one', () {
      final UserProfile profile = UserProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'کاربر',
      });

      expect(profile.username, isNull);
      expect(profile.handle, isEmpty);
    });

    test('writes a claimed username as a handle', () {
      final UserProfile profile = UserProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'کاربر',
        'username': 'someone',
      });

      expect(profile.handle, '@someone');
    });

    test('leaves last seen null when privacy withheld it', () {
      // The server omits the field entirely rather than sending a placeholder,
      // so a client that treated missing as "never seen" would be inventing a
      // fact about someone.
      final UserProfile hidden = UserProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'کاربر',
      });
      expect(hidden.lastSeen, isNull);

      final UserProfile shown = UserProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'کاربر',
        'last_seen': '2026-01-01T10:00:00Z',
      });
      expect(shown.lastSeen, isNotNull);
    });

    test('defaults the relationship flags to no relationship', () {
      final UserProfile profile = UserProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'کاربر',
      });

      expect(profile.isContact, isFalse);
      expect(profile.isBlocked, isFalse);
      expect(profile.isBot, isFalse);
    });
  });

  group('SelfProfile', () {
    test('carries the fields only the owner sees', () {
      final SelfProfile self = SelfProfile.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'من',
        'phone_number': '+989120000000',
        'about': 'سلام',
        'language': 'fa',
      });

      expect(self.phoneNumber, '+989120000000');
      expect(self.about, 'سلام');
      expect(self.language, 'fa');
      expect(self.birthday, isNull);
    });
  });

  group('UsernameAvailability', () {
    test('reads available as available whatever reason says', () {
      final UsernameAvailability result =
          UsernameAvailability.fromJson(<String, dynamic>{
        'username': 'free_name',
        'available': true,
      });

      expect(result.isAvailable, isTrue);
      expect(result.status, UsernameStatus.available);
    });

    test('maps each refusal to its own status', () {
      UsernameAvailability of(String reason) =>
          UsernameAvailability.fromJson(<String, dynamic>{
            'username': 'x',
            'available': false,
            'reason': reason,
          });

      expect(of('invalid').status, UsernameStatus.invalid);
      expect(of('reserved').status, UsernameStatus.reserved);
      expect(of('taken').status, UsernameStatus.taken);
    });

    test('treats an unrecognised refusal as taken, not as available', () {
      // Failing open here would let the form offer a Claim button that the
      // server then refuses, so an unknown reason is still a refusal.
      final UsernameAvailability result =
          UsernameAvailability.fromJson(<String, dynamic>{
        'username': 'x',
        'available': false,
        'reason': 'something-new',
      });

      expect(result.isAvailable, isFalse);
      expect(result.status, UsernameStatus.taken);
    });
  });

  group('Bot', () {
    test('defaults privacy mode on', () {
      // The safe default is the private one: a bot registered by a client that
      // does not send the field must not silently see every message.
      final Bot bot = Bot.fromJson(<String, dynamic>{
        'user_id': 'b1',
        'owner_id': 'u1',
        'display_name': 'Helper',
        'created_at': '2026-01-01T00:00:00Z',
      });

      expect(bot.privacyMode, isTrue);
      expect(bot.isActive, isTrue);
      expect(bot.inlineEnabled, isFalse);
    });

    test('exposes the handle bots are addressed by', () {
      final Bot bot = Bot.fromJson(<String, dynamic>{
        'user_id': 'b1',
        'owner_id': 'u1',
        'display_name': 'Helper',
        'username': 'helperbot',
        'created_at': '2026-01-01T00:00:00Z',
      });

      expect(bot.handle, '@helperbot');
    });
  });

  group('IssuedToken', () {
    test('reads the secret from the field the server issues it under', () {
      final IssuedToken token = IssuedToken.fromJson(<String, dynamic>{
        'id': 't1',
        'token': 'sobh_live_abcdef',
        'prefix': 'sobh_live_ab',
      });

      expect(token.secret, 'sobh_live_abcdef');
      expect(token.prefix, 'sobh_live_ab');
    });
  });

  group('BotToken', () {
    test('never carries a secret, only enough to tell tokens apart', () {
      final BotToken token = BotToken.fromJson(<String, dynamic>{
        'id': 't1',
        'prefix': 'sobh_live_ab',
        'created_at': '2026-01-01T00:00:00Z',
      });

      expect(token.prefix, 'sobh_live_ab');
      expect(token.lastUsedAt, isNull);
    });
  });

  group('BotWebhook', () {
    test('reads an empty allowed list as every kind of update', () {
      final BotWebhook webhook = BotWebhook.fromJson(<String, dynamic>{
        'url': 'https://example.test/hook',
        'max_connections': 40,
        'allowed_updates': <dynamic>[],
      });

      expect(webhook.allowedUpdates, isEmpty);
      expect(webhook.lastError, isEmpty);
    });

    test('survives a registration that has never delivered', () {
      final BotWebhook webhook = BotWebhook.fromJson(<String, dynamic>{
        'url': 'https://example.test/hook',
      });

      expect(webhook.lastDeliveryAt, isNull);
      expect(webhook.maxConnections, 40);
    });
  });

  group('StickerSet', () {
    test('keeps the stickers in the order the server sent them', () {
      final StickerSet set = StickerSet.fromJson(<String, dynamic>{
        'id': 's1',
        'slug': 'cats',
        'title': 'گربه‌ها',
        'stickers': <dynamic>[
          <String, dynamic>{
            'id': 'a',
            'media_id': 'm1',
            'emoji': '😀',
            'position': 0,
          },
          <String, dynamic>{
            'id': 'b',
            'media_id': 'm2',
            'emoji': '😅',
            'position': 1,
          },
        ],
      });

      expect(set.stickers.map((Sticker s) => s.mediaId), <String>['m1', 'm2']);
      expect(set.isAdded, isFalse);
    });

    test('handles a set listed without its images', () {
      // Search returns sets without stickers; the picker returns them with.
      final StickerSet set = StickerSet.fromJson(<String, dynamic>{
        'id': 's1',
        'slug': 'cats',
        'title': 'گربه‌ها',
        'is_added': true,
      });

      expect(set.stickers, isEmpty);
      expect(set.isAdded, isTrue);
    });
  });

  group('LinkPreview', () {
    test('reads a preview that has only a title', () {
      final LinkPreview preview = LinkPreview.fromJson(<String, dynamic>{
        'url': 'https://example.test/article',
        'title': 'خبر',
      });

      expect(preview.title, 'خبر');
      expect(preview.description, isEmpty);
      expect(preview.imageMediaId, isNull);
    });
  });

  group('ScheduledMessage', () {
    test('reads a queued post that has not been tried yet', () {
      final ScheduledMessage post = ScheduledMessage.fromJson(<String, dynamic>{
        'id': 'q1',
        'chat_id': 'c1',
        'content': 'سلام',
        'scheduled_at': '2026-09-01T08:00:00Z',
      });

      expect(post.attempts, 0);
      expect(post.hasFailed, isFalse);
    });

    test('reports a post that could not be sent', () {
      // A failed post must be distinguishable from a pending one, or it sits
      // in the list looking like it is still going to go out.
      final ScheduledMessage post = ScheduledMessage.fromJson(<String, dynamic>{
        'id': 'q1',
        'chat_id': 'c1',
        'content': 'سلام',
        'scheduled_at': '2026-09-01T08:00:00Z',
        'attempts': 5,
        'last_error': 'not a member of this chat',
      });

      expect(post.hasFailed, isTrue);
      expect(post.attempts, 5);
      expect(post.lastError, contains('not a member'));
    });

    test('converts the send time to local, since that is what a user reads',
        () {
      final ScheduledMessage post = ScheduledMessage.fromJson(<String, dynamic>{
        'id': 'q1',
        'chat_id': 'c1',
        'content': 'سلام',
        'scheduled_at': '2026-09-01T08:00:00Z',
      });

      expect(post.scheduledAt.isUtc, isFalse);
      expect(
        post.scheduledAt.toUtc(),
        DateTime.utc(2026, 9, 1, 8),
      );
    });
  });
}
