import 'dart:convert';
import 'dart:typed_data';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_admin/core/admin_api.dart';

/// What the panel actually puts on the wire.
///
/// The panel is the only client for several of these endpoints, so a wrong
/// method or a misspelt field is a feature that silently does not exist —
/// which is how the newsroom ended up with galleries and translations in the
/// database that nothing could reach. These assert the request, not the
/// server's answer, because the request is the part the panel owns.
class _RecordingAdapter implements HttpClientAdapter {
  _RecordingAdapter(this.body);

  /// The envelope to answer every call with.
  final Map<String, dynamic> body;
  final List<RequestOptions> calls = <RequestOptions>[];

  @override
  Future<ResponseBody> fetch(
    RequestOptions options,
    Stream<Uint8List>? requestStream,
    Future<void>? cancelFuture,
  ) async {
    calls.add(options);
    return ResponseBody.fromString(
      jsonEncode(<String, dynamic>{'success': true, 'data': body}),
      200,
      headers: <String, List<String>>{
        Headers.contentTypeHeader: <String>[Headers.jsonContentType],
      },
    );
  }

  @override
  void close({bool force = false}) {}
}

({AdminApi api, _RecordingAdapter adapter}) _client([
  Map<String, dynamic> body = const <String, dynamic>{},
]) {
  final Dio dio = Dio();
  final _RecordingAdapter adapter = _RecordingAdapter(body);
  dio.httpClientAdapter = adapter;
  final AdminApi api = AdminApi(baseUrl: 'http://server', dio: dio);
  api.setAccessToken('token');
  return (api: api, adapter: adapter);
}

Map<String, dynamic> _sent(RequestOptions options) =>
    options.data as Map<String, dynamic>;

void main() {
  group('newsroom', () {
    test('saving a new author omits the id rather than sending an empty one',
        () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.saveAuthor(displayName: 'سردبیر');

      final RequestOptions call = c.adapter.calls.single;
      expect(call.method, 'PUT');
      expect(call.path, '/editorial/authors');
      // An empty id would be read as "update the author whose id is nothing"
      // rather than as "create one".
      expect(_sent(call).containsKey('id'), isFalse);
      expect(_sent(call).containsKey('user_id'), isFalse);
      expect(_sent(call)['display_name'], 'سردبیر');
      expect(_sent(call)['is_active'], isTrue);
    });

    test('editing an author carries its id', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.saveAuthor(
        id: 'author-1',
        displayName: 'سردبیر',
        userId: 'user-9',
        isActive: false,
      );

      final Map<String, dynamic> body = _sent(c.adapter.calls.single);
      expect(body['id'], 'author-1');
      expect(body['user_id'], 'user-9');
      expect(body['is_active'], isFalse);
    });

    test('replacing a gallery sends the whole ordered list', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.replaceGallery('article-1', <Map<String, dynamic>>[
        <String, dynamic>{
          'media_id': 'm1',
          'position': 0,
          'caption': 'یک',
        },
        <String, dynamic>{'media_id': 'm2', 'position': 1, 'caption': ''},
      ]);

      final RequestOptions call = c.adapter.calls.single;
      expect(call.method, 'PUT');
      expect(call.path, '/editorial/articles/article-1/gallery');
      final List<dynamic> items = _sent(call)['items'] as List<dynamic>;
      expect(items, hasLength(2));
      expect((items.first as Map<String, dynamic>)['position'], 0);
    });

    test('a translation is addressed by its locale', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.saveTranslation(
        'article-1',
        'en',
        title: 'Headline',
        source: 'machine',
      );

      final RequestOptions call = c.adapter.calls.single;
      expect(call.method, 'PUT');
      expect(call.path, '/editorial/articles/article-1/translations/en');
      expect(_sent(call)['title'], 'Headline');
      // The distinction between a machine draft and a translation someone has
      // read is the whole point of approving one, so it has to survive the
      // round trip.
      expect(_sent(call)['source'], 'machine');
    });

    test('approving posts to the approve path', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.approveTranslation('article-1', 'ar');

      final RequestOptions call = c.adapter.calls.single;
      expect(call.method, 'POST');
      expect(
        call.path,
        '/editorial/articles/article-1/translations/ar/approve',
      );
    });

    test('deleting a translation uses DELETE', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.deleteTranslation('article-1', 'tr');

      final RequestOptions call = c.adapter.calls.single;
      expect(call.method, 'DELETE');
      expect(call.path, '/editorial/articles/article-1/translations/tr');
    });
  });

  group('anti-spam', () {
    test('a score is read, not searched for', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client(
        <String, dynamic>{
          'score': <String, dynamic>{'score': 60},
          'restricted': true,
          'threshold': 50,
        },
      );
      final Map<String, dynamic> result = await c.api.spamScore('user-1');

      expect(c.adapter.calls.single.method, 'GET');
      expect(c.adapter.calls.single.path, '/admin/spam-scores/user-1');
      // The threshold travels with the score: a number on its own does not
      // tell the operator whether it is high.
      expect(result['threshold'], 50);
      expect(result['restricted'], isTrue);
    });

    test('lifting a restriction uses DELETE', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.liftSpamScore('user-1');

      expect(c.adapter.calls.single.method, 'DELETE');
      expect(c.adapter.calls.single.path, '/admin/spam-scores/user-1');
    });
  });

  group('authorisation', () {
    test('every call carries the operator’s token', () async {
      final ({AdminApi api, _RecordingAdapter adapter}) c = _client();
      await c.api.authors();
      await c.api.liftSpamScore('user-1');

      for (final RequestOptions call in c.adapter.calls) {
        expect(call.headers['Authorization'], 'Bearer token');
      }
    });
  });
}
