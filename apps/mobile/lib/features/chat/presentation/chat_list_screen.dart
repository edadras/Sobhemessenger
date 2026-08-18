import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/routing/app_router.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/websocket/socket_client.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../../secretchat/presentation/secret_chat_screen.dart';
import '../../stories/presentation/stories_tray.dart';
import '../data/chat_repository.dart';
import '../data/folders_repository.dart';
import 'folders_screen.dart';

/// The conversation list (§48).
class ChatListScreen extends ConsumerStatefulWidget {
  const ChatListScreen({super.key});

  @override
  ConsumerState<ChatListScreen> createState() => _ChatListScreenState();
}

class _ChatListScreenState extends ConsumerState<ChatListScreen> {
  @override
  void initState() {
    super.initState();
    // The list on screen streams from local storage, which is what makes it
    // work offline — but something has to put the conversations there in the
    // first place. Without this the list is permanently empty on a new install.
    unawaited(_sync());
  }

  /// Refreshes the list from the server.
  ///
  /// A failure is deliberately quiet when there is already a list to show: the
  /// cached conversations are still correct, and an error banner over them
  /// would suggest otherwise. It is only surfaced when the list is empty, where
  /// the difference between "no conversations" and "could not load them"
  /// matters.
  Future<void> _sync() async {
    try {
      await ref.read(chatRepositoryProvider).syncChats();
      if (mounted) {
        setState(() => _failure = null);
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(() => _failure = error);
      }
    }
  }

  ApiException? _failure;

  /// Narrows the streamed rows to the selected folder.
  ///
  /// While the folder's membership is still loading the full list is shown
  /// rather than an empty one: a list that blinks empty on every folder tap
  /// reads as "you have no chats", which is never true.
  List<ChatRow> _inSelectedFolder(List<ChatRow> rows) {
    final String? folderId = ref.watch(selectedFolderProvider);
    if (folderId == null) {
      return rows;
    }
    final Set<String>? ids =
        ref.watch(folderChatIdsProvider(folderId)).valueOrNull;
    if (ids == null) {
      return rows;
    }
    return <ChatRow>[
      for (final ChatRow row in rows)
        if (ids.contains(row.id)) row,
    ];
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ChatRow>> chats = ref.watch(chatListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.navChats),
        actions: <Widget>[
          IconButton(
            icon: const Icon(Icons.folder_outlined),
            tooltip: l10n.foldersManage,
            onPressed: () => Navigator.of(context).push(
              MaterialPageRoute<void>(builder: (_) => const FoldersScreen()),
            ),
          ),
          IconButton(
            icon: const Icon(Icons.search),
            tooltip: l10n.searchTitle,
            onPressed: () => context.push(Routes.search),
          ),
        ],
        bottom: const _ConnectionBanner(),
      ),
      body: Column(
        children: <Widget>[
          // Stories sit above the list and collapse entirely when there are
          // none, so they never cost the conversation list any height.
          const Padding(
            padding: EdgeInsets.symmetric(vertical: SobhSpacing.sm),
            child: StoriesTray(),
          ),
          // The folder strip collapses to nothing when there are no folders,
          // so somebody who has never made one sees the list they always saw.
          const _FolderTabs(),
          Expanded(
            child: chats.when(
              // The list streams from the local database, so the loading state
              // only appears on a genuinely cold start.
              loading: () => const Center(child: CircularProgressIndicator()),
              error: (Object error, StackTrace stack) =>
                  _ErrorState(message: l10n.errorGeneric),
              data: (List<ChatRow> allRows) {
                // A folder is a filter over the same list rather than a
                // different one, so it narrows the rows already on screen.
                final List<ChatRow> rows = _inSelectedFolder(allRows);
                if (rows.isEmpty) {
                  // With nothing cached, a failed fetch and a genuinely empty
                  // account look identical to the user unless they are told
                  // apart — and only one of them is worth retrying.
                  if (_failure != null) {
                    return SobhErrorState(
                      error: _failure!,
                      onRetry: _sync,
                    );
                  }
                  return _EmptyState(
                    title: l10n.chatsEmptyTitle,
                    body: l10n.chatsEmptyBody,
                  );
                }
                return RefreshIndicator(
                  onRefresh: _sync,
                  child: ListView.separated(
                    itemCount: rows.length,
                    separatorBuilder: (_, __) =>
                        const Divider(indent: SobhSpacing.xxl + SobhSpacing.lg),
                    itemBuilder: (BuildContext context, int index) =>
                        _ChatTile(chat: rows[index]),
                  ),
                );
              },
            ),
          ),
        ],
      ),
    );
  }
}

class _ChatTile extends StatelessWidget {
  const _ChatTile({required this.chat});

  final ChatRow chat;

  /// What to call this conversation.
  ///
  /// A private or secret chat has no title of its own — it is named after the
  /// person on the other side, which the server resolves because it differs for
  /// each of the two members. The question mark is the last resort for a peer
  /// whose account has been deleted.
  String get _name {
    if (chat.title.isNotEmpty) {
      return chat.title;
    }
    return chat.peerName?.isNotEmpty ?? false ? chat.peerName! : '';
  }

  bool get _isSecret => chat.type == 'secret';

  void _open(BuildContext context) {
    // An encrypted conversation must not open the ordinary screen. That screen
    // is built on the local message store and the plaintext send path — the
    // server now refuses both for a secret chat, so it would be an empty
    // conversation that errors on every send.
    if (_isSecret) {
      final String? peerUserId = chat.peerUserId;
      if (peerUserId == null) {
        return;
      }
      Navigator.of(context).push(
        MaterialPageRoute<void>(
          builder: (_) => SecretChatScreen(
            chatId: chat.id,
            peerUserId: peerUserId,
            peerName: _name,
          ),
        ),
      );
      return;
    }
    context.go('/chats/${chat.id}');
  }

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;
    final String name = _name;

