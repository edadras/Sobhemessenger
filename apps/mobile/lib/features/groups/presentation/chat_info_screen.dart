import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/widgets/async_states.dart';
import '../../auth/session_controller.dart';
import '../../chat/data/chat_repository.dart';
import '../../chat/data/organise_repository.dart';
import '../../chat/data/topics_repository.dart';
import '../../chat/presentation/topics_screen.dart';
import '../data/groups_repository.dart';
import 'channel_statistics_screen.dart';
import 'chat_settings_screen.dart';
import 'role_bundles_screen.dart';

/// Members, invite links and join requests for one group or channel (§16).
class ChatInfoScreen extends ConsumerWidget {
  const ChatInfoScreen({super.key, required this.chatId, this.title = ''});

  final String chatId;
  final String title;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<Member>> members =
        ref.watch(chatMembersProvider(chatId));
    final String? myUserId = ref.watch(sessionControllerProvider).userId;
    final String chatType =
        ref.watch(chatRowProvider(chatId)).valueOrNull?.type ?? 'group';

    return Scaffold(
      appBar: AppBar(title: Text(title.isEmpty ? l10n.groupsInfoTitle : title)),
      body: members.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(chatMembersProvider(chatId)),
        ),
        data: (List<Member> rows) {
          // Administration is only offered to someone who can actually perform
          // it; the server enforces the same rule, this just avoids showing
          // buttons that would be refused.
          //
          // It has to be *this* member's role. Asking whether the list
          // contains any admin is a question with the answer "yes" in every
          // group, which offered every member the administrator's controls and
          // left the refusal to arrive as an error afterwards.
          final Member? me = rows
              .where((Member m) => m.userId == myUserId)
              .firstOrNull;
          final bool canAdminister = me?.isAdmin ?? false;
          final bool isOwner = me?.isOwner ?? false;

          return ListView(
            children: <Widget>[
              ListTile(
                leading: const Icon(Icons.group_outlined),
                title: Text(l10n.groupsMembers(rows.length)),
              ),
              ListTile(
                leading: const Icon(Icons.forum_outlined),
                title: Text(l10n.topicsTitle),
                onTap: () => Navigator.of(context).push(
                  MaterialPageRoute<void>(
                    builder: (_) => TopicsScreen(
                      chatId: chatId,
                      canModerate: canAdminister,
                    ),
                  ),
                ),
              ),
              ListTile(
                leading: const Icon(Icons.cleaning_services_outlined),
                title: Text(l10n.chatClearHistory),
                onTap: () => _clearHistory(context, ref, chatId, canAdminister),
              ),
              if (canAdminister) ...<Widget>[
                ListTile(
                  leading: const Icon(Icons.dynamic_feed_outlined),
                  title: Text(l10n.topicsEnable),
                  subtitle: Text(l10n.topicsEnableBody),
                  onTap: () => _enableForum(context, ref, chatId),
                ),
                ListTile(
                  leading: const Icon(Icons.tune),
                  title: Text(l10n.settingsTitle),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => ChatSettingsScreen(chatId: chatId),
                    ),
                  ),
                ),
                ListTile(
                  leading: const Icon(Icons.badge_outlined),
                  title: Text(l10n.rolesTitle),
                  subtitle: Text(l10n.rolesSubtitle),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => RoleBundlesScreen(
                        chatId: chatId,
                        chatType: chatType,
                      ),
                    ),
                  ),
                ),
                if (chatType == 'channel')
                  ListTile(
                    leading: const Icon(Icons.query_stats),
                    title: Text(l10n.channelStatisticsTitle),
                    onTap: () => Navigator.of(context).push(
                      MaterialPageRoute<void>(
                        builder: (_) => ChannelStatisticsScreen(
                          chatId: chatId,
                        ),
                      ),
                    ),
                  ),
                ListTile(
                  leading: const Icon(Icons.link),
                  title: Text(l10n.groupsInviteLink),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => InviteLinksScreen(chatId: chatId),
                    ),
                  ),
                ),
                ListTile(
                  leading: const Icon(Icons.how_to_reg_outlined),
                  title: Text(l10n.groupsJoinRequests),
                  onTap: () => Navigator.of(context).push(
                    MaterialPageRoute<void>(
                      builder: (_) => JoinRequestsScreen(chatId: chatId),
                    ),
                  ),
                ),
              ],
              const Divider(),
              for (final Member member in rows)
                ListTile(
                  leading: SobhAvatar(name: member.displayName),
                  title: Text(member.displayName),
                  subtitle: Text(
                    switch (member.role) {
                      'owner' => l10n.groupsRoleOwner,
                      'admin' => l10n.groupsRoleAdmin,
                      _ => l10n.groupsRoleMember,
                    },
                  ),
                  trailing: canAdminister && !member.isOwner
                      ? _MemberActions(
                          chatId: chatId,
                          chatType: chatType,
                          member: member,
                          canTransferOwnership: isOwner,
                        )
                      : null,
                ),
              const Divider(),
              ListTile(
                leading: Icon(Icons.logout, color: SobhTheme.of(context).error),
                title: Text(
                  l10n.groupsLeave,
                  style: TextStyle(color: SobhTheme.of(context).error),
                ),
                onTap: () => _confirmLeave(context, ref, l10n),
              ),
            ],
          );
        },
      ),
    );
  }

  Future<void> _confirmLeave(
    BuildContext context,
    WidgetRef ref,
    AppLocalizations l10n,
  ) async {
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            content: Text(l10n.groupsLeaveConfirm),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.groupsLeave),
              ),
            ],
          ),
        ) ??
        false;

    if (!confirmed || !context.mounted) {
      return;
    }
    await ref.read(groupsRepositoryProvider).leave(chatId);
    if (context.mounted) {
      context.go('/chats');
    }
  }
}

