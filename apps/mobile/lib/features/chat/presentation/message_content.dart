import 'dart:convert';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../stickers/data/stickers_repository.dart';
import '../data/bot_interaction_repository.dart';

/// Drawing the message types that are more than text (§12, §13).
///
/// A location, a contact card and an inline keyboard all arrive as structured
/// payloads. Everything here is defensive about what it reads: the server
/// validates on the way in, but a client that crashes on an unexpected shape
/// takes the whole conversation down with it, and one bad message would be
/// enough.

/// A shared location, drawn as a card rather than a map.
///
/// Whether a location payload is still being shared.
///
/// Shared with the chat screen, which needs the same answer to decide whether
/// to offer "stop sharing" — asking it in two places with two rules would let
/// the button and the bubble disagree about what the message is.
bool isLiveLocation(String payloadJson) {
  final Object? until = _decode(payloadJson)['live_until'];
  if (until is! String) {
    return false;
  }
  final DateTime? deadline = DateTime.tryParse(until)?.toLocal();
  return deadline != null && deadline.isAfter(DateTime.now());
}

/// The coordinates open in whatever map application the device has, rather
/// than the app embedding a map: that would mean shipping a tile provider's
/// SDK and telling it where every user is, which is a lot to give away for a
/// picture of a street.
class LocationBubble extends StatelessWidget {
  const LocationBubble({required this.payloadJson, super.key});

  final String payloadJson;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    final Map<String, dynamic> payload = _decode(payloadJson);
    final double? latitude = (payload['latitude'] as num?)?.toDouble();
    final double? longitude = (payload['longitude'] as num?)?.toDouble();
    if (latitude == null || longitude == null) {
      return Text(
        l10n.messageUnsupported,
        style: TextStyle(color: palette.textSecondary),
      );
    }

    final String placeName = payload['place_name'] as String? ?? '';
    final String placeAddress = payload['place_address'] as String? ?? '';
    final DateTime? liveUntil = payload['live_until'] == null
        ? null
        : DateTime.tryParse(payload['live_until'] as String)?.toLocal();
    final bool isLive = liveUntil != null && liveUntil.isAfter(DateTime.now());

    return InkWell(
      borderRadius: BorderRadius.circular(SobhRadius.md),
      onTap: () => _openInMaps(latitude, longitude, placeName),
      child: Row(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Icon(
            isLive ? Icons.share_location : Icons.location_on_outlined,
            color: isLive ? palette.success : palette.textSecondary,
            size: SobhSizes.iconLarge,
          ),
          const SizedBox(width: SobhSpacing.md),
          Flexible(
            child: Column(
              crossAxisAlignment: CrossAxisAlignment.start,
              mainAxisSize: MainAxisSize.min,
              children: <Widget>[
                Text(
                  placeName.isNotEmpty ? placeName : l10n.locationShared,
                  style: Theme.of(context).textTheme.bodyMedium,
                ),
                if (placeAddress.isNotEmpty)
                  Text(
                    placeAddress,
                    style: Theme.of(context)
                        .textTheme
                        .bodySmall
                        ?.copyWith(color: palette.textSecondary),
                  ),
                if (isLive)
                  Text(
                    l10n.locationLiveUntil(
                      TimeOfDay.fromDateTime(liveUntil).format(context),
                    ),
                    style: Theme.of(context)
                        .textTheme
                        .bodySmall
                        ?.copyWith(color: palette.success),
                  )
                else if (liveUntil != null)
                  // A share that has finished says so, rather than looking like
                  // a pin that is still moving.
                  Text(
                    l10n.locationLiveEnded,
                    style: Theme.of(context)
                        .textTheme
                        .bodySmall
                        ?.copyWith(color: palette.textDisabled),
                  ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  Future<void> _openInMaps(
    double latitude,
    double longitude,
    String label,
  ) async {
    // geo: is what Android and iOS both understand; a device with no map
    // application simply does nothing, which is better than an error about a
    // scheme the user has never heard of.
    final Uri uri = Uri.parse(
      'geo:$latitude,$longitude?q=$latitude,$longitude'
      '${label.isEmpty ? '' : '(${Uri.encodeComponent(label)})'}',
    );
    if (await canLaunchUrl(uri)) {
      await launchUrl(uri);
    }
  }
}

/// A shared contact card.
class ContactBubble extends StatelessWidget {
  const ContactBubble({required this.payloadJson, super.key});

  final String payloadJson;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    final Map<String, dynamic> payload = _decode(payloadJson);
    final String phone = payload['phone_number'] as String? ?? '';
    final String first = payload['first_name'] as String? ?? '';
    final String last = payload['last_name'] as String? ?? '';
    if (phone.isEmpty && first.isEmpty) {
      return Text(
        l10n.messageUnsupported,
        style: TextStyle(color: palette.textSecondary),
      );
    }

    final String name = '$first $last'.trim();

    return Row(
      mainAxisSize: MainAxisSize.min,
      children: <Widget>[
        SobhAvatar(name: name.isEmpty ? phone : name),
        const SizedBox(width: SobhSpacing.md),
        Flexible(
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            mainAxisSize: MainAxisSize.min,
            children: <Widget>[
              Text(
                name.isEmpty ? phone : name,
                style: Theme.of(context).textTheme.bodyMedium,
              ),
              if (name.isNotEmpty)
                Text(
                  phone,
                  style: Theme.of(context)
                      .textTheme
                      .bodySmall
                      ?.copyWith(color: palette.textSecondary),
                ),
            ],
          ),
        ),
      ],
    );
  }
}

/// The buttons a bot attached under a message.
class InlineKeyboardView extends ConsumerStatefulWidget {
  const InlineKeyboardView({
    required this.message,
    required this.markupJson,
    super.key,
  });

  final MessageRow message;
  final String markupJson;

  @override
  ConsumerState<InlineKeyboardView> createState() => _InlineKeyboardViewState();
}

class _InlineKeyboardViewState extends ConsumerState<InlineKeyboardView> {
  /// The button currently waiting for its bot, so a second tap does not queue
  /// a second callback while the first is still in flight.
  String? _pending;

  @override
  Widget build(BuildContext context) {
    final List<List<Map<String, dynamic>>> rows = _rows(widget.markupJson);
    if (rows.isEmpty) {
      return const SizedBox.shrink();
    }

    return Padding(
      padding: const EdgeInsets.only(top: SobhSpacing.sm),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          for (final List<Map<String, dynamic>> row in rows)
            Padding(
              padding: const EdgeInsets.only(bottom: SobhSpacing.xs),
              child: Row(
                mainAxisSize: MainAxisSize.min,
                children: <Widget>[
                  for (final Map<String, dynamic> button in row)
                    Expanded(child: _button(button)),
                ],
              ),
            ),
        ],
      ),
    );
  }

  Widget _button(Map<String, dynamic> button) {
    final String text = button['text'] as String? ?? '';
    final String? callbackData = button['callback_data'] as String?;
    final String? url = button['url'] as String?;
    final bool busy = _pending != null && _pending == callbackData;

    return Padding(
      padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.xxs),
      child: OutlinedButton(
        onPressed: _pending != null
            ? null
            : () {
                if (callbackData != null) {
                  _tap(callbackData);
                } else if (url != null) {
                  _open(url);
                }
              },
        child: busy
            ? const SizedBox(
                width: SobhSizes.iconSmall,
                height: SobhSizes.iconSmall,
                child: CircularProgressIndicator(strokeWidth: 2),
              )
            : Text(text, maxLines: 1, overflow: TextOverflow.ellipsis),
      ),
    );
  }

  Future<void> _tap(String data) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() => _pending = data);

