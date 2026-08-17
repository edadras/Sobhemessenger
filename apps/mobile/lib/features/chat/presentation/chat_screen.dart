import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';
import 'package:path_provider/path_provider.dart';
import 'package:record/record.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../auth/session_controller.dart';
import '../../calls/data/call_controller.dart';
import '../../calls/presentation/call_screen.dart';
import '../../media/data/media_repository.dart';
import '../../media/presentation/attachment_picker.dart';
import '../../media/presentation/media_widgets.dart';
import '../../polls/presentation/poll_widgets.dart';
import '../data/chat_repository.dart';

/// A single conversation (§49).
class ChatScreen extends ConsumerStatefulWidget {
  const ChatScreen({required this.chatId, super.key});

  final String chatId;

  @override
  ConsumerState<ChatScreen> createState() => _ChatScreenState();
}

class _ChatScreenState extends ConsumerState<ChatScreen> {
  final TextEditingController _composer = TextEditingController();
  final ScrollController _scrollController = ScrollController();

  /// Non-null while an attachment is uploading, so the composer can show how
  /// far it has got instead of a spinner of unknown length.
  UploadProgress? _uploading;

  @override
  void dispose() {
    _composer.dispose();
    _scrollController.dispose();
    super.dispose();
  }

  Future<void> _send() async {
    final String text = _composer.text.trim();
    if (text.isEmpty) {
      return;
    }
    // Clear immediately: the message is already stored locally, so there is
    // nothing to roll back if the network is down.
    _composer.clear();
    await ref
        .read(chatRepositoryProvider)
        .sendText(chatId: widget.chatId, content: text);
  }

  /// Picks a file, uploads it, then sends a message referencing it.
  ///
  /// The upload happens before the message is queued, so what reaches the
  /// offline outbox is a small reference the retry loop can replay cheaply —
  /// a retried send never re-uploads the bytes (§13).
  Future<void> _attach() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final PickedAttachment? picked = await showAttachmentSheet(context);
    if (picked == null || !mounted) {
      return;
    }