/// What an administrator can do to one member.
///
/// Collapsed into a menu rather than a row of icons: removing someone and
/// handing them the chat are both one tap away otherwise, and they are not
/// mistakes anyone recovers from quickly.
class _MemberActions extends ConsumerWidget {
  const _MemberActions({
    required this.chatId,
    required this.chatType,
    required this.member,
    required this.canTransferOwnership,
  });

  final String chatId;
  final String chatType;
  final Member member;

  /// Only the current owner may hand the chat over, so only they are offered
  /// it. The server checks the same thing.
  final bool canTransferOwnership;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return PopupMenuButton<String>(
      onSelected: (String action) => switch (action) {
        'remove' => _remove(context, ref),
        'bundle' => _assignBundle(context, ref),
        _ => _transfer(context, ref),
      },
      itemBuilder: (BuildContext context) => <PopupMenuEntry<String>>[
        PopupMenuItem<String>(
          value: 'bundle',
          child: ListTile(
            leading: const Icon(Icons.badge_outlined),
            title: Text(l10n.rolesAssign),
          ),
        ),
        PopupMenuItem<String>(
          value: 'remove',
          child: ListTile(
            leading: const Icon(Icons.person_remove_outlined),
            title: Text(l10n.groupsRemoveMember),
          ),
        ),
        if (canTransferOwnership)
          PopupMenuItem<String>(
            value: 'transfer',
            child: ListTile(
              leading: const Icon(Icons.workspace_premium_outlined),
              title: Text(l10n.groupsTransferOwnership),
            ),
          ),
      ],
    );
  }

  Future<void> _remove(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);
    try {
      await ref
          .read(groupsRepositoryProvider)
          .removeMember(chatId, member.userId);
      ref.invalidate(chatMembersProvider(chatId));
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }

  /// Gives this member one of the chat's named bundles, or takes it away.
  Future<void> _assignBundle(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final ScaffoldMessengerState messenger = ScaffoldMessenger.of(context);

    final List<GroupRole> roles;
    try {
      roles = await ref.read(groupsRepositoryProvider).roles(chatId);
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
      return;
    }
    if (!context.mounted) {
      return;
    }

    if (roles.isEmpty) {
      // Offering an empty picker would look broken. Send them where bundles
      // are made instead.
      await Navigator.of(context).push(
        MaterialPageRoute<void>(
          builder: (_) => RoleBundlesScreen(chatId: chatId, chatType: chatType),
        ),
      );
      return;
    }

    // The sentinel stands for "no bundle": null cannot be returned from the
    // sheet and also mean "the user backed out".
    const String none = '';
    final String? chosen = await showModalBottomSheet<String>(
      context: context,
      builder: (BuildContext context) => SafeArea(
        child: ListView(
          shrinkWrap: true,
          children: <Widget>[
            ListTile(
              leading: const Icon(Icons.block),
              title: Text(l10n.rolesNone),
              onTap: () => Navigator.of(context).pop(none),
            ),
            const Divider(height: 1),
            for (final GroupRole role in roles)
              ListTile(
                leading: const Icon(Icons.badge_outlined),
                title: Text(role.name),
                onTap: () => Navigator.of(context).pop(role.id),
              ),
          ],
        ),
      ),
    );
    if (chosen == null) {
      return;
    }

    try {
      await ref.read(groupsRepositoryProvider).assignRole(
            chatId,
            member.userId,
            chosen == none ? null : chosen,
          );
      ref.invalidate(chatMembersProvider(chatId));
      ref.invalidate(chatRolesProvider(chatId));
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }

  /// Hands the chat over. Confirmed by typing nothing clever — just a plain
  /// dialog naming who is about to own it, because the caller loses their own
  /// authority the moment it succeeds and cannot undo it from here.
  Future<void> _transfer(BuildContext context, WidgetRef ref) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text(l10n.groupsTransferOwnership),
            content: Text(
              l10n.groupsTransferOwnershipConfirm(member.displayName),
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: Text(l10n.commonCancel),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: Text(l10n.groupsTransferOwnershipConfirmAction),
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
          .read(groupsRepositoryProvider)
          .transferOwnership(chatId, member.userId);
      ref.invalidate(chatMembersProvider(chatId));
      messenger.showSnackBar(
        SnackBar(content: Text(l10n.groupsTransferOwnershipDone)),
      );
    } on ApiException catch (error) {
      messenger.showSnackBar(
        SnackBar(
          content: Text(error.isOffline ? l10n.errorNetwork : error.message),
        ),
      );
    }
  }
}

