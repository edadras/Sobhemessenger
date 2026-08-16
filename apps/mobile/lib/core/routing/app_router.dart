import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../features/auth/presentation/phone_entry_screen.dart';
import '../../features/auth/presentation/verify_code_screen.dart';
import '../../features/auth/session_controller.dart';
import '../../features/calls/presentation/calls_screen.dart';
import '../../features/chat/presentation/chat_list_screen.dart';
import '../../features/chat/presentation/chat_screen.dart';
import '../../features/contacts/presentation/contacts_screen.dart';
import '../../features/groups/presentation/chat_info_screen.dart';
import '../../features/news/presentation/news_feed_screen.dart';
import '../../features/profile/presentation/profile_screen.dart';
import '../../features/search/presentation/search_screen.dart';
import '../../features/settings/presentation/settings_screen.dart';
import '../../features/shell/presentation/home_shell.dart';
import '../../features/stories/presentation/stories_tray.dart';

/// Route names, referenced by constant so a typo is a compile error.
abstract final class Routes {
  const Routes._();

  static const String splash = '/';
  static const String phoneEntry = '/auth/phone';
  static const String verifyCode = '/auth/verify';
  static const String chats = '/chats';
  static const String chat = '/chats/:chatId';
  static const String contacts = '/contacts';
  static const String news = '/news';
  static const String calls = '/calls';
  static const String stories = '/stories';
  static const String search = '/search';
  static const String profile = '/profile';
  static const String settings = '/settings';
}

/// Wraps GoRouter so the provider exposes a stable object across rebuilds.
class GoRouterConfig {
  const GoRouterConfig(this.router);

  final GoRouter router;
}

final GlobalKey<NavigatorState> _rootNavigator = GlobalKey<NavigatorState>();

final Provider<GoRouterConfig> appRouterProvider =
    Provider<GoRouterConfig>((Ref ref) {
  final SessionState session = ref.watch(sessionControllerProvider);

  return GoRouterConfig(
    GoRouter(
      navigatorKey: _rootNavigator,
      initialLocation: Routes.splash,
      // Redirect is the single place authentication decides where the user
      // may be, so no screen has to guard itself.
      redirect: (BuildContext context, GoRouterState state) {
        final bool signedIn = session.isAuthenticated;
        final bool onAuthRoute = state.matchedLocation.startsWith('/auth');

        if (session.isRestoring) {
          return null;
        }
        if (!signedIn && !onAuthRoute) {
          return Routes.phoneEntry;
        }
        if (signedIn &&
            (onAuthRoute || state.matchedLocation == Routes.splash)) {
          return Routes.chats;
        }
        return null;
      },
      routes: <RouteBase>[
        GoRoute(path: Routes.splash, builder: (_, __) => const _SplashScreen()),
        GoRoute(
          path: Routes.phoneEntry,
          builder: (_, __) => const PhoneEntryScreen(),
        ),
        GoRoute(
          path: Routes.verifyCode,
          builder: (BuildContext context, GoRouterState state) =>
              VerifyCodeScreen(
            phone: state.uri.queryParameters['phone'] ?? '',
          ),
        ),

        // The five tabs, each with its own navigation stack so switching tabs
        // does not discard where the user was.
        StatefulShellRoute.indexedStack(
          builder: (
            BuildContext context,
            GoRouterState state,
            StatefulNavigationShell shell,
          ) =>
              HomeShell(navigationShell: shell),
          branches: <StatefulShellBranch>[
            StatefulShellBranch(
              routes: <RouteBase>[
                GoRoute(
                  path: Routes.chats,
                  builder: (_, __) => const ChatListScreen(),
                  routes: <RouteBase>[
                    GoRoute(
                      path: ':chatId',
                      // A conversation covers the bottom bar: it is a
                      // destination in its own right, not a tab.
                      parentNavigatorKey: _rootNavigator,
                      builder: (BuildContext context, GoRouterState state) =>
                          ChatScreen(chatId: state.pathParameters['chatId']!),
                      routes: <RouteBase>[
                        GoRoute(
                          path: 'info',
                          parentNavigatorKey: _rootNavigator,
                          builder:
                              (BuildContext context, GoRouterState state) =>
                                  ChatInfoScreen(
                            chatId: state.pathParameters['chatId']!,
                            title: state.uri.queryParameters['title'] ?? '',
                          ),
                        ),
                      ],
                    ),
                  ],
                ),
              ],
            ),
            StatefulShellBranch(
              routes: <RouteBase>[
                GoRoute(
                  path: Routes.contacts,
                  builder: (_, __) => const ContactsScreen(),
                ),
              ],
            ),
            StatefulShellBranch(
              routes: <RouteBase>[
                GoRoute(
                  path: Routes.news,
                  builder: (_, __) => const NewsFeedScreen(),
                ),
              ],
            ),
            StatefulShellBranch(
              routes: <RouteBase>[
                GoRoute(
                  path: Routes.calls,
                  builder: (_, __) => const CallsScreen(),
                ),
              ],
            ),
            StatefulShellBranch(
              routes: <RouteBase>[
                GoRoute(
                  path: Routes.profile,
                  builder: (_, __) => const ProfileScreen(),
                ),
              ],
            ),
          ],
        ),

        // Reachable from several tabs, so they sit above the shell rather than
        // inside any one branch.
        GoRoute(path: Routes.search, builder: (_, __) => const SearchScreen()),
        GoRoute(
          path: Routes.stories,
          builder: (_, __) => const StoriesScreen(),
        ),
        GoRoute(
          path: Routes.settings,
          builder: (_, __) => const SettingsScreen(),
        ),
      ],
      errorBuilder: (BuildContext context, GoRouterState state) =>
          _RouteNotFoundScreen(location: state.matchedLocation),
    ),
  );
});

class _SplashScreen extends StatelessWidget {
  const _SplashScreen();

  @override
  Widget build(BuildContext context) =>
      const Scaffold(body: Center(child: CircularProgressIndicator()));
}

class _RouteNotFoundScreen extends StatelessWidget {
  const _RouteNotFoundScreen({required this.location});

  final String location;

  @override
  Widget build(BuildContext context) {
    return Scaffold(
      body: Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: <Widget>[
            Text(location, style: Theme.of(context).textTheme.bodyMedium),
            TextButton(
              onPressed: () => context.go(Routes.chats),
              child: const Text('SOBH'),
            ),
          ],
        ),
      ),
    );
  }
}
