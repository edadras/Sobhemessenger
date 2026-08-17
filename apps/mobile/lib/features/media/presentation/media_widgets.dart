import 'package:cached_network_image/cached_network_image.dart';
import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:just_audio/just_audio.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../data/media_repository.dart';

/// Renders one attachment inside a message bubble.
///
/// It resolves the object first, because how to render it — and whether it is
/// safe to render at all — depends on the kind and the scan result, neither of
/// which the message itself carries.
class AttachmentView extends ConsumerWidget {
  const AttachmentView({super.key, required this.mediaId, this.caption = ''});

  final String mediaId;
  final String caption;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AsyncValue<Media> media = ref.watch(mediaProvider(mediaId));

    return media.when(
      loading: () => const _AttachmentPlaceholder(),
      error: (Object error, StackTrace _) => const _AttachmentUnavailable(),
      data: (Media object) {
        if (!object.isReady) {
          // Still being scanned or transcoded. Showing a link to bytes that
          // may be quarantined would defeat the point of scanning them.
          return _AttachmentProcessing(media: object);
        }
        return switch (object.kind) {
          'image' => _ImageAttachment(media: object, caption: caption),
          'video' => _VideoAttachment(media: object, caption: caption),
          'voice' || 'audio' => VoiceAttachment(media: object),
          _ => _FileAttachment(media: object),
        };
      },
    );
  }
}

class _ImageAttachment extends ConsumerWidget {
  const _ImageAttachment({required this.media, required this.caption});

  final Media media;
  final String caption;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    // The medium variant is what a bubble needs; the original is fetched only
    // when the image is opened full-screen.
    final AsyncValue<String> url = ref.watch(
      mediaUrlProvider((mediaId: media.id, variant: 'medium')),
    );

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: <Widget>[
        ClipRRect(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          child: AspectRatio(
            aspectRatio: media.aspectRatio,
            child: url.when(
              loading: () => const _AttachmentPlaceholder(),
              error: (Object error, StackTrace _) =>
                  const _AttachmentUnavailable(),
              data: (String resolved) => GestureDetector(
                onTap: () => Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) => FullScreenImage(media: media),
                  ),
                ),
                child: CachedNetworkImage(
                  imageUrl: resolved,
                  fit: BoxFit.cover,
                  placeholder: (_, __) => const _AttachmentPlaceholder(),
                  errorWidget: (_, __, ___) => const _AttachmentUnavailable(),
                ),
              ),
            ),
          ),
        ),
        if (caption.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(top: SobhSpacing.xs),
            child: Text(caption, style: Theme.of(context).textTheme.bodyMedium),
          ),
      ],
    );
  }
}

/// A video shows its poster with a play badge; playback opens full screen.
class _VideoAttachment extends ConsumerWidget {
  const _VideoAttachment({required this.media, required this.caption});

  final Media media;
  final String caption;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<String> poster = ref.watch(
      mediaUrlProvider((mediaId: media.id, variant: 'poster')),
    );

    return Column(
      crossAxisAlignment: CrossAxisAlignment.start,
      children: <Widget>[
        ClipRRect(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          child: AspectRatio(
            aspectRatio: media.aspectRatio,
            child: Stack(
              fit: StackFit.expand,
              children: <Widget>[
                poster.when(
                  loading: () => const _AttachmentPlaceholder(),
                  error: (Object error, StackTrace _) =>
                      Container(color: palette.surfaceVariant),
                  data: (String resolved) => CachedNetworkImage(
                    imageUrl: resolved,
                    fit: BoxFit.cover,
                    placeholder: (_, __) => const _AttachmentPlaceholder(),
                    errorWidget: (_, __, ___) =>
                        Container(color: palette.surfaceVariant),
                  ),
                ),
                Center(
                  child: CircleAvatar(
                    backgroundColor: palette.surface,
                    child: Icon(Icons.play_arrow, color: palette.primary),
                  ),
                ),
                if (media.duration != null)
                  Positioned(
                    right: SobhSpacing.sm,
                    bottom: SobhSpacing.sm,
                    child: Container(
                      padding: const EdgeInsets.symmetric(
                        horizontal: SobhSpacing.sm,
                        vertical: SobhSpacing.xxs,
                      ),
                      decoration: BoxDecoration(
                        color: palette.surface,
                        borderRadius: BorderRadius.circular(SobhRadius.sm),
                      ),
                      child: Text(
                        formatDuration(media.duration!),
                        style: Theme.of(context).textTheme.labelSmall,
                      ),
                    ),
                  ),
              ],
            ),
          ),
        ),
        if (caption.isNotEmpty)
          Padding(
            padding: const EdgeInsets.only(top: SobhSpacing.xs),
            child: Text(caption, style: Theme.of(context).textTheme.bodyMedium),
          ),
        Padding(
          padding: const EdgeInsets.only(top: SobhSpacing.xxs),
          child: Text(
            l10n.mediaVideo,
            style: Theme.of(
              context,
            ).textTheme.labelSmall?.copyWith(color: palette.textSecondary),
          ),
        ),
      ],
    );
  }
}

