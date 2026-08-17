import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/features/calls/data/call_controller.dart';
import 'package:sobh_app/features/communities/data/communities_repository.dart';
import 'package:sobh_app/features/media/data/media_repository.dart';
import 'package:sobh_app/features/media/presentation/attachment_picker.dart';
import 'package:sobh_app/features/media/presentation/media_widgets.dart';
import 'package:sobh_app/features/polls/data/polls_repository.dart';

void main() {
  group('Media', () {
    Map<String, dynamic> media({
      String scan = 'clean',
      String process = 'ready',
      int? width,
      int? height,
    }) =>
        <String, dynamic>{
          'id': 'm1',
          'kind': 'image',
          'mime_type': 'image/jpeg',
          'size_bytes': 1024,
          'scan_status': scan,
          'process_status': process,
          if (width != null) 'width': width,
          if (height != null) 'height': height,
        };

    // Until an object is both scanned and processed, showing it would mean
    // linking to bytes that may be quarantined or half-rendered.
    test('is not ready while the scan is pending', () {
      expect(Media.fromJson(media(scan: 'pending')).isReady, isFalse);
    });

    test('is not ready while processing is unfinished', () {
      expect(Media.fromJson(media(process: 'processing')).isReady, isFalse);
    });

    test('is never ready when the scanner found something', () {
      expect(Media.fromJson(media(scan: 'infected')).isReady, isFalse);
    });

    test('is ready when the scan was skipped and processing finished', () {
      expect(Media.fromJson(media(scan: 'skipped')).isReady, isTrue);
    });

    test('reports the aspect ratio so the layout can reserve space', () {
      expect(
        Media.fromJson(media(width: 1600, height: 900)).aspectRatio,
        closeTo(16 / 9, 0.001),
      );
    });

    // A missing or zero dimension must not divide by zero in a build method.
    test('falls back to square when dimensions are unknown', () {
      expect(Media.fromJson(media()).aspectRatio, 1);
      expect(Media.fromJson(media(width: 100, height: 0)).aspectRatio, 1);
    });
  });

  group('AttachmentPicker.mimeTypeFor', () {
    test('maps the extensions the server accepts', () {
      expect(
        AttachmentPicker.mimeTypeFor('a.jpg', fallback: 'x'),
        'image/jpeg',
      );
      expect(AttachmentPicker.mimeTypeFor('a.PNG', fallback: 'x'), 'image/png');
      expect(AttachmentPicker.mimeTypeFor('a.mp4', fallback: 'x'), 'video/mp4');
      expect(
        AttachmentPicker.mimeTypeFor('a.opus', fallback: 'x'),
        'audio/ogg',
      );
      expect(
        AttachmentPicker.mimeTypeFor('a.pdf', fallback: 'x'),
        'application/pdf',
      );
    });

    // A declared type is only a hint; the server sniffs the magic bytes. So an
    // unknown extension falls back rather than guessing.
    test('falls back for an extension it does not know', () {
      expect(AttachmentPicker.mimeTypeFor('a.xyz', fallback: 'x'), 'x');
      expect(AttachmentPicker.mimeTypeFor('noextension', fallback: 'x'), 'x');
    });
  });

  group('PickedAttachment', () {
    test('maps each kind to the message type that carries it', () {
      String typeFor(MediaKind kind) => PickedAttachment(
            file: File('/tmp/example'),
            kind: kind,
            mimeType: 'application/octet-stream',
          ).messageType;

      expect(typeFor(MediaKind.image), 'photo');
      expect(typeFor(MediaKind.video), 'video');
      expect(typeFor(MediaKind.voice), 'voice');
      expect(typeFor(MediaKind.file), 'document');
    });
  });

  group('formatBytes', () {
    test('uses binary units, like a file manager', () {
      expect(formatBytes(512), '512 B');
      expect(formatBytes(2048), '2.0 KB');
      expect(formatBytes(5 * 1024 * 1024), '5.0 MB');
    });
  });

  group('formatDuration', () {
    test('is mm:ss below an hour and h:mm:ss above it', () {
      expect(formatDuration(const Duration(seconds: 5)), '00:05');
      expect(formatDuration(const Duration(minutes: 3, seconds: 7)), '03:07');
      expect(
        formatDuration(const Duration(hours: 1, minutes: 2, seconds: 3)),
        '1:02:03',
      );
    });
  });

  group('UploadProgress', () {
    test('reports a fraction that never leaves the unit interval', () {
      expect(const UploadProgress(sent: 0, total: 0).fraction, 0);
      expect(const UploadProgress(sent: 50, total: 100).fraction, 0.5);
      expect(const UploadProgress(sent: 200, total: 100).fraction, 1);
    });
  });

  group('Poll', () {
    Map<String, dynamic> poll({
      List<String> myVotes = const <String>[],
      String? closedAt,
      int totalVoters = 4,
    }) =>
        <String, dynamic>{
          'id': 'p1',
          'chat_id': 'c1',
          'question': 'Which?',
          'total_voters': totalVoters,
          'options': <dynamic>[
            <String, dynamic>{
              'id': 'o1',
              'position': 0,
              'text': 'A',
              'vote_count': 3,
            },
            <String, dynamic>{
              'id': 'o2',
              'position': 1,
              'text': 'B',
              'vote_count': 1,
            },
          ],
          'my_votes': myVotes,
          if (closedAt != null) 'closed_at': closedAt,
        };

    // Showing results before someone has voted would bias their vote.
    test('hides results until the viewer has voted', () {
      expect(Poll.fromJson(poll()).showsResults, isFalse);
      expect(
        Poll.fromJson(poll(myVotes: <String>['o1'])).showsResults,
        isTrue,
      );
    });

    test('shows results once the poll is closed, voted or not', () {
      expect(
        Poll.fromJson(poll(closedAt: '2026-01-01T00:00:00Z')).showsResults,
        isTrue,
      );
    });

    test('computes each option share of the total voters', () {
      final Poll data = Poll.fromJson(poll());
      expect(data.shareOf(data.options.first), 0.75);
      expect(data.shareOf(data.options.last), 0.25);
    });

    test('does not divide by zero before the first vote', () {
      final Poll data = Poll.fromJson(poll(totalVoters: 0));
      expect(data.shareOf(data.options.first), 0);
    });
  });

  group('Community', () {
    test('groups rooms by section, preserving the author order', () {
      final Community community = Community.fromJson(<String, dynamic>{
        'id': 'c1',
        'title': 'Neighbourhood',
        'member_count': 12,
        'is_public': true,
        'rooms': <dynamic>[
          <String, dynamic>{
            'chat_id': 'r1',
            'title': 'General',
            'section': 'Talk',
          },
          <String, dynamic>{
            'chat_id': 'r2',
            'title': 'Notices',
            'section': 'Info',
          },
          <String, dynamic>{
            'chat_id': 'r3',
            'title': 'Random',
            'section': 'Talk',
          },
        ],
      });

      expect(community.roomsBySection.keys.toList(), <String>['Talk', 'Info']);
      expect(community.roomsBySection['Talk']!.length, 2);
      expect(community.roomsBySection['Talk']!.first.title, 'General');
    });

    test('is not a member without a role', () {
      expect(
        Community.fromJson(<String, dynamic>{
          'id': 'c1',
          'title': 'T',
          'member_count': 0,
          'is_public': true,
        }).isMember,
        isFalse,
      );
    });
  });

  group('CallSession', () {
    CallSession session({DateTime? connectedAt}) => CallSession(
          callId: 'c1',
          phase: CallPhase.connected,
          isVideo: false,
          isOutgoing: true,
          peerName: 'A',
          connectedAt: connectedAt,
        );

    // The duration counts from the moment media connected, not from dialling:
    // a minute of ringing is not a minute of call.
    test('has no elapsed time before the call connects', () {
      expect(session().elapsed, Duration.zero);
    });

    test('counts from the connection, not the start', () {
      final CallSession live = session(
        connectedAt: DateTime.now().subtract(const Duration(seconds: 30)),
      );
      expect(live.elapsed.inSeconds, greaterThanOrEqualTo(29));
    });

    test('copyWith keeps the fields it was not asked to change', () {
      final CallSession muted = session().copyWith(muted: true);
      expect(muted.muted, isTrue);
      expect(muted.callId, 'c1');
      expect(muted.peerName, 'A');
      expect(muted.isOutgoing, isTrue);
    });
  });
}
