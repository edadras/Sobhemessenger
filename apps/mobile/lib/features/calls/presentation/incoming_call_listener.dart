import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';
import '../data/call_controller.dart';
import 'call_screen.dart';

/// Watches for incoming calls anywhere in the app and opens the call screen.
///
/// It wraps the whole navigator rather than living on one screen, because a
/// call can arrive while the user is reading the news — the only screen that
/// must never miss it is whichever one is on top.
class IncomingCallListener extends ConsumerStatefulWidget {
  const IncomingCallListener({
    super.key,
    required this.navigatorKey,
    required this.child,
  });

  final GlobalKey<NavigatorState> navigatorKey;
  final Widget child;

  @override
  ConsumerState<IncomingCallListener> createState() =>
      _IncomingCallListenerState();
}

class _IncomingCallListenerState extends ConsumerState<IncomingCallListener> {
  StreamSubscription<SocketFrame>? _subscription;

  @override
  void initState() {
    super.initState();
    _subscription = ref.read(socketClientProvider).events.listen(_onEvent);
  }

  @override
  void dispose() {
    unawaited(_subscription?.cancel());
    super.dispose();
  }

  Future<void> _onEvent(SocketFrame frame) async {
    if (frame.event != 'call.incoming') {
      return;
    }

    final Map<String, dynamic> payload = frame.payload;
    final String? callId = payload['call_id'] as String?;
    final String? from =
        payload['from'] as String? ?? payload['initiator_id'] as String?;
    if (callId == null || from == null) {
      return;
    }

    // A call already in progress wins: answering a second one would tear down
    // the first, which is never what the user meant.
    if (ref.read(callControllerProvider) != null) {
      return;
    }

    await ref.read(callControllerProvider.notifier).receive(
          callId: callId,
          fromUserId: from,
          peerName: payload['display_name'] as String? ?? '',
          video: payload['type'] == 'video',
        );

    await widget.navigatorKey.currentState?.push(
      MaterialPageRoute<void>(builder: (_) => const CallScreen()),
    );
  }

  @override
  Widget build(BuildContext context) => widget.child;
}