/// A voice note: play control plus the waveform the server computed.
class VoiceAttachment extends ConsumerStatefulWidget {
  const VoiceAttachment({super.key, required this.media});

  final Media media;

  @override
  ConsumerState<VoiceAttachment> createState() => _VoiceAttachmentState();
}

class _VoiceAttachmentState extends ConsumerState<VoiceAttachment> {
  final AudioPlayer _player = AudioPlayer();
  bool _loaded = false;

  @override
  void dispose() {
    _player.dispose();
    super.dispose();
  }

  Future<void> _toggle() async {
    if (_player.playing) {
      await _player.pause();
      return;
    }
    if (!_loaded) {
      // The URL is signed and short-lived, so it is resolved when playback is
      // actually requested rather than when the bubble is built.
      final String url =
          await ref.read(mediaRepositoryProvider).downloadUrl(widget.media.id);
      await _player.setUrl(url);
      _loaded = true;
    }
    await _player.play();
  }

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        StreamBuilder<PlayerState>(
          stream: _player.playerStateStream,
          builder: (BuildContext context, AsyncSnapshot<PlayerState> snapshot) {
            final bool playing = snapshot.data?.playing ?? false;
            return IconButton(
              icon: Icon(playing ? Icons.pause : Icons.play_arrow),
              color: palette.primary,
              onPressed: _toggle,
            );
          },
        ),
        SizedBox(
          width: SobhSizes.avatarLarge,
          height: SobhSizes.iconLarge,
          child: StreamBuilder<Duration>(
            stream: _player.positionStream,
            builder: (BuildContext context, AsyncSnapshot<Duration> snapshot) {
              final Duration total = widget.media.duration ?? Duration.zero;
              final double progress = total.inMilliseconds == 0
                  ? 0
                  : (snapshot.data ?? Duration.zero).inMilliseconds /
                      total.inMilliseconds;
              return CustomPaint(
                painter: _WaveformPainter(
                  samples: widget.media.waveform,
                  progress: progress.clamp(0.0, 1.0),
                  played: palette.primary,
                  unplayed: palette.outline,
                ),
              );
            },
          ),
        ),
        const SizedBox(width: SobhSpacing.sm),
        if (widget.media.duration != null)
          Text(
            formatDuration(widget.media.duration!),
            style: Theme.of(
              context,
            ).textTheme.labelSmall?.copyWith(color: palette.textSecondary),
          ),
      ],
    );
  }
}

/// Draws the waveform the server extracted, filled up to the play position.
class _WaveformPainter extends CustomPainter {
  const _WaveformPainter({
    required this.samples,
    required this.progress,
    required this.played,
    required this.unplayed,
  });

  final List<int> samples;
  final double progress;
  final Color played;
  final Color unplayed;

  @override
  void paint(Canvas canvas, Size size) {
    if (samples.isEmpty) {
      return;
    }

    final double barWidth = size.width / samples.length;
    final int peak = samples.reduce((int a, int b) => a > b ? a : b);
    final Paint paint = Paint()..strokeWidth = barWidth * 0.6;

    for (int i = 0; i < samples.length; i++) {
      final double normalised = peak == 0 ? 0 : samples[i] / peak;
      final double height = (size.height * normalised).clamp(1.0, size.height);
      final double x = i * barWidth + barWidth / 2;

      paint.color = (i / samples.length) <= progress ? played : unplayed;
      canvas.drawLine(
        Offset(x, (size.height - height) / 2),
        Offset(x, (size.height + height) / 2),
        paint,
      );
    }
  }

