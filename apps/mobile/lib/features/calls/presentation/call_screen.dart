import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:flutter_webrtc/flutter_webrtc.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../media/presentation/media_widgets.dart';
import '../data/call_controller.dart';

/// The in-call screen (§18).
class CallScreen extends ConsumerStatefulWidget {
  const CallScreen({super.key});

  @override
  ConsumerState<CallScreen> createState() => _CallScreenState();
}

class _CallScreenState extends ConsumerState<CallScreen> {
  final RTCVideoRenderer _local = RTCVideoRenderer();
  final RTCVideoRenderer _remote = RTCVideoRenderer();

  StreamSubscription<MediaStream?>? _localSub;
  StreamSubscription<MediaStream?>? _remoteSub;
  bool _renderersReady = false;

  @override
  void initState() {
    super.initState();
    unawaited(_initRenderers());
  }

  Future<void> _initRenderers() async {
    await _local.initialize();
    await _remote.initialize();
    if (!mounted) {
      return;
    }
    setState(() => _renderersReady = true);

    final CallController controller = ref.read(callControllerProvider.notifier);
    _local.srcObject = controller.currentLocalStream;
    _remote.srcObject = controller.currentRemoteStream;

    _localSub = controller.localStream.listen((MediaStream? stream) {
      if (mounted) {
        setState(() => _local.srcObject = stream);
      }
    });
    _remoteSub = controller.remoteStream.listen((MediaStream? stream) {
      if (mounted) {
        setState(() => _remote.srcObject = stream);
      }
    });
  }

  @override
  void dispose() {
    unawaited(_localSub?.cancel());
    unawaited(_remoteSub?.cancel());
    _local.dispose();
    _remote.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final CallSession? session = ref.watch(callControllerProvider);

    // The call ended while this screen was open: close it rather than leaving
    // a dead screen the user has to dismiss.
    ref.listen<CallSession?>(callControllerProvider, (
      CallSession? previous,
      CallSession? next,
    ) {
      if (next?.phase == CallPhase.ended && mounted) {
        // The navigator is captured now, before the delay: `context` is not
        // guaranteed to still be mounted when the timer fires, and State
        // .mounted does not vouch for it either.
        final NavigatorState navigator = Navigator.of(context);
        Future<void>.delayed(SobhDuration.slow, () {
          if (navigator.canPop()) {
            navigator.pop();
          }
          ref.read(callControllerProvider.notifier).clear();
        });
      }
    });

    if (session == null) {
      return Scaffold(
        body: SobhEmptyState(icon: Icons.call_end, title: l10n.callsEnded),
      );
    }

    return Scaffold(
      backgroundColor: palette.chatBackground,
      body: Stack(
        fit: StackFit.expand,
        children: <Widget>[
          // Remote video fills the screen; audio-only shows the peer's avatar.
          if (session.isVideo && _renderersReady && _remote.srcObject != null)
            RTCVideoView(
              _remote,
              objectFit: RTCVideoViewObjectFit.RTCVideoViewObjectFitCover,
            )
          else
            _AudioBackdrop(session: session),

          if (session.isVideo && _renderersReady && _local.srcObject != null)
            PositionedDirectional(
              top: SobhSpacing.xxxl,
              end: SobhSpacing.lg,
              width: SobhSizes.avatarLarge,
              height: SobhSizes.avatarLarge * 1.5,
              child: ClipRRect(
                borderRadius: BorderRadius.circular(SobhRadius.md),
                child: RTCVideoView(_local, mirror: true),
              ),
            ),

          PositionedDirectional(
            top: SobhSpacing.xxl,
            start: SobhSpacing.lg,
            end: SobhSpacing.lg,
            child: _CallHeader(session: session),
          ),

          PositionedDirectional(
            bottom: SobhSpacing.xxl,
            start: SobhSpacing.lg,
            end: SobhSpacing.lg,
            child: _CallControls(session: session),
          ),
        ],
      ),
    );
  }
}

class _AudioBackdrop extends StatelessWidget {
  const _AudioBackdrop({required this.session});

  final CallSession session;

  @override
  Widget build(BuildContext context) => Center(
        child: SobhAvatar(
          name: session.peerName,
          radius: SobhSizes.avatarLarge / 2,
        ),
      );
}

class _CallHeader extends StatelessWidget {
  const _CallHeader({required this.session});