    setState(() => _uploading = const UploadProgress(sent: 0, total: 1));
    try {
      final Media media = await ref.read(mediaRepositoryProvider).upload(
            file: picked.file,
            kind: picked.kind,
            mimeType: picked.mimeType,
            onProgress: (UploadProgress progress) {
              if (mounted) {
                setState(() => _uploading = progress);
              }
            },
          );

      await ref.read(chatRepositoryProvider).sendMedia(
            chatId: widget.chatId,
            type: picked.messageType,
            mediaIds: <String>[media.id],
            content: _composer.text.trim(),
          );
      _composer.clear();
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      if (mounted) {
        setState(() => _uploading = null);
      }
    }
  }

  /// Sends a recorded voice note.
  Future<void> _sendVoice(File recording, Duration length) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _uploading = const UploadProgress(sent: 0, total: 1));

    try {
      final Media media = await ref.read(mediaRepositoryProvider).upload(
            file: recording,
            kind: MediaKind.voice,
            // Opus in an Ogg container: the server normalises voice to Opus
            // and computes the waveform from it.
            mimeType: 'audio/ogg',
            onProgress: (UploadProgress progress) {
              if (mounted) {
                setState(() => _uploading = progress);
              }
            },
          );

      await ref.read(chatRepositoryProvider).sendMedia(
        chatId: widget.chatId,
        type: 'voice',
        mediaIds: <String>[media.id],
      );
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    } finally {
      if (mounted) {
        setState(() => _uploading = null);
      }
    }
  }

  /// Places a call and opens the in-call screen.
  Future<void> _startCall({required bool video}) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final NavigatorState navigator = Navigator.of(context);

    try {
      await ref.read(callControllerProvider.notifier).place(
            chatId: widget.chatId,
            // The chat's own title is the best name the client holds without
            // a second round trip for the peer's profile.
            peerName: l10n.callsStart,
            video: video,
          );
      await navigator.push(
        MaterialPageRoute<void>(builder: (_) => const CallScreen()),
      );
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<List<MessageRow>> messages = ref.watch(
      chatMessagesProvider(widget.chatId),
    );
    final String? currentUserId = ref.watch(sessionControllerProvider).userId;

    return Scaffold(
      backgroundColor: palette.chatBackground,
      appBar: AppBar(
        title: Text(l10n.navChats),
        actions: <Widget>[
          IconButton(
            icon: const Icon(Icons.call_outlined),
            tooltip: l10n.callsAudio,
            onPressed: () => _startCall(video: false),
          ),
          IconButton(
            icon: const Icon(Icons.videocam_outlined),
            tooltip: l10n.callsVideo,
            onPressed: () => _startCall(video: true),
          ),
          IconButton(
            icon: const Icon(Icons.poll_outlined),
            tooltip: l10n.pollsCreate,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(
                builder: (_) => CreatePollScreen(chatId: widget.chatId),
              ),
            ),
          ),
          IconButton(
            icon: const Icon(Icons.info_outline),
            tooltip: l10n.groupsInfoTitle,
            onPressed: () => context.push('/chats/${widget.chatId}/info'),
          ),
        ],
      ),
      body: Column(
        children: <Widget>[
          Expanded(
            child: messages.when(
              loading: () => const Center(child: CircularProgressIndicator()),
              error: (Object error, StackTrace stack) =>
                  Center(child: Text(l10n.errorGeneric)),
              data: (List<MessageRow> rows) => ListView.builder(
                controller: _scrollController,
                reverse: true,
                padding: const EdgeInsets.symmetric(
                  horizontal: SobhSpacing.md,
                  vertical: SobhSpacing.sm,
                ),
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) {
                  final MessageRow message = rows[rows.length - 1 - index];
                  return _MessageBubble(
                    message: message,
                    isOutgoing: message.senderId == null ||
                        message.senderId == currentUserId,
                  );
                },
              ),
            ),
          ),
          if (_uploading != null) _UploadBar(progress: _uploading!),
          _Composer(
            controller: _composer,
            onSend: _send,
            onAttach: _attach,
            onVoice: _sendVoice,
            hint: l10n.chatMessageHint,
          ),
        ],
      ),
    );
  }
}

class _UploadBar extends StatelessWidget {
  const _UploadBar({required this.progress});

  final UploadProgress progress;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Container(
      color: palette.surfaceVariant,
      padding: const EdgeInsets.symmetric(
        horizontal: SobhSpacing.lg,
        vertical: SobhSpacing.sm,
      ),
      child: Row(
        children: <Widget>[
          Expanded(
            child: LinearProgressIndicator(
              value: progress.fraction == 0 ? null : progress.fraction,
            ),
          ),
          const SizedBox(width: SobhSpacing.md),
          Text(
            l10n.mediaUploading,
            style: Theme.of(context).textTheme.labelSmall,
          ),
        ],
      ),
    );
  }
}

class _MessageBubble extends StatelessWidget {
  const _MessageBubble({required this.message, required this.isOutgoing});

  final MessageRow message;
  final bool isOutgoing;

