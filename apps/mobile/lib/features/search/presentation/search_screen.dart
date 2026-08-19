import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../chat/data/chat_repository.dart';
import '../../groups/presentation/join_by_link_screen.dart';
import '../../profile/data/profile_repository.dart';
import '../data/search_repository.dart';

/// Global search (§28).
class SearchScreen extends ConsumerStatefulWidget {
  const SearchScreen({super.key});

  @override
  ConsumerState<SearchScreen> createState() => _SearchScreenState();
}

class _SearchScreenState extends ConsumerState<SearchScreen> {
  final TextEditingController _controller = TextEditingController();
  Timer? _debounce;
  String _query = '';

  @override
  void dispose() {
    _debounce?.cancel();
    _controller.dispose();
    super.dispose();
  }

  /// Queries are debounced so typing a word is one request, not eight.
  void _onChanged(String value) {
    _debounce?.cancel();
    _debounce = Timer(SobhDuration.slow, () {
      if (mounted) {
        setState(() => _query = value.trim());
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Scaffold(
      appBar: AppBar(
        title: TextField(
          controller: _controller,
          autofocus: true,
          textInputAction: TextInputAction.search,
          decoration: InputDecoration(
            hintText: l10n.searchHint,
            border: InputBorder.none,
          ),
          onChanged: _onChanged,
        ),
        actions: <Widget>[
          if (_controller.text.isNotEmpty)
            IconButton(
              icon: const Icon(Icons.clear),
              onPressed: () {
                _controller.clear();
                setState(() => _query = '');
              },
            ),
        ],
      ),
      body: _query.startsWith('@') && _query.length > 1
          // A leading @ is somebody naming one exact account, not searching.
          // Looking a username up was in the repository and reachable from
          // nowhere, so an exact handle went through the fuzzy search and
          // could come back below three near-misses.
          ? _ByUsername(username: _query)
          : _query.isEmpty
          ? ListView(
              children: <Widget>[
                // An invite link arrives as a message somewhere else, so the
                // place to redeem it is wherever someone goes to find a chat.
                ListTile(
                  leading: const Icon(Icons.link),
                  title: Text(l10n.joinByLinkTitle),
                  subtitle: Text(l10n.joinByLinkBody),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => const JoinByLinkScreen(),
                    ),
                  ),
                ),
                const Divider(),
                Padding(
                  padding: const EdgeInsets.only(top: SobhSpacing.xl),
                  child: SobhEmptyState(
                    icon: Icons.search,
                    title: l10n.searchPrompt,
                  ),
                ),
              ],
            )
          : _Results(query: _query),
    );
  }
}

class _Results extends ConsumerWidget {
  const _Results({required this.query});

  final String query;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<SearchResults> results = ref.watch(searchProvider(query));

    return results.when(
      loading: () => const SobhLoading(),
      error: (Object error, StackTrace _) => SobhErrorState(
        error: error,
        onRetry: () => ref.invalidate(searchProvider(query)),
      ),
      data: (SearchResults found) {
        if (found.isEmpty) {
          return SobhEmptyState(
            icon: Icons.search_off,
            title: l10n.searchEmpty,
          );
        }
        return ListView(
          children: <Widget>[
            _Section(
              title: l10n.searchSectionMessages,
              results: found.messages,
            ),
            _Section(title: l10n.searchSectionPeople, results: found.users),
            _Section(title: l10n.searchSectionChats, results: found.chats),
            _Section(title: l10n.searchSectionNews, results: found.news),
          ],
        );
      },
    );
  }
}

class _Section extends StatelessWidget {
  const _Section({required this.title, required this.results});

  final String title;
  final List<SearchResult> results;

  @override
  Widget build(BuildContext context) {
    if (results.isEmpty) {
      return const SizedBox.shrink();
    }

    final SobhPalette palette = SobhTheme.of(context);
    return Column(
      crossAxisAlignment: CrossAxisAlignment.stretch,
      children: <Widget>[
        Padding(
          padding: const EdgeInsets.fromLTRB(
            SobhSpacing.lg,
            SobhSpacing.lg,
            SobhSpacing.lg,
            SobhSpacing.sm,
          ),
          child: Text(
            title,
            style: Theme.of(context)
                .textTheme
                .labelLarge
                ?.copyWith(color: palette.textSecondary),
          ),
        ),
        for (final SearchResult result in results)
          ListTile(
            leading: SobhAvatar(name: result.title),
            title: Text(
              result.title,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
            ),
            subtitle: result.snippet.isEmpty
                ? null
                : Text(
                    result.snippet,
                    maxLines: 2,
                    overflow: TextOverflow.ellipsis,
                  ),
            onTap: result.chatId == null
                ? null
                : () => context.go('/chats/${result.chatId}'),
          ),
      ],
    );
  }
}

/// One account, looked up by its exact handle.
class _ByUsername extends ConsumerWidget {
  const _ByUsername({required this.username});

  final String username;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return FutureBuilder<UserProfile>(
      future: ref.read(profileRepositoryProvider).byUsername(username),
      builder: (BuildContext context, AsyncSnapshot<UserProfile> snapshot) {
        if (snapshot.connectionState == ConnectionState.waiting) {
          return const SobhLoading();
        }
        // No such handle is the ordinary answer to a guess, not a failure:
        // most of what somebody types after an @ is not an account.
        if (snapshot.hasError || snapshot.data == null) {
          return SobhEmptyState(
            icon: Icons.person_off_outlined,
            title: l10n.searchNoSuchUsername(username),
          );
        }

        final UserProfile user = snapshot.data!;
        return ListView(
          children: <Widget>[
            ListTile(
              leading: SobhAvatar(name: user.displayName),
              title: Text(user.displayName),
              subtitle:
                  user.username == null ? null : Text('@${user.username}'),
              // Straight into a conversation with them, which is what
              // somebody typing an exact handle is after. There is no
              // stand-alone profile screen for another account, and inventing
              // one here would be a second place to maintain what the chat
              // header already shows.
              onTap: () async {
                final String chatId = await ref
                    .read(chatRepositoryProvider)
                    .openPrivateChat(user.userId);
                if (context.mounted) {
                  context.go('/chats/$chatId');
                }
              },
            ),
          ],
        );
      },
    );
  }
}
