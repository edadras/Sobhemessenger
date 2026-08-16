import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/calls/data/calls_repository.dart';
import 'package:sobh_app/features/contacts/data/contacts_repository.dart';
import 'package:sobh_app/features/groups/data/groups_repository.dart';
import 'package:sobh_app/features/news/data/news_repository.dart';
import 'package:sobh_app/features/search/data/search_repository.dart';
import 'package:sobh_app/features/stories/data/stories_repository.dart';

/// The wire format is the contract with the backend. These tests pin the parts
/// of it the UI depends on, so a field renamed on the server fails here rather
/// than showing up as a blank screen.
void main() {
  group('Contact', () {
    test('prefers the locally saved name over the display name', () {
      final Contact contact = Contact.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'Chosen Name',
        'first_name': 'Local',
        'last_name': 'Name',
      });
      expect(contact.label, 'Local Name');
    });

    test('falls back to the display name when there is no local name', () {
      final Contact contact = Contact.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'Chosen Name',
      });
      expect(contact.label, 'Chosen Name');
    });

    test('leaves last seen null when privacy withheld it', () {
      final Contact contact = Contact.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'Someone',
      });
      expect(contact.lastSeen, isNull);
    });
  });

  group('Call', () {
    Map<String, dynamic> call({int? duration, String state = 'ended'}) =>
        <String, dynamic>{
          'id': 'c1',
          'initiator_id': 'u1',
          'type': 'audio',
          'state': state,
          'started_at': '2026-01-01T10:00:00Z',
          if (duration != null) 'duration_seconds': duration,
        };

    // A call that ended without ever connecting is a missed call. The history
    // list needs that distinction, and the raw state does not carry it.
    test('counts an ended call with no duration as missed', () {
      expect(Call.fromJson(call()).wasMissed, isTrue);
    });

    test('does not count a connected call as missed', () {
      expect(Call.fromJson(call(duration: 42)).wasMissed, isFalse);
    });

    test('does not count a ringing call as missed', () {
      expect(Call.fromJson(call(state: 'ringing')).wasMissed, isFalse);
    });
  });

  group('IceServer', () {
    test('omits empty credentials from the WebRTC configuration', () {
      final IceServer server = IceServer.fromJson(<String, dynamic>{
        'urls': <dynamic>['stun:stun.example.org:3478'],
      });
      expect(server.toConfiguration().containsKey('username'), isFalse);
      expect(server.toConfiguration().containsKey('credential'), isFalse);
    });

    test('carries TURN credentials when the server issued them', () {
      final IceServer server = IceServer.fromJson(<String, dynamic>{
        'urls': <dynamic>['turn:turn.example.org:3478'],
        'username': '1735689600',
        'credential': 'hmac',
      });
      expect(server.toConfiguration()['username'], '1735689600');
      expect(server.toConfiguration()['credential'], 'hmac');
    });
  });

  group('Member', () {
    test('treats the owner as an admin', () {
      final Member owner = Member.fromJson(<String, dynamic>{
        'user_id': 'u1',
        'display_name': 'A',
        'role': 'owner',
      });
      expect(owner.isAdmin, isTrue);
      expect(owner.isOwner, isTrue);
    });

    test('does not treat a plain member as an admin', () {
      final Member member = Member.fromJson(<String, dynamic>{
        'user_id': 'u2',
        'display_name': 'B',
        'role': 'member',
      });
      expect(member.isAdmin, isFalse);
    });
  });

  group('DiscoverableChat', () {
    test('accepts either chat_id or id, since both endpoints return it', () {
      expect(
        DiscoverableChat.fromJson(
          <String, dynamic>{'chat_id': 'c1', 'title': 'T', 'type': 'channel'},
        ).chatId,
        'c1',
      );
      expect(
        DiscoverableChat.fromJson(
          <String, dynamic>{'id': 'c2', 'title': 'T', 'type': 'group'},
        ).chatId,
        'c2',
      );
    });
  });

  group('Article', () {
    test('survives a feed entry that carries no body', () {
      final Article article = Article.fromJson(<String, dynamic>{
        'id': 'a1',
        'slug': 'headline',
        'title': 'Headline',
        'reading_minutes': 3,
      });
      expect(article.body, isEmpty);
      expect(article.readingMinutes, 3);
      expect(article.isBreaking, isFalse);
    });
  });

  group('NewsCategory', () {
    test('uses the resolved name when the server localised it', () {
      final NewsCategory category = NewsCategory.fromJson(
        <String, dynamic>{'id': 'c1', 'slug': 'politics', 'name': 'سیاست'},
      );
      expect(category.name, 'سیاست');
    });

    test('falls back to the per-locale map when no name was resolved', () {
      final NewsCategory category = NewsCategory.fromJson(<String, dynamic>{
        'id': 'c1',
        'slug': 'politics',
        'names': <String, dynamic>{'fa': 'سیاست'},
      });
      expect(category.name, 'سیاست');
    });
  });

  group('SearchResult', () {
    test('prefers the highlighted snippet over the raw content', () {
      final SearchResult result = SearchResult.fromJson(<String, dynamic>{
        'id': 'm1',
        'fields': <String, dynamic>{'content': 'the full message body'},
        'highlight': <String, dynamic>{
          'content': <dynamic>['the <em>matched</em> part'],
        },
      });
      expect(result.snippet, 'the <em>matched</em> part');
    });

    test('falls back to the content when nothing was highlighted', () {
      final SearchResult result = SearchResult.fromJson(<String, dynamic>{
        'id': 'm1',
        'fields': <String, dynamic>{'content': 'the full message body'},
      });
      expect(result.snippet, 'the full message body');
    });

    test('reads a title from whichever field the index provides', () {
      expect(
        SearchResult.fromJson(<String, dynamic>{
          'id': 'u1',
          'fields': <String, dynamic>{'display_name': 'Someone'},
        }).title,
        'Someone',
      );
    });
  });

  group('SearchResults', () {
    test('reports empty when nothing matched', () {
      expect(
        SearchResults.fromJson(<String, dynamic>{'query': 'x', 'total': 0})
            .isEmpty,
        isTrue,
      );
    });
  });

  group('Story', () {
    test('parses expiry so the viewer can show time remaining', () {
      final Story story = Story.fromJson(<String, dynamic>{
        'id': 's1',
        'author_id': 'u1',
        'created_at': '2026-01-01T10:00:00Z',
        'expires_at': '2026-01-02T10:00:00Z',
      });
      expect(story.expiresAt.difference(story.createdAt).inHours, 24);
      expect(story.seenByMe, isFalse);
    });
  });
}
