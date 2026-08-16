import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../flags/presentation/flags_page.dart';
import '../../news/presentation/editorial_page.dart';
import '../../reports/presentation/reports_page.dart';
import '../../users/presentation/users_page.dart';
import 'dashboard_page.dart';

/// The panel's frame: a persistent navigation rail plus the active section.
///
/// Sections are not hidden by role here — the API refuses what the operator
/// may not do, and the page surfaces that refusal. Hiding them client-side as
/// well would only make the panel lie about what it can do when a permission
/// changes mid-session.
class AdminShell extends ConsumerStatefulWidget {
  const AdminShell({super.key});

  @override
  ConsumerState<AdminShell> createState() => _AdminShellState();
}

class _AdminShellState extends ConsumerState<AdminShell> {
  int _selectedIndex = 0;
  final TextEditingController _tokenController = TextEditingController();
  bool _authenticated = false;

  static const List<_Section> _sections = <_Section>[
    _Section('داشبورد', Icons.dashboard_outlined, Icons.dashboard),
    _Section('کاربران', Icons.people_outline, Icons.people),
    _Section('گزارش‌ها', Icons.flag_outlined, Icons.flag),
    _Section('اخبار', Icons.article_outlined, Icons.article),
    _Section('قابلیت‌ها', Icons.toggle_on_outlined, Icons.toggle_on),
  ];

  @override
  void dispose() {
    _tokenController.dispose();
    super.dispose();
  }

  void _applyToken() {
    final String token = _tokenController.text.trim();
    if (token.isEmpty) {
      return;
    }
    ref.read(adminApiProvider).setAccessToken(token);
    setState(() => _authenticated = true);
  }

  @override
  Widget build(BuildContext context) {
    if (!_authenticated) {
      return _SignInPage(controller: _tokenController, onSubmit: _applyToken);
    }

    return Scaffold(
      body: Row(
        children: <Widget>[
          NavigationRail(
            selectedIndex: _selectedIndex,
            onDestinationSelected: (int index) =>
                setState(() => _selectedIndex = index),
            labelType: NavigationRailLabelType.all,
            leading: const Padding(
              padding: EdgeInsets.symmetric(vertical: 16),
              child: Icon(Icons.wb_twilight, size: 32),
            ),
            destinations: _sections
                .map(
                  (_Section section) => NavigationRailDestination(
                    icon: Icon(section.icon),
                    selectedIcon: Icon(section.selectedIcon),
                    label: Text(section.label),
                  ),
                )
                .toList(),
          ),
          const VerticalDivider(width: 1),
          Expanded(child: _pageFor(_selectedIndex)),
        ],
      ),
    );
  }

  Widget _pageFor(int index) => switch (index) {
        0 => const DashboardPage(),
        1 => const UsersPage(),
        2 => const ReportsPage(),
        3 => const EditorialPage(),
        _ => const FlagsPage(),
      };
}

class _Section {
  const _Section(this.label, this.icon, this.selectedIcon);

  final String label;
  final IconData icon;
  final IconData selectedIcon;
}

/// Sign-in accepts an access token obtained through the normal OTP flow.
///
/// The panel deliberately has no separate credential of its own: an operator
/// is an ordinary account that happens to hold an admin role, so there is one
/// authentication path to audit rather than two.
class _SignInPage extends StatelessWidget {
  const _SignInPage({required this.controller, required this.onSubmit});

  final TextEditingController controller;
  final VoidCallback onSubmit;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: ConstrainedBox(
          constraints: const BoxConstraints(maxWidth: 420),
          child: Card(
            child: Padding(
              padding: const EdgeInsets.all(24),
              child: Column(
                mainAxisSize: MainAxisSize.min,
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: <Widget>[
                  Text(
                    'SOBH Admin',
                    style: Theme.of(context).textTheme.headlineSmall,
                  ),
                  const SizedBox(height: 8),
                  Text(
                    'برای ورود، توکن دسترسی حساب مدیر را وارد کنید',
                    style: Theme.of(context).textTheme.bodyMedium,
                  ),
                  const SizedBox(height: 24),
                  TextField(
                    controller: controller,
                    obscureText: true,
                    textDirection: TextDirection.ltr,
                    decoration: const InputDecoration(
                      labelText: 'Access token',
                      prefixIcon: Icon(Icons.key_outlined),
                    ),
                    onSubmitted: (_) => onSubmit(),
                  ),
                  const SizedBox(height: 16),
                  FilledButton(onPressed: onSubmit, child: const Text('ورود')),
                ],
              ),
            ),
          ),
        ),
      ),
    );
  }
}

/// AsyncSection renders the three states every data page needs, so no page
/// re-implements loading, error and empty handling.
class AsyncSection<T> extends StatelessWidget {
  const AsyncSection({
    required this.future,
    required this.builder,
    this.emptyMessage = 'موردی یافت نشد',
    super.key,
  });

  final Future<T> future;
  final Widget Function(BuildContext context, T data) builder;
  final String emptyMessage;

  @override
  Widget build(BuildContext context) {
    return FutureBuilder<T>(
      future: future,
      builder: (BuildContext context, AsyncSnapshot<T> snapshot) {
        if (snapshot.connectionState == ConnectionState.waiting) {
          return const Center(child: CircularProgressIndicator());
        }
        if (snapshot.hasError) {
          return _ErrorState(error: snapshot.error!);
        }

        final T? data = snapshot.data;
        if (data == null || (data is List && data.isEmpty)) {
          return Center(child: Text(emptyMessage));
        }
        return builder(context, data);
      },
    );
  }
}

class _ErrorState extends StatelessWidget {
  const _ErrorState({required this.error});

  final Object error;

  @override
  Widget build(BuildContext context) {
    final ColorScheme colors = Theme.of(context).colorScheme;

    // A permission failure is not a fault to report; it is the expected answer
    // for an operator whose role does not cover this section.
    if (error is AdminApiException &&
        (error as AdminApiException).isForbidden) {
      return Center(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            Icon(Icons.lock_outline, size: 48, color: colors.outline),
            const SizedBox(height: 12),
            const Text('دسترسی شما به این بخش مجاز نیست'),
          ],
        ),
      );
    }

    return Center(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Icon(Icons.error_outline, size: 48, color: colors.error),
          const SizedBox(height: 12),
          Text('$error', textAlign: TextAlign.center),
        ],
      ),
    );
  }
}
