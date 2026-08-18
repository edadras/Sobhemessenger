import 'dart:async';
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
import '../../stickers/presentation/sticker_picker.dart';
import '../data/chat_repository.dart';
import '../data/inbound_sync.dart';
import '../data/organise_repository.dart';
import 'location_sheet.dart';
import 'message_actions.dart';
import 'message_content.dart';
import 'scheduled_messages_screen.dart';

/// What the chat's overflow menu offers beyond the actions with their own
/// buttons.
enum _ChatAction { scheduled, mute, unmute, archive }

/// What the paperclip offers. One menu rather than three buttons, because
/// three would crowd the composer on a narrow screen.
enum _Attachment { media, location, contact }

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

  /// True while a backfill is in flight, so scrolling does not start a second.
  bool _loadingHistory = false;

  /// False once the server has no more history to give, so an empty chat does
  /// not ask for the same nothing on every scroll.
  bool _moreHistory = true;

  /// How far the read cursor has been pushed, so scrolling does not send the
  /// same acknowledgement repeatedly.
  int _markedReadUpTo = 0;

  bool _typing = false;
  Timer? _typingStopTimer;

  /// How long a pause counts as having stopped typing.
  static const Duration _typingIdle = Duration(seconds: 3);

  @override
  void initState() {
    super.initState();
    // The local database is what the screen draws, so history is fetched into
    // it rather than into the widget. An offline open shows what is cached and
    // silently fills in when the network returns.
    unawaited(_loadHistory());
    _scrollController.addListener(_onScroll);
    _composer.addListener(_onComposerChanged);
  }

  /// Tells the other side the user is typing, and stops saying so once they
  /// pause.
  ///
  /// Rate-limited to one "started" per interval rather than one request per
  /// keystroke, which would be a request per character typed. The stop is on a
  /// timer because there is no keystroke to hang it off — someone who stops
  /// typing sends nothing at all.
  void _onComposerChanged() {
    _typingStopTimer?.cancel();

    if (_composer.text.trim().isEmpty) {
      _setTyping(false);
      return;
    }

    _setTyping(true);
    _typingStopTimer = Timer(_typingIdle, () => _setTyping(false));
  }

  void _setTyping(bool typing) {
    if (_typing == typing) {
      return;
    }
    _typing = typing;
    unawaited(
      ref.read(chatRepositoryProvider).setTyping(widget.chatId, typing: typing),
    );
  }

  /// Marks everything on screen as read.
  ///
  /// Without this the unread badge never clears — it is server-side state, and
  /// no amount of looking at the conversation changes it on its own. Called on
  /// open and whenever new messages arrive while the screen is up, because a
  /// message read as it lands is still read.
  Future<void> _markRead(List<MessageRow> messages) async {
    final int highest = messages
        .map((MessageRow row) => row.seq)
        .whereType<int>()
        .fold<int>(0, (int a, int b) => a > b ? a : b);
    if (highest <= _markedReadUpTo) {
      return;
    }
    _markedReadUpTo = highest;

    try {
      await ref.read(chatRepositoryProvider).markRead(widget.chatId, highest);
    } on ApiException {
      // Offline. The badge clears on the next open once the request lands, and
      // an error over a conversation the user is reading would be noise.
      _markedReadUpTo = 0;
    }
  }

  void _onScroll() {
    // The list is reversed, so the far edge is the oldest message.
    if (_scrollController.position.extentAfter < 400) {
      unawaited(_loadHistory());
    }
  }

  Future<void> _loadHistory() async {
    if (_loadingHistory || !_moreHistory) {
      return;
    }
    _loadingHistory = true;

    try {
      final List<MessageRow> known = await ref
          .read(chatRepositoryProvider)
          .watchMessages(widget.chatId)
          .first;
      final int? oldest =
          known.map((MessageRow row) => row.seq).whereType<int>().fold<int?>(
                null,
                (int? lowest, int seq) =>
                    lowest == null || seq < lowest ? seq : lowest,
              );

      final int fetched = await ref
          .read(inboundSyncProvider)
          .loadHistory(widget.chatId, beforeSeq: oldest);
      if (fetched == 0) {
        _moreHistory = false;
      }
    } on ApiException {
      // Offline is the ordinary case here, and the cached history is already
      // on screen; the next scroll or reconnection tries again.
    } finally {
      _loadingHistory = false;
    }
  }

  @override
  void dispose() {
    _typingStopTimer?.cancel();
    // Leaving the screen mid-sentence must clear the indicator, or the other
    // person is told someone is typing who has closed the conversation.
    if (_typing) {
      unawaited(
        ref
            .read(chatRepositoryProvider)
            .setTyping(widget.chatId, typing: false),
      );
    }
    _composer.removeListener(_onComposerChanged);
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

  /// Sends a sticker, which is an ordinary message carrying one media id.
  Future<void> _sendSticker() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final String? mediaId = await showStickerPicker(context);
    if (mediaId == null) {
      return;
    }

    try {
      await ref.read(chatRepositoryProvider).sendMedia(
        chatId: widget.chatId,
        type: 'sticker',
        mediaIds: <String>[mediaId],
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

  Future<void> _onAction(_ChatAction action) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final OrganiseRepository organise = ref.read(organiseRepositoryProvider);

    try {
      switch (action) {
        case _ChatAction.scheduled:
          await Navigator.of(context).push(
            MaterialPageRoute<void>(
              builder: (_) => ScheduledMessagesScreen(chatId: widget.chatId),
            ),
          );
        case _ChatAction.mute:
          // A year is how a client says "until I say otherwise" without a
          // second flag that could fall out of step with the time.
          await organise.setMuted(
            widget.chatId,
            DateTime.now().add(const Duration(days: 365)),
          );
          _tell(l10n.chatMuted);
        case _ChatAction.unmute:
          await organise.setMuted(widget.chatId, null);
          _tell(l10n.chatUnmuted);
        case _ChatAction.archive:
          await organise.setFlags(widget.chatId, archived: true);
          _tell(l10n.chatArchived);
      }
    } on ApiException catch (error) {
      _tell(error.isOffline ? l10n.errorNetwork : error.message);
    }
  }

  void _tell(String message) {
    if (mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(message)));
    }
  }

  /// Shares a point on the map, once or as a live position.
  Future<void> _sendLocation() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SharedLocation? location = await showLocationSheet(context);
    if (location == null || !mounted) {
      return;
    }

    try {
      await ref.read(chatRepositoryProvider).sendTyped(
            chatId: widget.chatId,
            type: 'location',
            payload: location.toPayload(),
          );
    } on ApiException catch (error) {
      _tell(error.isOffline ? l10n.errorNetwork : error.message);
    }
  }

  /// Shares one contact's name and number — not their whole address-book entry.
  Future<void> _sendContact() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SharedContact? contact = await pickContactToShare(context);
    if (contact == null || !mounted) {
      return;
    }

    try {
      await ref.read(chatRepositoryProvider).sendTyped(
            chatId: widget.chatId,
            type: 'contact',
            payload: contact.toPayload(),
          );
    } on ApiException catch (error) {
      _tell(error.isOffline ? l10n.errorNetwork : error.message);
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
    // The chat's type decides which actions a message offers — commenting
    // belongs to a channel post and nothing else. It comes from the local row
    // rather than a fetch, so a long press never waits on the network.
    final String chatType = ref
            .watch(chatListProvider)
            .valueOrNull
            ?.where((ChatRow row) => row.id == widget.chatId)
            .firstOrNull
            ?.type ??
        '';

    // Reading is acknowledged whenever the conversation changes underneath the
    // screen, which covers both opening it and a message arriving while it is
    // open. Deferred past this frame because it writes to the database the
    // build is reading from.
    messages.whenData((List<MessageRow> rows) {
      if (rows.isNotEmpty) {
        WidgetsBinding.instance.addPostFrameCallback((_) {
          if (mounted) {
            unawaited(_markRead(rows));
          }
        });
      }
    });

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
          PopupMenuButton<_ChatAction>(
            onSelected: _onAction,
            itemBuilder: (BuildContext context) =>
                <PopupMenuEntry<_ChatAction>>[
              PopupMenuItem<_ChatAction>(
                value: _ChatAction.scheduled,
                child: ListTile(
                  leading: const Icon(Icons.schedule_outlined),
                  title: Text(l10n.chatScheduledTitle),
                ),
              ),
              PopupMenuItem<_ChatAction>(
                value: _ChatAction.mute,
                child: ListTile(
                  leading: const Icon(Icons.notifications_off_outlined),
                  title: Text(l10n.chatMute),
                ),
              ),
              PopupMenuItem<_ChatAction>(
                value: _ChatAction.unmute,
                child: ListTile(
                  leading: const Icon(Icons.notifications_active_outlined),
                  title: Text(l10n.chatUnmute),
                ),
              ),
              PopupMenuItem<_ChatAction>(
                value: _ChatAction.archive,
                child: ListTile(
                  leading: const Icon(Icons.archive_outlined),
                  title: Text(l10n.chatArchive),
                ),
              ),
            ],
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
                  return GestureDetector(
                    onLongPress: () => showMessageActions(
                      context,
                      ref,
                      message,
                      chatType: chatType,
                    ),
                    child: _MessageBubble(
                      message: message,
                      isOutgoing: message.senderId == null ||
                          message.senderId == currentUserId,
                    ),
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
            onSticker: _sendSticker,
            onLocation: _sendLocation,
            onContact: _sendContact,
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
              // A location and a contact are structured, so they are drawn
              // rather than printed. A deleted message shows its tombstone
              // instead: whatever it carried is gone.
              if (!isDeleted &&
                  message.type == 'location' &&
                  message.payloadJson != null)
                LocationBubble(payloadJson: message.payloadJson!),
              if (!isDeleted &&
                  message.type == 'contact' &&
                  message.payloadJson != null)
                ContactBubble(payloadJson: message.payloadJson!),

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

              // Buttons a bot attached. They go under the text, and disappear
              // with the message if it is deleted.
              if (!isDeleted && message.replyMarkupJson != null)
                InlineKeyboardView(
                  message: message,
                  markupJson: message.replyMarkupJson!,
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
    required this.onSticker,
    required this.onLocation,
    required this.onContact,
    required this.onVoice,
    required this.hint,
  });

  final TextEditingController controller;
  final Future<void> Function() onSend;
  final Future<void> Function() onAttach;
  final Future<void> Function() onSticker;
  final Future<void> Function() onLocation;
  final Future<void> Function() onContact;
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
                  PopupMenuButton<_Attachment>(
                    icon: const Icon(Icons.attach_file),
                    tooltip: l10n.mediaAttach,
                    onSelected: (_Attachment choice) {
                      switch (choice) {
                        case _Attachment.media:
                          unawaited(widget.onAttach());
                        case _Attachment.location:
                          unawaited(widget.onLocation());
                        case _Attachment.contact:
                          unawaited(widget.onContact());
                      }
                    },
                    itemBuilder: (BuildContext context) =>
                        <PopupMenuEntry<_Attachment>>[
                      PopupMenuItem<_Attachment>(
                        value: _Attachment.media,
                        child: ListTile(
                          leading: const Icon(Icons.image_outlined),
                          title: Text(l10n.mediaAttach),
                        ),
                      ),
                      PopupMenuItem<_Attachment>(
                        value: _Attachment.location,
                        child: ListTile(
                          leading: const Icon(Icons.location_on_outlined),
                          title: Text(l10n.locationShare),
                        ),
                      ),
                      PopupMenuItem<_Attachment>(
                        value: _Attachment.contact,
                        child: ListTile(
                          leading: const Icon(Icons.person_outline),
                          title: Text(l10n.contactShare),
                        ),
                      ),
                    ],
                  ),
                  IconButton(
                    onPressed: widget.onSticker,
                    icon: const Icon(Icons.emoji_emotions_outlined),
                    tooltip: l10n.stickersTitle,
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
