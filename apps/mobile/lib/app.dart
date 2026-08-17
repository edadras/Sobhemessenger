import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'core/localization/generated/app_localizations.dart';
import 'core/routing/app_router.dart';
import 'core/settings/settings_controller.dart';
import 'core/theme/app_theme.dart';
import 'features/calls/presentation/incoming_call_listener.dart';

/// Root of the SOBH application.
class SobhApp extends ConsumerWidget {
  const SobhApp({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppSettings settings = ref.watch(settingsControllerProvider);
    final GoRouterConfig router = ref.watch(appRouterProvider);

    return MaterialApp.router(
      title: 'SOBH',
      debugShowCheckedModeBanner: false,
      routerConfig: router.router,
      themeMode: settings.themeMode,
      theme: AppTheme.light(),
      darkTheme: AppTheme.dark(),
      locale: settings.locale,
      supportedLocales: AppLocalizations.supportedLocales,
      localizationsDelegates: const <LocalizationsDelegate<Object>>[
        AppLocalizations.delegate,
        GlobalMaterialLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
      ],
      builder: (BuildContext context, Widget? child) {
        // Text direction follows the chosen locale, so Persian and Arabic get
        // a true RTL layout rather than mirrored Latin (§44).
        return Directionality(
          textDirection: _directionFor(settings.locale),
          child: MediaQuery.withClampedTextScaling(
            // Respect the OS font-size setting, but cap it so chat bubbles and
            // action bars stay usable at the extremes (§45).
            minScaleFactor: 0.8,
            maxScaleFactor: 1.6,
            // A call can arrive on any screen, so the listener sits above the
            // whole navigator rather than on one of them (§18).
            child: IncomingCallListener(
              navigatorKey: rootNavigatorKey,
              child: child ?? const SizedBox.shrink(),
            ),
          ),
        );
      },
    );
  }

  TextDirection _directionFor(Locale? locale) {
    const Set<String> rtlLanguages = <String>{'fa', 'ar', 'he', 'ur'};
    return rtlLanguages.contains(locale?.languageCode ?? 'fa')
        ? TextDirection.rtl
        : TextDirection.ltr;
  }
}
