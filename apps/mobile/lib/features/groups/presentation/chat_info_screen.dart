import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/widgets/async_states.dart';
import '../data/groups_repository.dart';

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
          final bool canAdminister = rows.any((Member m) => m.isAdmin);

          return ListView(
            children: <Widget>[
              ListTile(
                leading: const Icon(Icons.group_outlined),
                title: Text(l10n.groupsMembers(rows.length)),
              ),
              if (canAdminister) ...<Widget>[
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
                      ? IconButton(
                          icon: const Icon(Icons.person_remove_outlined),
                          onPressed: () async {
                            await ref
                                .read(groupsRepositoryProvider)
                                .removeMember(chatId, member.userId);
                            ref.invalidate(chatMembersProvider(chatId));
                          },
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