/// Invite links, with their usage counts (§16).
class InviteLinksScreen extends ConsumerStatefulWidget {
  const InviteLinksScreen({super.key, required this.chatId});

  final String chatId;

  @override
  ConsumerState<InviteLinksScreen> createState() => _InviteLinksScreenState();
}

class _InviteLinksScreenState extends ConsumerState<InviteLinksScreen> {
  late Future<List<InviteLink>> _links = _load();

  Future<List<InviteLink>> _load() =>
      ref.read(groupsRepositoryProvider).inviteLinks(widget.chatId);

  void _reload() => setState(() => _links = _load());

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.groupsInviteLink)),
      floatingActionButton: FloatingActionButton(
        onPressed: () async {
          await ref
              .read(groupsRepositoryProvider)
              .createInviteLink(widget.chatId);
          _reload();
        },
        child: const Icon(Icons.add_link),
      ),
      body: FutureBuilder<List<InviteLink>>(
        future: _links,
        builder:
            (BuildContext context, AsyncSnapshot<List<InviteLink>> snapshot) {
          if (snapshot.connectionState != ConnectionState.done) {
            return const SobhLoading();
          }
          if (snapshot.hasError) {
            return SobhErrorState(error: snapshot.error!, onRetry: _reload);
          }

          final List<InviteLink> links = snapshot.data ?? const <InviteLink>[];
          if (links.isEmpty) {
            return SobhEmptyState(
              icon: Icons.link_off,
              title: l10n.groupsInviteLink,
            );
          }

          return ListView.separated(
            itemCount: links.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) {
              final InviteLink link = links[index];
              return ListTile(
                title: Text(link.url.isEmpty ? link.slug : link.url),
                subtitle: Text(l10n.groupsMembers(link.usageCount)),
                trailing: IconButton(
                  icon: const Icon(Icons.delete_outline),
                  onPressed: () async {
                    await ref
                        .read(groupsRepositoryProvider)
                        .revokeInviteLink(widget.chatId, link.id);
                    _reload();
                  },
                ),
              );
            },
          );
        },
      ),
    );
  }
}

/// People waiting to be admitted (§16).
class JoinRequestsScreen extends ConsumerWidget {
  const JoinRequestsScreen({super.key, required this.chatId});

  final String chatId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<JoinRequest>> requests =
        ref.watch(joinRequestsProvider(chatId));

