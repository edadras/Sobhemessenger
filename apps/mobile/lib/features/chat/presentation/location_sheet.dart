import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_contacts/flutter_contacts.dart';
import 'package:geolocator/geolocator.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';

/// Sharing a location or a contact (§12).

/// What the user chose to share.
class SharedLocation {
  const SharedLocation({
    required this.latitude,
    required this.longitude,
    this.accuracy = 0,
    this.livePeriodSeconds = 0,
  });

  final double latitude;
  final double longitude;
  final double accuracy;

  /// Zero for a fixed point; otherwise how long to keep updating.
  final int livePeriodSeconds;

  bool get isLive => livePeriodSeconds > 0;

  Map<String, dynamic> toPayload() => <String, dynamic>{
        'latitude': latitude,
        'longitude': longitude,
        'horizontal_accuracy': accuracy,
        if (isLive) 'live_period_seconds': livePeriodSeconds,
      };
}

/// Asks for a fix and offers to share it once or keep it updating.
///
/// Returns null when the user backs out or the platform refuses, which is the
/// common case and not an error worth an alert.
Future<SharedLocation?> showLocationSheet(BuildContext context) =>
    showModalBottomSheet<SharedLocation>(
      context: context,
      isScrollControlled: true,
      builder: (BuildContext context) => const _LocationSheet(),
    );

class _LocationSheet extends StatefulWidget {
  const _LocationSheet();

  @override
  State<_LocationSheet> createState() => _LocationSheetState();
}

class _LocationSheetState extends State<_LocationSheet> {
  Position? _position;
  String? _problem;
  bool _locating = true;

  @override
  void initState() {
    super.initState();
    unawaited(_locate());
  }

  Future<void> _locate() async {
    setState(() {
      _locating = true;
      _problem = null;
    });

    try {
      if (!await Geolocator.isLocationServiceEnabled()) {
        _fail((AppLocalizations l10n) => l10n.locationServiceOff);
        return;
      }

      LocationPermission permission = await Geolocator.checkPermission();
      if (permission == LocationPermission.denied) {
        permission = await Geolocator.requestPermission();
      }
      // A permanent refusal is not something asking again will fix, so it is
      // reported differently from a "not now".
      if (permission == LocationPermission.deniedForever) {
        _fail((AppLocalizations l10n) => l10n.locationPermissionPermanent);
        return;
      }
      if (permission == LocationPermission.denied) {
        _fail((AppLocalizations l10n) => l10n.locationPermissionDenied);
        return;
      }

      final Position position = await Geolocator.getCurrentPosition();
      if (mounted) {
        setState(() {
          _position = position;
          _locating = false;
        });
      }
    } on Object {
      _fail((AppLocalizations l10n) => l10n.locationUnavailable);
    }
  }

  void _fail(String Function(AppLocalizations) message) {
    if (mounted) {
      setState(() {
        _problem = message(AppLocalizations.of(context));
        _locating = false;
      });
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return SafeArea(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.lg),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.stretch,
          children: <Widget>[
            Text(
              l10n.locationShare,
              style: Theme.of(context).textTheme.titleMedium,
              textAlign: TextAlign.center,
            ),
            const SizedBox(height: SobhSpacing.lg),
            if (_locating)
              const Center(child: CircularProgressIndicator())
            else if (_problem != null) ...<Widget>[
              Text(
                _problem!,
                textAlign: TextAlign.center,
                style: TextStyle(color: palette.error),
              ),
              const SizedBox(height: SobhSpacing.md),
              OutlinedButton(
                onPressed: _locate,
                child: Text(l10n.commonRetry),
              ),
            ] else if (_position != null) ...<Widget>[
              ListTile(
                leading: const Icon(Icons.location_on_outlined),
                title: Text(l10n.locationSendCurrent),
                // The accuracy is shown rather than hidden: "within 40 metres"
                // is a different offer from "within 4".
                subtitle: Text(
                  l10n.locationAccuracy(_position!.accuracy.round()),
                ),
                onTap: () => Navigator.of(context).pop(
                  SharedLocation(
                    latitude: _position!.latitude,
                    longitude: _position!.longitude,
                    accuracy: _position!.accuracy,
                  ),
                ),
              ),
              const Divider(),
              Padding(
                padding: const EdgeInsets.symmetric(
                  horizontal: SobhSpacing.lg,
                  vertical: SobhSpacing.sm,
                ),
                child: Text(
                  l10n.locationLiveExplain,
                  style: Theme.of(context)
                      .textTheme
                      .bodySmall
                      ?.copyWith(color: palette.textSecondary),
                ),
              ),
              for (final ({String label, int seconds}) option
                  in <({String label, int seconds})>[
                (label: l10n.locationLive15Minutes, seconds: 15 * 60),
                (label: l10n.locationLive1Hour, seconds: 60 * 60),
                (label: l10n.locationLive8Hours, seconds: 8 * 60 * 60),
              ])
                ListTile(
                  leading: const Icon(Icons.share_location),
                  title: Text(option.label),
                  onTap: () => Navigator.of(context).pop(
                    SharedLocation(
                      latitude: _position!.latitude,
                      longitude: _position!.longitude,
                      accuracy: _position!.accuracy,
                      livePeriodSeconds: option.seconds,
                    ),
                  ),
                ),
            ],
          ],
        ),
      ),
    );
  }
}

/// A contact card the user picked from the address book.
class SharedContact {
  const SharedContact({
    required this.phoneNumber,
    required this.firstName,
    this.lastName = '',
  });

  final String phoneNumber;
  final String firstName;
  final String lastName;

  Map<String, dynamic> toPayload() => <String, dynamic>{
        'phone_number': phoneNumber,
        'first_name': firstName,
        if (lastName.isNotEmpty) 'last_name': lastName,
      };
}

/// Picks a contact to share.
///
/// Only the name and one number are taken. The address book holds a great deal
/// more, and sharing "a contact" should not mean handing over someone's whole
/// record because it happened to be available.
Future<SharedContact?> pickContactToShare(BuildContext context) async {
  if (!await FlutterContacts.requestPermission(readonly: true)) {
    return null;
  }

  final Contact? picked = await FlutterContacts.openExternalPick();
  if (picked == null) {
    return null;
  }

  final Contact? full =
      await FlutterContacts.getContact(picked.id, withProperties: true);
  final Contact contact = full ?? picked;
  if (contact.phones.isEmpty) {
    return null;
  }

  return SharedContact(
    phoneNumber: contact.phones.first.number,
    firstName:
        contact.name.first.isEmpty ? contact.displayName : contact.name.first,
    lastName: contact.name.last,
  );
}
