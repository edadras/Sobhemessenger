import 'dart:async';
import 'dart:io';

import 'package:dio/dio.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// The media kinds the server distinguishes (§13).
enum MediaKind {
  image('image'),
  video('video'),
  audio('audio'),
  voice('voice'),
  file('file'),
  avatar('avatar');

  const MediaKind(this.wire);

  final String wire;
}

/// One stored object and everything the UI needs to render it.
class Media {
  const Media({
    required this.id,
    required this.kind,
    required this.mimeType,
    required this.sizeBytes,
    required this.scanStatus,
    required this.processStatus,
    this.fileName = '',
    this.width,
    this.height,
    this.durationMs,
    this.blurhash,
    this.waveform = const <int>[],
    this.variants = const <MediaVariant>[],
  });

  factory Media.fromJson(Map<String, dynamic> json) => Media(
        id: json['id'] as String,
        kind: json['kind'] as String? ?? 'file',
        mimeType: json['mime_type'] as String? ?? 'application/octet-stream',
        sizeBytes: (json['size_bytes'] as num?)?.toInt() ?? 0,
        scanStatus: json['scan_status'] as String? ?? 'pending',
        processStatus: json['process_status'] as String? ?? 'pending',
        fileName: json['file_name'] as String? ?? '',
        width: (json['width'] as num?)?.toInt(),
        height: (json['height'] as num?)?.toInt(),
        durationMs: (json['duration_ms'] as num?)?.toInt(),
        blurhash: json['blurhash'] as String?,
        waveform: <int>[
          for (final dynamic sample
              in json['waveform'] as List<dynamic>? ?? const <dynamic>[])
            (sample as num).toInt(),
        ],
        variants: <MediaVariant>[
          for (final dynamic variant
              in json['variants'] as List<dynamic>? ?? const <dynamic>[])
            MediaVariant.fromJson(variant as Map<String, dynamic>),
        ],
      );

  final String id;
  final String kind;
  final String mimeType;
  final int sizeBytes;
  final String scanStatus;
  final String processStatus;
  final String fileName;
  final int? width;
  final int? height;
  final int? durationMs;
  final String? blurhash;
  final List<int> waveform;
  final List<MediaVariant> variants;

  bool get isImage => kind == 'image';
  bool get isVideo => kind == 'video';
  bool get isVoice => kind == 'voice';

  /// Whether the object is safe and finished processing. Until both are true
  /// the UI shows a placeholder rather than a link to bytes that may still be
  /// quarantined or half-rendered.
  bool get isReady =>
      (scanStatus == 'clean' || scanStatus == 'skipped') &&
      processStatus == 'ready';

  Duration? get duration =>
      durationMs == null ? null : Duration(milliseconds: durationMs!);

  /// Aspect ratio for reserving layout space before the bytes arrive, so the
  /// list does not jump when an image loads.
  double get aspectRatio {
    if (width == null || height == null || height == 0) {
      return 1;
    }
    return width! / height!;
  }
}

class MediaVariant {
  const MediaVariant({
    required this.variant,
    required this.mimeType,
    required this.sizeBytes,
    this.width,
    this.height,
  });

  factory MediaVariant.fromJson(Map<String, dynamic> json) => MediaVariant(
        variant: json['variant'] as String,
        mimeType: json['mime_type'] as String? ?? '',
        sizeBytes: (json['size_bytes'] as num?)?.toInt() ?? 0,
        width: (json['width'] as num?)?.toInt(),
        height: (json['height'] as num?)?.toInt(),
      );

  final String variant;
  final String mimeType;
  final int sizeBytes;
  final int? width;
  final int? height;
}

/// A presigned multipart upload the server has opened for us.
class UploadSession {
  const UploadSession({
    required this.sessionId,
    required this.mediaId,
    required this.partSize,
    required this.parts,
  });

  factory UploadSession.fromJson(Map<String, dynamic> json) => UploadSession(
        sessionId: json['session_id'] as String,
        mediaId: json['media_id'] as String,
        partSize: (json['part_size'] as num).toInt(),
        parts: <UploadPart>[
          for (final dynamic part
              in json['parts'] as List<dynamic>? ?? const <dynamic>[])
            UploadPart.fromJson(part as Map<String, dynamic>),
        ],
      );

  final String sessionId;
  final String mediaId;
  final int partSize;
  final List<UploadPart> parts;
}

class UploadPart {
  const UploadPart({
    required this.partNumber,
    required this.url,
    required this.size,
  });

  factory UploadPart.fromJson(Map<String, dynamic> json) => UploadPart(
        partNumber: (json['part_number'] as num).toInt(),
        url: json['url'] as String,
        size: (json['size'] as num).toInt(),
      );

  final int partNumber;
  final String url;
  final int size;
}