    return ListTile(
      onTap: () => _open(context),
      leading: CircleAvatar(
        radius: SobhSizes.avatarMedium / 2,
        backgroundColor: palette.surfaceVariant,
        child: Text(
          name.isEmpty ? '؟' : name.characters.first,
          style: text.titleMedium,
        ),
      ),
      title: Row(
        children: <Widget>[
          if (_isSecret) ...<Widget>[
            // The lock is how someone tells the two conversations with the same
            // person apart at a glance, which decides what they are willing to
            // type into it.
            Icon(
              Icons.lock_outline,
              size: SobhSizes.iconSmall,
              color: palette.success,
            ),
            const SizedBox(width: SobhSpacing.xs),
          ],
          Expanded(
            child: Text(name, maxLines: 1, overflow: TextOverflow.ellipsis),
          ),
        ],
      ),
      subtitle: chat.draft.isNotEmpty
          ? Text(
              chat.draft,
              maxLines: 1,
              overflow: TextOverflow.ellipsis,
              style: text.bodySmall?.copyWith(color: palette.warning),
            )
          : null,
      trailing: chat.unreadCount > 0
          ? Container(
              padding: const EdgeInsets.symmetric(
                horizontal: SobhSpacing.sm,
                vertical: SobhSpacing.xxs,
              ),
              decoration: BoxDecoration(
                color: palette.unreadBadge,
                borderRadius: BorderRadius.circular(SobhRadius.round),
              ),
              child: Text(
                '${chat.unreadCount}',
                style: text.labelSmall?.copyWith(color: palette.onPrimary),
              ),
            )
          : null,
    );
  }
}

/// Shows connection state above the list so the user always knows whether they
/// are looking at live or cached data (§7).
class _ConnectionBanner extends ConsumerWidget implements PreferredSizeWidget {
  const _ConnectionBanner();

  @override
  Size get preferredSize => const Size.fromHeight(SobhSpacing.xl);

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<SocketStatus> status = ref.watch(
      StreamProvider<SocketStatus>(
        (Ref ref) => ref.watch(socketClientProvider).status,
      ),
    );

    final SocketStatus current = status.valueOrNull ?? SocketStatus.connecting;
    if (current == SocketStatus.connected) {
      return const SizedBox.shrink();
    }

    final String label = switch (current) {
      SocketStatus.connecting => l10n.statusConnecting,
      SocketStatus.syncing => l10n.statusSyncing,
      SocketStatus.waitingForNetwork => l10n.statusWaitingForNetwork,
      SocketStatus.disconnected => l10n.statusOffline,
      SocketStatus.connected => '',
    };

    return Container(
      width: double.infinity,
      color: palette.surfaceVariant,
      padding: const EdgeInsets.symmetric(vertical: SobhSpacing.xs),
      child: Text(
        label,
        textAlign: TextAlign.center,
        style: Theme.of(context).textTheme.labelSmall,
      ),
    );
  }
}

class _EmptyState extends StatelessWidget {
  const _EmptyState({required this.title, required this.body});

  final String title;
  final String body;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    return Center(
      child: Padding(
        padding: const EdgeInsets.all(SobhSpacing.xl),
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            Icon(
              Icons.forum_outlined,
              size: SobhSizes.avatarLarge,
              color: palette.textDisabled,
            ),
            const SizedBox(height: SobhSpacing.lg),
            Text(title, style: Theme.of(context).textTheme.titleMedium),
            const SizedBox(height: SobhSpacing.sm),
            Text(
              body,
              textAlign: TextAlign.center,
              style: Theme.of(context).textTheme.bodyMedium?.copyWith(
                    color: palette.textSecondary,
                  ),
            ),
          ],
        ),
      ),
    );
  }
}

class _ErrorState extends StatelessWidget {
  const _ErrorState({required this.message});

  final String message;

  @override
  Widget build(BuildContext context) => Center(
        child: Text(message, style: Theme.of(context).textTheme.bodyMedium),
      );
}

/// The folder tab strip (§12).
///
/// It is a strip of filters, not of inboxes: selecting one narrows the list
/// below, and "All" is the same list with nothing filtered out. It renders
/// nothing at all when there are no folders.
class _FolderTabs extends ConsumerWidget {
  const _FolderTabs();

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final List<ChatFolder> folders =
        ref.watch(chatFoldersProvider).valueOrNull ?? const <ChatFolder>[];
    if (folders.isEmpty) {
      return const SizedBox.shrink();
    }

    final String? selected = ref.watch(selectedFolderProvider);
    return SizedBox(
      height: SobhSizes.minTapTarget,
      child: ListView(
        scrollDirection: Axis.horizontal,
        padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.md),
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.only(right: SobhSpacing.sm),
            child: ChoiceChip(
              label: Text(l10n.foldersAll),
              selected: selected == null,
              onSelected: (_) =>
                  ref.read(selectedFolderProvider.notifier).state = null,
            ),
          ),
          for (final ChatFolder folder in folders)
            Padding(
              padding: const EdgeInsets.only(right: SobhSpacing.sm),
              child: ChoiceChip(
                label: Text(
                  folder.unreadCount > 0
                      ? '${folder.title} · ${folder.unreadCount}'
                      : folder.title,
                ),
                avatar: folder.emoji.isEmpty ? null : Text(folder.emoji),
                selected: selected == folder.id,
                onSelected: (_) =>
                    ref.read(selectedFolderProvider.notifier).state = folder.id,
              ),
            ),
        ],
      ),
    );
  }
}
