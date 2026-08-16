import 'package:flutter/material.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:sobh_app/core/localization/generated/app_localizations.dart';
import 'package:sobh_app/core/network/api_exception.dart';
import 'package:sobh_app/core/theme/app_theme.dart';
import 'package:sobh_app/core/widgets/async_states.dart';

/// Wraps a widget in just enough app to have localisations and a theme.
Widget host(Widget child, {Locale locale = const Locale('en')}) => MaterialApp(
      locale: locale,
      theme: AppTheme.light(),
      localizationsDelegates: const <LocalizationsDelegate<dynamic>>[
        AppLocalizations.delegate,
        GlobalMaterialLocalizations.delegate,
        GlobalWidgetsLocalizations.delegate,
        GlobalCupertinoLocalizations.delegate,
      ],
      supportedLocales: AppLocalizations.supportedLocales,
      home: Scaffold(body: child),
    );

void main() {
  group('SobhErrorState', () {
    // Being offline is a different problem from the server rejecting a
    // request, and the wording has to say so.
    testWidgets('names the network as the problem when offline',
        (WidgetTester tester) async {
      await tester.pumpWidget(
        host(
          const SobhErrorState(
            error: ApiException(
              code: ApiErrorCode.network,
              message: 'raw transport detail',
              statusCode: 0,
            ),
          ),
        ),
      );

      expect(find.text('Could not reach the server'), findsOneWidget);
      expect(find.text('raw transport detail'), findsNothing);
    });

    testWidgets('says so plainly when the caller is rate limited',
        (WidgetTester tester) async {
      await tester.pumpWidget(
        host(
          const SobhErrorState(
            error: ApiException(
              code: ApiErrorCode.rateLimited,
              message: 'slow down',
              statusCode: 429,
            ),
          ),
        ),
      );

      expect(find.text('Too many requests, please slow down'), findsOneWidget);
    });

    testWidgets('shows the server message for a rejection it cannot classify',
        (WidgetTester tester) async {
      await tester.pumpWidget(
        host(
          const SobhErrorState(
            error: ApiException(
              code: ApiErrorCode.validationFailed,
              message: 'That name is already taken',
              statusCode: 422,
            ),
          ),
        ),
      );

      expect(find.text('That name is already taken'), findsOneWidget);
    });

    testWidgets('falls back to the generic message for a non-API error',
        (WidgetTester tester) async {
      await tester.pumpWidget(host(SobhErrorState(error: Exception('boom'))));
      expect(
        find.text('Something went wrong, please try again'),
        findsOneWidget,
      );
    });

    testWidgets('offers retry only when there is something to retry',
        (WidgetTester tester) async {
      int retries = 0;
      await tester.pumpWidget(
        host(
          SobhErrorState(
            error: Exception('boom'),
            onRetry: () => retries++,
          ),
        ),
      );

      await tester.tap(find.text('Retry'));
      expect(retries, 1);

      await tester.pumpWidget(host(SobhErrorState(error: Exception('boom'))));
      expect(find.text('Retry'), findsNothing);
    });
  });

  group('SobhEmptyState', () {
    testWidgets('shows the title, the explanation and an action',
        (WidgetTester tester) async {
      await tester.pumpWidget(
        host(
          SobhEmptyState(
            icon: Icons.forum_outlined,
            title: 'No conversations yet',
            body: 'Pick a contact to get started',
            action: FilledButton(onPressed: () {}, child: const Text('Start')),
          ),
        ),
      );

      expect(find.text('No conversations yet'), findsOneWidget);
      expect(find.text('Pick a contact to get started'), findsOneWidget);
      expect(find.text('Start'), findsOneWidget);
    });
  });

  group('SobhAvatar', () {
    testWidgets('shows the first letter of the name',
        (WidgetTester tester) async {
      await tester.pumpWidget(host(const SobhAvatar(name: 'Alice')));
      expect(find.text('A'), findsOneWidget);
    });

    // Persian is written right to left, and "first" must mean the first
    // grapheme of the string, not a byte.
    testWidgets('shows the first letter of a Persian name',
        (WidgetTester tester) async {
      await tester.pumpWidget(host(const SobhAvatar(name: 'بابک')));
      expect(find.text('ب'), findsOneWidget);
    });

    testWidgets('degrades to a placeholder for a nameless entry',
        (WidgetTester tester) async {
      await tester.pumpWidget(host(const SobhAvatar(name: '   ')));
      expect(find.text('؟'), findsOneWidget);
    });
  });
}