/// How far an upload has got, for the progress indicator on the bubble.
class UploadProgress {
  const UploadProgress({required this.sent, required this.total});

  final int sent;
  final int total;

  double get fraction => total == 0 ? 0 : (sent / total).clamp(0.0, 1.0);
}

/// Media upload and download (§13).
///
/// Bytes never pass through the API: the server issues presigned part URLs and
/// the client PUTs directly to object storage, then tells the server the parts
/// landed. That is what keeps a 2 GB video off the API's memory and CPU.
class MediaRepository {
  MediaRepository({required ApiClient api, Dio? uploader})
      : _api = api,
        // A bare Dio, with no auth interceptor and no base URL: a presigned URL
        // carries its own authorisation, and attaching our bearer token to a
        // third-party storage host would leak it.
        _uploader = uploader ?? Dio();

  final ApiClient _api;
  final Dio _uploader;

  /// Uploads a file and returns the finished [Media].
  ///
  /// [onProgress] is called as parts complete so the message bubble can show
  /// progress rather than a spinner of unknown length.
  Future<Media> upload({
    required File file,
    required MediaKind kind,
    required String mimeType,
    void Function(UploadProgress)? onProgress,
    CancelToken? cancelToken,
  }) async {
    final int size = await file.length();

    final UploadSession session = UploadSession.fromJson(
      await _api.post<Map<String, dynamic>>(
        '/media/uploads',
        body: <String, dynamic>{
          'kind': kind.wire,
          'mime_type': mimeType,
          'file_name': file.uri.pathSegments.last,
          'size': size,
        },
      ),
    );

    try {
      final RandomAccessFile handle = await file.open();
      int sent = 0;
      try {
        for (final UploadPart part in session.parts) {
          await handle.setPosition((part.partNumber - 1) * session.partSize);
          final List<int> chunk = await handle.read(part.size);

          final Response<dynamic> response = await _uploader.put<dynamic>(
            part.url,
            data: Stream<List<int>>.value(chunk),
            options: Options(
              headers: <String, dynamic>{
                Headers.contentLengthHeader: chunk.length,
              },
              contentType: mimeType,
            ),
            cancelToken: cancelToken,
          );

          // Object storage returns the part's ETag, which the server needs to
          // finish the multipart upload.
          final String etag =
              response.headers.value('etag')?.replaceAll('"', '') ?? '';
          await _api.post<Map<String, dynamic>>(
            '/media/uploads/${session.sessionId}/parts',
            body: <String, dynamic>{
              'part_number': part.partNumber,
              'etag': etag,
              'size': chunk.length,
            },
          );

          sent += chunk.length;
          onProgress?.call(UploadProgress(sent: sent, total: size));
        }
      } finally {
        await handle.close();
      }

      return Media.fromJson(
        await _api.post<Map<String, dynamic>>(
          '/media/uploads/${session.sessionId}/complete',
        ),
      );
    } on Object {
      // An abandoned session holds a partial object in storage. Aborting is
      // best effort: the maintenance worker also reaps stale sessions.
      unawaited(
        _api
            .delete<Map<String, dynamic>>('/media/uploads/${session.sessionId}')
            .catchError((Object _) => <String, dynamic>{}),
      );
      rethrow;
    }
  }

  Future<Media> get(String mediaId) async =>
      Media.fromJson(await _api.get<Map<String, dynamic>>('/media/$mediaId'));

  /// Resolves a short-lived download URL.
  ///
  /// `redirect=false` asks for the URL as JSON instead of a 307, because the
  /// image cache needs a URL it can hold, not a redirect it must follow.
  Future<String> downloadUrl(String mediaId, {String? variant}) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/media/$mediaId/download',
      query: <String, dynamic>{
        'redirect': 'false',
        if (variant != null) 'variant': variant,
      },
    );
    return data['url'] as String;
  }

  Future<void> delete(String mediaId) =>
      _api.delete<Map<String, dynamic>>('/media/$mediaId');
}

final Provider<MediaRepository> mediaRepositoryProvider =
    Provider<MediaRepository>(
  (Ref ref) => MediaRepository(api: ref.watch(apiClientProvider)),
);

final FutureProviderFamily<Media, String> mediaProvider =
    FutureProvider.family<Media, String>(
  (Ref ref, String mediaId) => ref.watch(mediaRepositoryProvider).get(mediaId),
);

/// The download URL for one object, cached by the provider so a rebuild does
/// not ask the server to sign a new URL for the same image.
final FutureProviderFamily<String, ({String mediaId, String? variant})>
    mediaUrlProvider =
    FutureProvider.family<String, ({String mediaId, String? variant})>(
  (Ref ref, ({String mediaId, String? variant}) key) => ref
      .watch(mediaRepositoryProvider)
      .downloadUrl(key.mediaId, variant: key.variant),
);