  /// The attachments stored alongside the message, as media ids with captions.
  List<({String mediaId, String caption})> get _attachments {
    if (message.attachmentsJson == null) {
      return const <({String mediaId, String caption})>[];
    }
    final dynamic decoded = jsonDecode(message.attachmentsJson!);
    if (decoded is! List<dynamic>) {
      return const <({String mediaId, String caption})>[];
    }
    return <({String mediaId, String caption})>[
      for (final dynamic entry in decoded)
        if (entry is Map<String, dynamic> && entry['media_id'] is String)
          (
            mediaId: entry['media_id'] as String,
            caption: entry['caption'] as String? ?? '',
          ),
    ];
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;
    final bool isDeleted = message.deletedAt != null;
    final List<({String mediaId, String caption})> attachments =
        isDeleted ? const <({String mediaId, String caption})>[] : _attachments;

    return Align(
      alignment: isOutgoing
          ? AlignmentDirectional.centerEnd
          : AlignmentDirectional.centerStart,
      child: ConstrainedBox(
        constraints: BoxConstraints(
          maxWidth: MediaQuery.sizeOf(context).width *
              SobhSizes.maxBubbleWidthFraction,
        ),
        child: Container(
          margin: const EdgeInsets.symmetric(vertical: SobhSpacing.xs),
          padding: const EdgeInsets.symmetric(
            horizontal: SobhSpacing.md,
            vertical: SobhSpacing.sm,
          ),
          decoration: BoxDecoration(
            color: isOutgoing ? palette.bubbleOutgoing : palette.bubbleIncoming,
            borderRadius: BorderRadius.circular(SobhRadius.lg),
            border: Border.all(color: palette.outline),
          ),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: <Widget>[
              for (final ({String mediaId, String caption}) attachment
                  in attachments)
                Padding(
                  padding: const EdgeInsets.only(bottom: SobhSpacing.xs),
                  child: AttachmentView(
                    mediaId: attachment.mediaId,
                    caption: attachment.caption,
                  ),
                ),
              if (isDeleted || message.content.isNotEmpty)
                Text(
                  isDeleted ? l10n.chatMessageDeleted : message.content,
                  style: text.bodyMedium?.copyWith(
                    color: isOutgoing
                        ? palette.bubbleOutgoingText
                        : palette.bubbleIncomingText,
                    fontStyle: isDeleted ? FontStyle.italic : FontStyle.normal,
                  ),
                ),
              const SizedBox(height: SobhSpacing.xxs),
              Row(
                mainAxisSize: MainAxisSize.min,
                children: <Widget>[
                  if (message.editedAt != null)
                    Text(l10n.chatMessageEdited, style: text.labelSmall),
                  const SizedBox(width: SobhSpacing.xs),
                  if (isOutgoing) _StatusIcon(status: message.status),
                ],
              ),
            ],
          ),
        ),
      ),
    );
  }
}

/// The delivery ticks. `pending` and `failed` only ever appear on this device —
/// they describe the outbox, not server state (§7).
class _StatusIcon extends StatelessWidget {
  const _StatusIcon({required this.status});

  final MessageStatus status;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    final (IconData icon, Color color) = switch (status) {
      MessageStatus.pending => (Icons.schedule, palette.textDisabled),
      MessageStatus.sending => (Icons.schedule, palette.textDisabled),
      MessageStatus.sent => (Icons.check, palette.textSecondary),
      MessageStatus.delivered => (Icons.done_all, palette.textSecondary),
      MessageStatus.read => (Icons.done_all, palette.info),
      MessageStatus.failed => (Icons.error_outline, palette.error),
    };

    return Icon(icon, size: SobhSizes.iconSmall, color: color);
  }
}

class _Composer extends StatefulWidget {
  const _Composer({
    required this.controller,
    required this.onSend,
    required this.onAttach,
    required this.onVoice,
    required this.hint,
  });

  final TextEditingController controller;
  final Future<void> Function() onSend;
  final Future<void> Function() onAttach;
  final Future<void> Function(File recording, Duration length) onVoice;
  final String hint;

  @override
  State<_Composer> createState() => _ComposerState();
}

class _ComposerState extends State<_Composer> {
  final AudioRecorder _recorder = AudioRecorder();
  DateTime? _recordingStartedAt;
  String? _recordingPath;

  @override
  void initState() {
    super.initState();
    widget.controller.addListener(_onTextChanged);
  }

  @override
  void dispose() {
    widget.controller.removeListener(_onTextChanged);
    _recorder.dispose();
    super.dispose();
  }

  void _onTextChanged() => setState(() {});

  Future<void> _startRecording() async {
    if (!await _recorder.hasPermission()) {
      return;
    }
    final Directory directory = await getTemporaryDirectory();
    final String path =
        '${directory.path}/voice-${DateTime.now().millisecondsSinceEpoch}.ogg';

    await _recorder.start(
      // Opus is what the server normalises voice to, so recording in it
      // avoids a transcode and keeps the note small on a slow connection.
      const RecordConfig(encoder: AudioEncoder.opus, numChannels: 1),
      path: path,
    );
    setState(() {
      _recordingStartedAt = DateTime.now();
      _recordingPath = path;
    });
  }

