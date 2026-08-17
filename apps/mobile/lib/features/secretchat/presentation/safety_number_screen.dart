import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../data/secret_chat_service.dart';

/// Verification (§24).
///
/// The safety number is derived from both parties' identity keys. It matches on
/// both devices only if each is talking to the party it believes it is, so
/// comparing it out of band — aloud, in person — is the one check a server
/// cannot forge. Everything else about a conversation passes through the
/// server, including the keys themselves.
///
/// It is shown per device, because that is what it is computed against. A
/// person with a phone and a tablet has two, and a number that matched for one
/// says nothing about the other.
class SafetyNumberScreen extends ConsumerWidget {
  const SafetyNumberScreen({
    super.key,
    required this.peerUserId,
    required this.peerName,
  });

  final String peerUserId;
  final String peerName;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<String>> numbers =
        ref.watch(safetyNumbersProvider(peerUserId));

    return Scaffold(
      appBar: AppBar(title: Text(l10n.safetyNumberTitle)),
      body: numbers.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(safetyNumbersProvider(peerUserId)),
        ),
        data: (List<String> values) {
          if (values.isEmpty) {
            return SobhEmptyState(
              icon: Icons.verified_user_outlined,
              title: l10n.safetyNumberTitle,
              // Not an error: the number is derived from a key this device
              // learns when it first talks to the peer's device.
              body: l10n.safetyNumberUnknown,
            );
          }

          return ListView(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            children: <Widget>[
              Text(peerName, style: Theme.of(context).textTheme.titleMedium),
              const SizedBox(height: SobhSpacing.sm),
              Text(
                l10n.safetyNumberExplain,
                style: Theme.of(context).textTheme.bodyMedium,
              ),
              const SizedBox(height: SobhSpacing.xl),
              for (int i = 0; i < values.length; i++) ...<Widget>[
                if (values.length > 1)
                  Padding(
                    padding: const EdgeInsets.only(bottom: SobhSpacing.sm),
                    child: Text(
                      l10n.safetyNumberDevice(i + 1, values.length),
                      style: Theme.of(context).textTheme.labelMedium,
                    ),
                  ),
                _SafetyNumber(value: values[i]),
                const SizedBox(height: SobhSpacing.lg),
              ],
            ],
          );
        },
      ),
    );
  }
}

/// Renders the digits in groups of five, the way they are meant to be read
/// aloud. A sixty-digit run is unreadable, and a number nobody reads out is a
/// check nobody performs.
class _SafetyNumber extends StatelessWidget {
  const _SafetyNumber({required this.value});

  final String value;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return Container(
      padding: const EdgeInsets.all(SobhSpacing.lg),
      decoration: BoxDecoration(
        color: palette.surfaceVariant,
        borderRadius: BorderRadius.circular(SobhRadius.md),
      ),
      child: Text(
        _grouped(value),
        // Latin digits at a fixed width, left to right, whatever the app's
        // language: this is a string to compare character by character, not
        // text to read, and localised digits or RTL reordering would make two
        // identical numbers look different on two phones.
        textDirection: TextDirection.ltr,
        textAlign: TextAlign.center,
        style: const TextStyle(
          fontFamily: 'monospace',
          fontFeatures: <FontFeature>[FontFeature.tabularFigures()],
          letterSpacing: 1.5,
          height: 1.8,
        ),
      ),
    );
  }

  static String _grouped(String value) {
    final String digits = value.replaceAll(RegExp(r'\s+'), '');
    final StringBuffer buffer = StringBuffer();
    for (int i = 0; i < digits.length; i += 5) {
      if (i > 0) {
        // A line every four groups keeps the block square enough to scan.
        buffer.write((i ~/ 5) % 4 == 0 ? '\n' : ' ');
      }
      buffer.write(
        digits.substring(i, i + 5 > digits.length ? digits.length : i + 5),
      );
    }
    return buffer.toString();
  }
}

/// The safety number for every device the peer has that this one has spoken to.
///
/// Devices this one has never exchanged a message with are skipped rather than
/// reported as an error: their identity key is not known here yet, and there is
/// genuinely no number to show until it is.
final AutoDisposeFutureProviderFamily<List<String>, String>
    safetyNumbersProvider =
    FutureProvider.autoDispose.family<List<String>, String>(
  (Ref ref, String peerUserId) async {
    final SecretChatService service = ref.watch(secretChatServiceProvider);
    final String? localUserId = ref.watch(sessionControllerProvider).userId;
    if (localUserId == null) {
      return const <String>[];
    }

    final List<DeviceBundle> devices = await service.bundlesFor(peerUserId);

    final List<String> numbers = <String>[];
    for (final DeviceBundle device in devices) {
      try {
        numbers.add(
          await service.safetyNumber(
            localUserId: localUserId,
            remoteUserId: peerUserId,
            remoteDeviceId: device.deviceId,
          ),
        );
      } on StateError {
        // No session with this device yet.
        continue;
      }
    }
    return numbers;
  },
);
