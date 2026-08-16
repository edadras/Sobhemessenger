import 'package:flutter/material.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../groups/data/groups_repository.dart';
import '../../groups/presentation/create_chat_screen.dart';
import '../../search/presentation/search_screen.dart';

/// The destinations of the app, in the order they appear (§45).
///
/// The order is the branch order in the router: `HomeTab.values[index]` is how
/// the shell knows which tab it is on.
enum HomeTab { chats, contacts, news, calls, profile }

/// The bottom-navigation shell.
///
/// It wraps a StatefulShellRoute branch so each tab keeps its own navigation
/// stack: opening an article and switching to Chats and back must return to
/// the article, not to the top of the feed.
class HomeShell extends StatelessWidget {
  const HomeShell({super.key, required this.navigationShell});

  final StatefulNavigationShell navigationShell;

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final HomeTab current = HomeTab.values[navigationShell.currentIndex];

    return Scaffold(
      body: navigationShell,
      // Composing belongs to the chats tab: on every other tab the button
      // would either do nothing useful or mean something different.
      floatingActionButton: current == HomeTab.chats
          ? FloatingActionButton(
              onPressed: () => _showComposeSheet(context, l10n),
              child: const Icon(Icons.edit_outlined),
            )
          : null,
      bottomNavigationBar: NavigationBar(
        selectedIndex: navigationShell.currentIndex,
        // initialLocation restores the branch's root when the current tab is
        // tapped again, which is the behaviour a bottom bar is expected to have.
        onDestinationSelected: (int index) => navigationShell.goBranch(
          index,
          initialLocation: index == navigationShell.currentIndex,
        ),
        destinations: <NavigationDestination>[
          NavigationDestination(
            icon: const Icon(Icons.forum_outlined),
            selectedIcon: const Icon(Icons.forum),
            label: l10n.navChats,
          ),
          NavigationDestination(
            icon: const Icon(Icons.contacts_outlined),
            selectedIcon: const Icon(Icons.contacts),
            label: l10n.navContacts,
          ),
          NavigationDestination(
            icon: const Icon(Icons.article_outlined),
            selectedIcon: const Icon(Icons.article),
            label: l10n.navNews,
          ),
          NavigationDestination(
            icon: const Icon(Icons.call_outlined),
            selectedIcon: const Icon(Icons.call),
            label: l10n.navCalls,
          ),
          NavigationDestination(
            icon: const Icon(Icons.person_outline),
            selectedIcon: const Icon(Icons.person),
            label: l10n.navProfile,
          ),
        ],
      ),
    );
  }

  void _showComposeSheet(BuildContext context, AppLocalizations l10n) {
    showModalBottomSheet<void>(
      context: context,
      builder: (BuildContext sheetContext) => SafeArea(
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            ListTile(
              leading: const Icon(Icons.group_add_outlined),
              title: Text(l10n.groupsCreateTitle),
              onTap: () {
                Navigator.of(sheetContext).pop();
                Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) =>
                        const CreateChatScreen(kind: ChatKind.group),
                  ),
                );
              },
            ),
            ListTile(
              leading: const Icon(Icons.campaign_outlined),
              title: Text(l10n.channelsCreateTitle),
              onTap: () {
                Navigator.of(sheetContext).pop();
                Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) =>
                        const CreateChatScreen(kind: ChatKind.channel),
                  ),
                );
              },
            ),
            ListTile(
              leading: const Icon(Icons.explore_outlined),
              title: Text(l10n.groupsDiscover),
              onTap: () {
                Navigator.of(sheetContext).pop();
                Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) => const DiscoverScreen(),
                  ),
                );
              },
            ),
            ListTile(
              leading: const Icon(Icons.search),
              title: Text(l10n.searchTitle),
              onTap: () {
                Navigator.of(sheetContext).pop();
                Navigator.of(context).push(
                  MaterialPageRoute<void>(builder: (_) => const SearchScreen()),
                );
              },
            ),
          ],
        ),
      ),
    );
  }
}
