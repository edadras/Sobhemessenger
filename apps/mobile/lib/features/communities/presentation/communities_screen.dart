import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/communities_repository.dart';

/// The communities the user belongs to (§16).
class CommunitiesScreen extends ConsumerWidget {
  const CommunitiesScreen({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Community>> communities = ref.watch(
      communityListProvider,
    );

    return Scaffold(
      appBar: AppBar(title: Text(l10n.communitiesTitle)),
      body: communities.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(communityListProvider),
        ),
        data: (List<Community> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.groups_outlined,
                title: l10n.communitiesEmpty,
              )
            : RefreshIndicator(
                onRefresh: () async => ref.invalidate(communityListProvider),
                child: ListView.separated(
                  itemCount: rows.length,
                  separatorBuilder: (_, __) => const Divider(height: 1),
                  itemBuilder: (BuildContext context, int index) {
                    final Community community = rows[index];
                    return ListTile(
                      leading: SobhAvatar(name: community.title),
                      title: Text(community.title),
                      subtitle: Text(l10n.groupsMembers(community.memberCount)),
                      onTap: () => Navigator.of(context).push(
                        MaterialPageRoute<void>(
                          builder: (_) =>
                              CommunityScreen(communityId: community.id),
                        ),
                      ),
                    );
                  },
                ),
              ),
      ),
    );
  }
}

/// One community and its rooms, grouped into the author's sections.
class CommunityScreen extends ConsumerWidget {
  const CommunityScreen({super.key, required this.communityId});

  final String communityId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final AsyncValue<Community> community = ref.watch(
      communityProvider(communityId),
    );

    return Scaffold(
      appBar: AppBar(
        title: Text(community.valueOrNull?.title ?? l10n.communitiesTitle),
      ),
      body: community.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(communityProvider(communityId)),
        ),
        data: (Community data) {
          if (data.rooms.isEmpty) {
            return SobhEmptyState(
              icon: Icons.meeting_room_outlined,
              title: l10n.communitiesRooms,
            );
          }

          return ListView(
            children: <Widget>[
              if (data.description.isNotEmpty)
                Padding(
                  padding: const EdgeInsets.all(SobhSpacing.lg),
                  child: Text(data.description),
                ),
              if (!data.isMember)
                Padding(
                  padding: const EdgeInsets.symmetric(
                    horizontal: SobhSpacing.lg,
                  ),
                  child: FilledButton(
                    onPressed: () async {
                      await ref
                          .read(communitiesRepositoryProvider)
                          .join(communityId);
                      ref
                        ..invalidate(communityProvider(communityId))
                        ..invalidate(communityListProvider);
                    },
                    child: Text(l10n.groupsJoin),
                  ),
                ),
              for (final MapEntry<String, List<CommunityRoom>> section
                  in data.roomsBySection.entries) ...<Widget>[
                Padding(
                  padding: const EdgeInsets.fromLTRB(
                    SobhSpacing.lg,
                    SobhSpacing.lg,
                    SobhSpacing.lg,
                    SobhSpacing.sm,
                  ),
                  child: Text(
                    section.key.isEmpty ? l10n.communitiesRooms : section.key,
                    style: Theme.of(
                      context,
                    )
                        .textTheme
                        .labelLarge
                        ?.copyWith(color: palette.textSecondary),
                  ),
                ),
                for (final CommunityRoom room in section.value)
                  ListTile(
                    leading: Icon(
                      room.chatType == 'channel'
                          ? Icons.campaign_outlined
                          : Icons.forum_outlined,
                    ),
                    title: Text(room.title),
                    subtitle: Text(
                      room.chatType == 'channel'
                          ? l10n.groupsSubscribers(room.memberCount)
                          : l10n.groupsMembers(room.memberCount),
                    ),
                    // A room the user has not joined opens nothing: joining
                    // the community is what grants access to its rooms.
                    onTap: room.isMember
                        ? () => context.go('/chats/${room.chatId}')
                        : null,
                    trailing: data.canArrange
                        ? IconButton(
                            icon: const Icon(Icons.playlist_remove),
                            tooltip: l10n.communitiesRemoveRoom,
                            onPressed: () => _removeRoom(
                              context,
                              ref,
                              communityId,
                              room,
                            ),
                          )
                        : null,
                  ),
              ],
            ],
          );
        },
      ),
    );
  }

  /// Unfiles a room from the community.
  ///
  /// Confirmed, and worded so it is clear the conversation survives: this
  /// takes the room out of the arrangement, it does not delete the chat or
  /// remove anybody from it.
  Future<void> _removeRoom(
    BuildContext context,
    WidgetRef ref,
    String communityId,
    CommunityRoom room,
  ) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.communitiesRemoveRoom),
            content: Text(l10n.communitiesRemoveRoomConfirm(room.title)),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.commonRemove),
              ),
            ],
          ),
        ) ??
        false;
    if (!confirmed || !context.mounted) {
      return;
    }

    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    try {
      await ref
          .read(communitiesRepositoryProvider)
          .removeRoom(communityId, room.chatId);
      ref.invalidate(communityProvider(communityId));
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}