  Future<void> _stopRecording({required bool send}) async {
    final DateTime? startedAt = _recordingStartedAt;
    final String? path = await _recorder.stop();
    setState(() {
      _recordingStartedAt = null;
      _recordingPath = null;
    });

    if (!send || path == null || startedAt == null) {
      if (path != null) {
        // A cancelled recording is deleted rather than left in the cache.
        await File(path).delete().catchError((Object _) => File(path));
      }
      return;
    }

    final Duration length = DateTime.now().difference(startedAt);
    // Anything under a second is a mis-tap, not a message.
    if (length < const Duration(seconds: 1)) {
      await File(path).delete().catchError((Object _) => File(path));
      return;
    }
    await widget.onVoice(File(path), length);
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final bool recording = _recordingPath != null;
    final bool hasText = widget.controller.text.trim().isNotEmpty;

    return SafeArea(
      child: Container(
        color: palette.surface,
        padding: const EdgeInsets.all(SobhSpacing.sm),
        child: recording
            ? _RecordingBar(
                startedAt: _recordingStartedAt!,
                onCancel: () => _stopRecording(send: false),
                onSend: () => _stopRecording(send: true),
              )
            : Row(
                children: <Widget>[
                  IconButton(
                    onPressed: widget.onAttach,
                    icon: const Icon(Icons.attach_file),
                    tooltip: l10n.mediaAttach,
                  ),
                  Expanded(
                    child: TextField(
                      controller: widget.controller,
                      minLines: 1,
                      maxLines: 5,
                      textInputAction: TextInputAction.newline,
                      decoration: InputDecoration(hintText: widget.hint),
                    ),
                  ),
                  const SizedBox(width: SobhSpacing.sm),
                  // The send button becomes a microphone when there is nothing
                  // to send, which is the gesture people already know.
                  if (hasText)
                    IconButton.filled(
                      onPressed: widget.onSend,
                      icon: const Icon(Icons.send),
                      tooltip: widget.hint,
                    )
                  else
                    IconButton.filled(
                      onPressed: _startRecording,
                      icon: const Icon(Icons.mic),
                      tooltip: l10n.chatHoldToRecord,
                    ),
                ],
              ),
      ),
    );
  }
}

/// Shown while a voice note is being recorded.
class _RecordingBar extends StatefulWidget {
  const _RecordingBar({
    required this.startedAt,
    required this.onCancel,
    required this.onSend,
  });

  final DateTime startedAt;
  final VoidCallback onCancel;
  final VoidCallback onSend;

  @override
  State<_RecordingBar> createState() => _RecordingBarState();
}

class _RecordingBarState extends State<_RecordingBar> {
  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return Row(
      children: <Widget>[
        IconButton(
          onPressed: widget.onCancel,
          icon: Icon(Icons.delete_outline, color: palette.error),
          tooltip: MaterialLocalizations.of(context).cancelButtonLabel,
        ),
        Icon(
          Icons.fiber_manual_record,
          color: palette.error,
          size: SobhSizes.iconSmall,
        ),
        const SizedBox(width: SobhSpacing.sm),
        // A one-second tick is enough for a duration readout and avoids
        // rebuilding the composer on every frame.
        StreamBuilder<int>(
          stream:
              Stream<int>.periodic(const Duration(seconds: 1), (int i) => i),
          builder: (BuildContext context, AsyncSnapshot<int> snapshot) => Text(
            formatDuration(DateTime.now().difference(widget.startedAt)),
            style: Theme.of(context).textTheme.bodyMedium,
          ),
        ),
        const Spacer(),
        IconButton.filled(
          onPressed: widget.onSend,
          icon: const Icon(Icons.send),
          tooltip: MaterialLocalizations.of(context).okButtonLabel,
        ),
      ],
    );
  }
}