    try {
      await ref
          .read(botInteractionRepositoryProvider)
          .tap(messageId: widget.message.id, data: data);
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
        setState(() => _pending = null);
      }
    }
  }

  Future<void> _open(String url) async {
    final Uri? uri = Uri.tryParse(url);
    if (uri != null && await canLaunchUrl(uri)) {
      await launchUrl(uri, mode: LaunchMode.externalApplication);
    }
  }

  /// Reads the keyboard, tolerating anything that is not the shape expected.
  static List<List<Map<String, dynamic>>> _rows(String raw) {
    final Map<String, dynamic> markup = _decode(raw);
    final dynamic keyboard = markup['inline_keyboard'];
    if (keyboard is! List) {
      return const <List<Map<String, dynamic>>>[];
    }

    return <List<Map<String, dynamic>>>[
      for (final dynamic row in keyboard)
        if (row is List)
          <Map<String, dynamic>>[
            for (final dynamic button in row)
              if (button is Map<String, dynamic>) button,
          ],
    ];
  }
}

Map<String, dynamic> _decode(String raw) {
  try {
    final dynamic decoded = jsonDecode(raw);
    return decoded is Map<String, dynamic> ? decoded : <String, dynamic>{};
  } on FormatException {
    // Malformed JSON in one message must not take the conversation down with
    // it; the bubble falls back to "unsupported".
    return <String, dynamic>{};
  }
}

/// The unfurled preview of the first link in a message (§27).
///
/// OpenGraph unfurling was built on the server, behind the SSRF guard, with a
/// cache that also caches failures — and the app never asked for one, so a
/// link was a link. It renders nothing at all when there is no preview, which
/// is the common case: most links have no OpenGraph tags, and a box saying so
/// would be worse than the bare link.
class LinkPreviewCard extends ConsumerWidget {
  const LinkPreviewCard({super.key, required this.url});

  final String url;

  /// The first http(s) link in a message, or null. Only the first: a message
  /// full of links should not become a wall of cards.
  static String? firstLinkIn(String content) {
    final RegExpMatch? match =
        RegExp(r'https?://[^\s<>"]+').firstMatch(content);
    return match?.group(0);
  }

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final SobhPalette palette = SobhTheme.of(context);
    final LinkPreview? preview =
        ref.watch(linkPreviewProvider(url)).valueOrNull;
    if (preview == null || (preview.title.isEmpty && preview.siteName.isEmpty)) {
      return const SizedBox.shrink();
    }

    return Padding(
      padding: const EdgeInsets.only(top: SobhSpacing.xs),
      child: Container(
        padding: const EdgeInsets.all(SobhSpacing.sm),
        decoration: BoxDecoration(
          color: palette.surfaceVariant,
          borderRadius: BorderRadius.circular(SobhRadius.md),
          border: BorderDirectional(
            start: BorderSide(color: palette.primary, width: 3),
          ),
        ),
        child: Column(
          crossAxisAlignment: CrossAxisAlignment.start,
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            if (preview.siteName.isNotEmpty)
              Text(
                preview.siteName,
                style: Theme.of(context)
                    .textTheme
                    .labelSmall
                    ?.copyWith(color: palette.primary),
              ),
            if (preview.title.isNotEmpty)
              Text(
                preview.title,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: Theme.of(context).textTheme.bodyMedium,
              ),
            if (preview.description.isNotEmpty)
              Text(
                preview.description,
                maxLines: 2,
                overflow: TextOverflow.ellipsis,
                style: Theme.of(context)
                    .textTheme
                    .bodySmall
                    ?.copyWith(color: palette.textSecondary),
              ),
          ],
        ),
      ),
    );
  }
}
