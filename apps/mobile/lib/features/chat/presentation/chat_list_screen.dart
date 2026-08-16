import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/storage/local_database.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/websocket/socket_client.dart';
import '../../auth/session_controller.dart';
import '../data/chat_repository.dart';

/// The conversation list (§48).
class ChatListScreen extends ConsumerWidget {
  const ChatListScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<ChatRow>> chats = ref.watch(chatListProvider);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.navChats),
        bottom: const _ConnectionBanner(),
      ),
      body: chats.when(
        // The list streams from the local database, so the loading state only
        // appears on a genuinely cold start.
        loading: () => const Center(child: CircularProgressIndicator()),
        error: (Object error, StackTrace stack) => _ErrorState(message: l10n.errorGeneric),
        data: (List<ChatRow> rows) {
          if (rows.isEmpty) {
            return _EmptyState(title: l10n.chatsEmptyTitle, body: l10n.chatsEmptyBody);
          }
          return ListView.separated(
            itemCount: rows.length,
            separatorBuilder: (_, __) => const Divider(indent: SobhSpacing.xxl + SobhSpacing.lg),
            itemBuilder: (BuildContext context, int index) => _ChatTile(chat: rows[index]),
          );
        },
      ),
    );
  }
}

class _ChatTile extends StatelessWidget {
  const _ChatTile({required this.chat});

  final ChatRow chat;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);
    final TextTheme text = Theme.of(context).textTheme;

    return ListTile(
      onTap: () => context.go('/chats/${chat.id}'),
      leading: CircleAvatar(
        radius: SobhSizes.avatarMedium / 2,
        backgroundColor: palette.surfaceVariant,
        child: Text(
          chat.title.isEmpty ? '؟' : chat.title.characters.first,
          style: text.titleMedium,
        ),
      ),
      title: Text(chat.title, maxLines: 1, overflow: TextOverflow.ellipsis),
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
    final AsyncValue<SocketStatus> status =
        ref.watch(StreamProvider<SocketStatus>((Ref ref) => ref.watch(socketClientProvider).status));

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
            Icon(Icons.forum_outlined, size: SobhSizes.avatarLarge, color: palette.textDisabled),
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
