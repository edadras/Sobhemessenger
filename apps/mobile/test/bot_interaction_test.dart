import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/chat/data/bot_interaction_repository.dart';

/// The wire shapes the chat screen draws bots with.
///
/// The server validates on the way in, but a client that crashes on an
/// unexpected shape takes the whole conversation down with it — one bad
/// message would be enough — so these pin what must survive being absent.
void main() {
  group('InlineResult', () {
    test('reads an article result', () {
      final InlineResult result = InlineResult.fromJson(<String, dynamic>{
        'id': 'margherita',
        'type': 'article',
        'title': 'مارگاریتا',
        'content': 'پیتزا مارگاریتا',
      });

      expect(result.id, 'margherita');
      expect(result.title, 'مارگاریتا');
      expect(result.content, 'پیتزا مارگاریتا');
      expect(result.mediaId, isNull);
    });

    test('defaults the type when the server omits it', () {
      // An unknown or absent type must still draw as a row rather than
      // throwing: the picker showing one plain entry beats an empty screen.
      final InlineResult result = InlineResult.fromJson(<String, dynamic>{
        'id': 'a',
        'title': 't',
      });

      expect(result.type, 'article');
      expect(result.description, isEmpty);
    });

    test('carries media and thumbnail ids for a media result', () {
      final InlineResult result = InlineResult.fromJson(<String, dynamic>{
        'id': 'photo-1',
        'type': 'photo',
        'title': 'عکس',
        'media_id': '11111111-1111-1111-1111-111111111111',
        'thumbnail_media_id': '22222222-2222-2222-2222-222222222222',
      });

      expect(result.mediaId, '11111111-1111-1111-1111-111111111111');
      expect(result.thumbnailMediaId, '22222222-2222-2222-2222-222222222222');
    });
  });

  group('InlineQuery', () {
    test('reads an open query', () {
      final InlineQuery query = InlineQuery.fromJson(<String, dynamic>{
        'id': '33333333-3333-3333-3333-333333333333',
        'query': 'pizza',
      });

      expect(query.id, '33333333-3333-3333-3333-333333333333');
      expect(query.query, 'pizza');
    });

    test('survives a query with no text, which is how a picker opens', () {
      // Typing `@bot ` with nothing after it is a real query: it is how a bot
      // offers its default suggestions.
      final InlineQuery query = InlineQuery.fromJson(<String, dynamic>{
        'id': '33333333-3333-3333-3333-333333333333',
      });

      expect(query.query, isEmpty);
    });
  });
}