  @override
  bool shouldRepaint(_WaveformPainter old) =>
      old.progress != progress || old.samples != samples;
}

class _FileAttachment extends ConsumerWidget {
  const _FileAttachment({required this.media});

  final Media media;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final SobhPalette palette = SobhTheme.of(context);

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        Icon(Icons.insert_drive_file_outlined, color: palette.primary),
        const SizedBox(width: SobhSpacing.sm),
        Flexible(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            mainAxisSize: MainAxisSize.min,
            children: <Widget>[
              Text(
                media.fileName.isEmpty ? media.mimeType : media.fileName,
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
              ),
              Text(
                formatBytes(media.sizeBytes),
                style: Theme.of(
                  context,
                ).textTheme.labelSmall?.copyWith(color: palette.textSecondary),
              ),
            ],
          ),
        ),
      ],
    );
  }
}

/// An image opened full screen, at its original resolution.
class FullScreenImage extends ConsumerWidget {
  const FullScreenImage({super.key, required this.media});

  final Media media;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AsyncValue<String> url = ref.watch(
      mediaUrlProvider((mediaId: media.id, variant: null)),
    );

    return Scaffold(
      appBar: AppBar(),
      body: Center(
        child: url.when(
          loading: () => const CircularProgressIndicator(),
          error: (Object error, StackTrace _) => const _AttachmentUnavailable(),
          data: (String resolved) => InteractiveViewer(
            maxScale: 5,
            child: CachedNetworkImage(
              imageUrl: resolved,
              fit: BoxFit.contain,
              placeholder: (_, __) => const CircularProgressIndicator(),
              errorWidget: (_, __, ___) => const _AttachmentUnavailable(),
            ),
          ),
        ),
      ),
    );
  }
}

class _AttachmentPlaceholder extends StatelessWidget {
  const _AttachmentPlaceholder();

  @override
  Widget build(BuildContext context) => Container(
        height: SobhSizes.avatarLarge,
        color: SobhTheme.of(context).surfaceVariant,
        alignment: Alignment.center,
        child: const SizedBox(
          width: SobhSizes.iconMedium,
          height: SobhSizes.iconMedium,
          child: CircularProgressIndicator(strokeWidth: 2),
        ),
      );
}

/// Shown while the object is still being scanned or transcoded.
class _AttachmentProcessing extends StatelessWidget {
  const _AttachmentProcessing({required this.media});

  final Media media;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    final String label = media.scanStatus == 'infected'
        ? l10n.mediaBlocked
        : l10n.mediaProcessing;

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        Icon(
          media.scanStatus == 'infected'
              ? Icons.gpp_bad_outlined
              : Icons.hourglass_empty,
          size: SobhSizes.iconSmall,
          color: media.scanStatus == 'infected'
              ? palette.error
              : palette.textSecondary,
        ),
        const SizedBox(width: SobhSpacing.xs),
        Text(
          label,
          style: Theme.of(
            context,
          ).textTheme.labelSmall?.copyWith(color: palette.textSecondary),
        ),
      ],
    );
  }
}

class _AttachmentUnavailable extends StatelessWidget {
  const _AttachmentUnavailable();

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        Icon(
          Icons.broken_image_outlined,
          size: SobhSizes.iconSmall,
          color: palette.textDisabled,
        ),
        const SizedBox(width: SobhSpacing.xs),
        Text(
          l10n.mediaUnavailable,
          style: Theme.of(
            context,
          ).textTheme.labelSmall?.copyWith(color: palette.textSecondary),
        ),
      ],
    );
  }
}

/// mm:ss, or h:mm:ss once a recording passes an hour.
String formatDuration(Duration duration) {
  final String minutes =
      duration.inMinutes.remainder(60).toString().padLeft(2, '0');
  final String seconds =
      duration.inSeconds.remainder(60).toString().padLeft(2, '0');
  return duration.inHours > 0
      ? '${duration.inHours}:$minutes:$seconds'
      : '$minutes:$seconds';
}

/// Binary units, because that is what a file manager shows.
String formatBytes(int bytes) {
  const List<String> units = <String>['B', 'KB', 'MB', 'GB'];
  double value = bytes.toDouble();
  int unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  return '${value.toStringAsFixed(unit == 0 ? 0 : 1)} ${units[unit]}';
}
