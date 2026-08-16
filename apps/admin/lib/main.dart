import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import 'core/admin_api.dart';
import 'features/dashboard/presentation/admin_shell.dart';

/// The API base URL is supplied at build time:
///
///   flutter build web --dart-define=SOBH_API_BASE_URL=https://api.sobh.app
const String apiBaseUrl = String.fromEnvironment(
  'SOBH_API_BASE_URL',
  defaultValue: 'http://localhost:8080',
);

final Provider<AdminApi> adminApiProvider =
    Provider<AdminApi>((Ref ref) => AdminApi(baseUrl: apiBaseUrl));

void main() {
  runApp(const ProviderScope(child: SobhAdminApp()));
}

class SobhAdminApp extends StatelessWidget {
  const SobhAdminApp({super.key});

  @override
  Widget build(BuildContext context) {
    return MaterialApp(
      title: 'SOBH Admin',
      debugShowCheckedModeBanner: false,
      theme: ThemeData(
        useMaterial3: true,
        colorSchemeSeed: const Color(0xFFE8813A),
        brightness: Brightness.light,
      ),
      darkTheme: ThemeData(
        useMaterial3: true,
        colorSchemeSeed: const Color(0xFFE8813A),
        brightness: Brightness.dark,
      ),
      locale: const Locale('fa'),
      supportedLocales: const <Locale>[
        Locale('fa'), Locale('en'), Locale('ar'), Locale('tr'),
      ],
      localizationsDelegates: const <LocalizationsDelegate<Object>>[
        GlobalMaterialLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
      ],
      home: const AdminShell(),
    );
  }
}
