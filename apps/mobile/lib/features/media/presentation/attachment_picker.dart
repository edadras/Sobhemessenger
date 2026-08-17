import 'dart:io';

import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:image_picker/image_picker.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../data/media_repository.dart';

/// A file the user picked, with the kind and MIME type the server needs.
class PickedAttachment {
  const PickedAttachment({
    required this.file,
    required this.kind,
    required this.mimeType,
  });

  final File file;
  final MediaKind kind;
  final String mimeType;

  /// The message type that carries this attachment.
  String get messageType => switch (kind) {
        MediaKind.image => 'photo',
        MediaKind.video => 'video',
        MediaKind.voice => 'voice',
        MediaKind.audio => 'audio',
        _ => 'document',
      };
}

/// Picks an attachment from the camera, the gallery or the file system.
///
/// The MIME type is declared here from the extension, but the server sniffs
/// the magic bytes and rejects a mismatch rather than trusting this — a
/// declared type is a hint, never an authorisation (§13).
class AttachmentPicker {
  const AttachmentPicker();

  Future<PickedAttachment?> pickImage({required ImageSource source}) async {
    final XFile? picked = await ImagePicker().pickImage(
      source: source,
      // Full-resolution originals are what the server's variant pipeline
      // wants; it produces the thumbnails, so the client must not pre-shrink.
      imageQuality: 100,
    );
    if (picked == null) {
      return null;
    }
    return PickedAttachment(
      file: File(picked.path),
      kind: MediaKind.image,
      mimeType: mimeTypeFor(picked.path, fallback: 'image/jpeg'),
    );
  }

  Future<PickedAttachment?> pickVideo({required ImageSource source}) async {
    final XFile? picked = await ImagePicker().pickVideo(source: source);
    if (picked == null) {
      return null;
    }
    return PickedAttachment(
      file: File(picked.path),
      kind: MediaKind.video,
      mimeType: mimeTypeFor(picked.path, fallback: 'video/mp4'),
    );
  }

  Future<PickedAttachment?> pickFile() async {
    final FilePickerResult? result = await FilePicker.platform.pickFiles();
    final String? path = result?.files.single.path;
    if (path == null) {
      return null;
    }
    return PickedAttachment(
      file: File(path),
      kind: MediaKind.file,
      mimeType: mimeTypeFor(path, fallback: 'application/octet-stream'),
    );
  }

  /// Maps an extension to a MIME type.
  ///
  /// Only the types the server accepts are listed; anything else is declared
  /// as a generic file, which is what it will be validated as.
  static String mimeTypeFor(String path, {required String fallback}) {
    final int dot = path.lastIndexOf('.');
    if (dot < 0) {
      return fallback;
    }
    return switch (path.substring(dot + 1).toLowerCase()) {
      'jpg' || 'jpeg' => 'image/jpeg',
      'png' => 'image/png',
      'gif' => 'image/gif',
      'webp' => 'image/webp',
      'heic' => 'image/heic',
      'mp4' => 'video/mp4',
      'mov' => 'video/quicktime',
      'webm' => 'video/webm',
      'mp3' => 'audio/mpeg',
      'ogg' || 'opus' => 'audio/ogg',
      'm4a' => 'audio/mp4',
      'wav' => 'audio/wav',
      'pdf' => 'application/pdf',
      _ => fallback,
    };
  }
}

/// The sheet offering the ways to attach something.
Future<PickedAttachment?> showAttachmentSheet(BuildContext context) {
  final AppLocalizations l10n = AppLocalizations.of(context);
  const AttachmentPicker picker = AttachmentPicker();

  return showModalBottomSheet<PickedAttachment?>(
    context: context,
    builder: (BuildContext sheetContext) => SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          ListTile(
            leading: const Icon(Icons.photo_camera_outlined),
            title: Text(l10n.mediaCamera),
            onTap: () async {
              final PickedAttachment? picked = await picker.pickImage(
                source: ImageSource.camera,
              );
              if (sheetContext.mounted) {
                Navigator.of(sheetContext).pop(picked);
              }
            },
          ),
          ListTile(
            leading: const Icon(Icons.photo_library_outlined),
            title: Text(l10n.mediaGallery),
            onTap: () async {
              final PickedAttachment? picked = await picker.pickImage(
                source: ImageSource.gallery,
              );
              if (sheetContext.mounted) {
                Navigator.of(sheetContext).pop(picked);
              }
            },
          ),
          ListTile(
            leading: const Icon(Icons.videocam_outlined),
            title: Text(l10n.mediaVideo),
            onTap: () async {
              final PickedAttachment? picked = await picker.pickVideo(
                source: ImageSource.gallery,
              );
              if (sheetContext.mounted) {
                Navigator.of(sheetContext).pop(picked);
              }
            },
          ),
          ListTile(
            leading: const Icon(Icons.attach_file),
            title: Text(l10n.mediaFile),
            onTap: () async {
              final PickedAttachment? picked = await picker.pickFile();
              if (sheetContext.mounted) {
                Navigator.of(sheetContext).pop(picked);
              }
            },
          ),
        ],
      ),
    ),
  );
}