  final CallSession session;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    final String status = switch (session.phase) {
      CallPhase.dialling => l10n.callsRinging,
      CallPhase.ringing => l10n.callsIncoming,
      CallPhase.connecting => l10n.callsConnecting,
      CallPhase.reconnecting => l10n.callsReconnecting,
      CallPhase.ended => l10n.callsEnded,
      // Only a connected call has a duration worth showing.
      CallPhase.connected => formatDuration(session.elapsed),
    };

    return Column(
      children: <Widget>[
        Text(
          session.peerName,
          style: Theme.of(context).textTheme.headlineSmall,
        ),
        const SizedBox(height: SobhSpacing.xs),
        // While connected the duration ticks; in every other phase the label
        // is static, so the timer only runs when it says something.
        if (session.phase == CallPhase.connected)
          StreamBuilder<int>(
            stream: Stream<int>.periodic(
              const Duration(seconds: 1),
              (int tick) => tick,
            ),
            builder: (BuildContext context, AsyncSnapshot<int> snapshot) =>
                Text(
              formatDuration(session.elapsed),
              style: Theme.of(context).textTheme.titleMedium,
            ),
          )
        else
          Text(status, style: Theme.of(context).textTheme.titleMedium),
      ],
    );
  }
}

class _CallControls extends ConsumerWidget {
  const _CallControls({required this.session});

  final CallSession session;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final CallController controller = ref.read(callControllerProvider.notifier);

    // A ringing call offers only answer and decline: mute and camera controls
    // would act on a microphone that is not open yet.
    if (session.phase == CallPhase.ringing) {
      return Row(
        mainAxisAlignment: MainAxisAlignment.spaceEvenly,
        children: <Widget>[
          _CallButton(
            icon: Icons.call_end,
            label: l10n.callsDecline,
            background: palette.error,
            onPressed: controller.decline,
          ),
          _CallButton(
            icon: Icons.call,
            label: l10n.callsAnswer,
            background: palette.success,
            onPressed: controller.answer,
          ),
        ],
      );
    }

    return Column(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        Row(
          mainAxisAlignment: MainAxisAlignment.spaceEvenly,
          children: <Widget>[
            _CallButton(
              icon: session.muted ? Icons.mic_off : Icons.mic,
              label: session.muted ? l10n.callsUnmute : l10n.callsMute,
              background: palette.surfaceVariant,
              onPressed: controller.toggleMute,
            ),
            _CallButton(
              icon: session.speakerOn ? Icons.volume_up : Icons.hearing,
              label: l10n.callsSpeaker,
              background: palette.surfaceVariant,
              onPressed: controller.toggleSpeaker,
            ),
            if (session.isVideo) ...<Widget>[
              _CallButton(
                icon: session.cameraOn ? Icons.videocam : Icons.videocam_off,
                label:
                    session.cameraOn ? l10n.callsCameraOff : l10n.callsCameraOn,
                background: palette.surfaceVariant,
                onPressed: controller.toggleCamera,
              ),
              _CallButton(
                icon: Icons.cameraswitch_outlined,
                label: l10n.callsSwitchCamera,
                background: palette.surfaceVariant,
                onPressed: controller.switchCamera,
              ),
            ],
          ],
        ),
        const SizedBox(height: SobhSpacing.xl),
        _CallButton(
          icon: Icons.call_end,
          label: l10n.callsEnd,
          background: palette.error,
          onPressed: controller.hangUp,
        ),
      ],
    );
  }
}

class _CallButton extends StatelessWidget {
  const _CallButton({
    required this.icon,
    required this.label,
    required this.background,
    required this.onPressed,
  });

  final IconData icon;
  final String label;
  final Color background;
  final Future<void> Function() onPressed;

  @override
  Widget build(BuildContext context) => Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          IconButton.filled(
            onPressed: onPressed,
            icon: Icon(icon),
            iconSize: SobhSizes.iconLarge,
            style: IconButton.styleFrom(
              backgroundColor: background,
              minimumSize:
                  const Size.square(SobhSizes.minTapTarget + SobhSpacing.md),
            ),
            tooltip: label,
          ),
          const SizedBox(height: SobhSpacing.xs),
          Text(label, style: Theme.of(context).textTheme.labelSmall),
        ],
      );
}