    Future<void> resolve(String userId, {required bool approve}) async {
      await ref
          .read(groupsRepositoryProvider)
          .resolveJoinRequest(chatId, userId, approve: approve);
      ref
        ..invalidate(joinRequestsProvider(chatId))
        ..invalidate(chatMembersProvider(chatId));
    }

    return Scaffold(
      appBar: AppBar(title: Text(l10n.groupsJoinRequests)),
      body: requests.when(
        loading: () => const SobhLoading(),
        error: (Object error, StackTrace _) => SobhErrorState(
          error: error,
          onRetry: () => ref.invalidate(joinRequestsProvider(chatId)),
        ),
        data: (List<JoinRequest> rows) => rows.isEmpty
            ? SobhEmptyState(
                icon: Icons.how_to_reg_outlined,
                title: l10n.groupsJoinRequests,
              )
            : ListView.separated(
                itemCount: rows.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final JoinRequest request = rows[index];
                  return ListTile(
                    leading: SobhAvatar(name: request.displayName),
                    title: Text(request.displayName),
                    subtitle: request.username == null
                        ? null
                        : Text('@${request.username}'),
                    trailing: Row(
                      mainAxisSize: MainAxisSize.min,
                      children: <Widget>[
                        TextButton(
                          onPressed: () =>
                              resolve(request.userId, approve: false),
                          child: Text(l10n.groupsDecline),
                        ),
                        FilledButton(
                          onPressed: () =>
                              resolve(request.userId, approve: true),
                          child: Text(l10n.groupsApprove),
                        ),
                      ],
                    ),
                  );
                },
              ),
      ),
    );
  }
}

/// Clearing a conversation's history from the info screen.
///
/// The default is one-sided and always permitted: what you keep in your own
/// copy is yours to discard. Clearing for everyone is offered only to someone
/// who could delete the messages one at a time anyway, and the server checks
/// the same thing.
Future<void> _clearHistory(
  BuildContext context,
  WidgetRef ref,
  String chatId,
  bool canDeleteForEveryone,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  bool forEveryone = false;
  final bool? confirmed = await showDialog<bool>(
    context: context,
    builder: (BuildContext context) => StatefulBuilder(
      builder: (BuildContext context, StateSetter setState) => AlertDialog(
        title: Text(l10n.chatClearHistory),
        content: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: <Widget>[
            Text(l10n.chatClearHistoryBody),
            if (canDeleteForEveryone)
              CheckboxListTile(
                contentPadding: EdgeInsets.zero,
                value: forEveryone,
                title: Text(l10n.chatClearForEveryone),
                onChanged: (bool? value) =>
                    setState(() => forEveryone = value ?? false),
              ),
          ],
        ),
        actions: <Widget>[
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: Text(l10n.commonCancel),
          ),
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: Text(l10n.chatClearHistory),
          ),
        ],
      ),
    ),
  );
  if (confirmed != true) {
    return;
  }

  try {
    await ref
        .read(organiseRepositoryProvider)
        .clearHistory(chatId, forEveryone: forEveryone);
    if (context.mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(l10n.chatHistoryCleared)));
    }
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(error.message)));
    }
  }
}

/// Turning a group into a forum.
///
/// It is confirmed because it changes how every message in the group is
/// addressed: the existing history is filed under General and new messages
/// have to name a topic or land there.
Future<void> _enableForum(
  BuildContext context,
  WidgetRef ref,
  String chatId,
) async {
  final AppLocalizations l10n = AppLocalizations.of(context);
  final bool? confirmed = await showDialog<bool>(
    context: context,
    builder: (BuildContext context) => AlertDialog(
      title: Text(l10n.topicsEnable),
      content: Text(l10n.topicsEnableBody),
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(context).pop(false),
          child: Text(l10n.commonCancel),
        ),
        FilledButton(
          onPressed: () => Navigator.of(context).pop(true),
          child: Text(l10n.commonContinue),
        ),
      ],
    ),
  );
  if (confirmed != true) {
    return;
  }

  try {
    await ref.read(topicsRepositoryProvider).enableForum(chatId);
    ref.invalidate(forumTopicsProvider(chatId));
    if (context.mounted) {
      await Navigator.of(context).push(
        MaterialPageRoute<void>(
          builder: (_) => TopicsScreen(chatId: chatId, canModerate: true),
        ),
      );
    }
  } on ApiException catch (error) {
    if (context.mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(error.message)));
    }
  }
}
