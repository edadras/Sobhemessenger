import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:shared_preferences/shared_preferences.dart';

/// User-facing app settings (§43, §44).
@immutable
class AppSettings {
  const AppSettings({
    this.themeMode = ThemeMode.system,
    this.locale = const Locale('fa'),
  });

  final ThemeMode themeMode;
  final Locale locale;

  AppSettings copyWith({ThemeMode? themeMode, Locale? locale}) => AppSettings(
        themeMode: themeMode ?? this.themeMode,
        locale: locale ?? this.locale,
      );
}

/// The languages the app ships (§44). A stored value outside this set is
/// ignored rather than trusted, so a downgrade cannot leave the app in a
/// language it no longer has strings for.
const Set<String> supportedLanguageCodes = <String>{'fa', 'en', 'tr', 'ar'};

const String _themeKey = 'settings.theme_mode';
const String _localeKey = 'settings.locale';

/// Holds theme and language, and persists both.
///
/// Persian is the default: it is the launch market, and an English default
/// would make first launch wrong for most users. Preferences are stored in
/// plain shared preferences rather than the keychain — they are not secrets,
/// and reading them must not be slow enough to delay first paint.
class SettingsController extends Notifier<AppSettings> {
  @override
  AppSettings build() {
    unawaited(_restore());
    return const AppSettings();
  }

  Future<void> _restore() async {
    final SharedPreferences prefs = await SharedPreferences.getInstance();

    final String? storedTheme = prefs.getString(_themeKey);
    final String? storedLocale = prefs.getString(_localeKey);

    state = AppSettings(
      themeMode: switch (storedTheme) {
        'light' => ThemeMode.light,
        'dark' => ThemeMode.dark,
        _ => ThemeMode.system,
      },
      locale:
          storedLocale != null && supportedLanguageCodes.contains(storedLocale)
              ? Locale(storedLocale)
              : const Locale('fa'),
    );
  }

  Future<void> setThemeMode(ThemeMode mode) async {
    state = state.copyWith(themeMode: mode);
    final SharedPreferences prefs = await SharedPreferences.getInstance();
    await prefs.setString(_themeKey, mode.name);
  }

  Future<void> setLocale(Locale locale) async {
    if (!supportedLanguageCodes.contains(locale.languageCode)) {
      return;
    }
    state = state.copyWith(locale: locale);
    final SharedPreferences prefs = await SharedPreferences.getInstance();
    await prefs.setString(_localeKey, locale.languageCode);
  }
}

final NotifierProvider<SettingsController, AppSettings>
    settingsControllerProvider =
    NotifierProvider<SettingsController, AppSettings>(SettingsController.new);
