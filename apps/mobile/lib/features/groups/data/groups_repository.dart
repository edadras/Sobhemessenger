import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// The three kinds of conversation that are not one-to-one (§14, §15).
enum ChatKind {
  group('group'),
  channel('channel');

  const ChatKind(this.wire);

  final String wire;
}

/// One member of a group or channel.
class Member {
  const Member({
    required this.userId,
    required this.displayName,
    required this.role,
    this.username,
    this.customTitle = '',
  });

  factory Member.fromJson(Map<String, dynamic> json) => Member(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        role: json['role'] as String? ?? 'member',
        username: json['username'] as String?,
        customTitle: json['custom_title'] as String? ?? '',
      );

  final String userId;
  final String displayName;
  final String role;
  final String? username;
  final String customTitle;

  bool get isOwner => role == 'owner';
  bool get isAdmin => role == 'admin' || isOwner;
}

/// A public group or channel in the directory.
class DiscoverableChat {
  const DiscoverableChat({
    required this.chatId,
    required this.title,
    required this.type,
    required this.memberCount,
    this.username,
    this.description = '',
  });

  factory DiscoverableChat.fromJson(Map<String, dynamic> json) =>
      DiscoverableChat(
        chatId: json['chat_id'] as String? ?? json['id'] as String,
        title: json['title'] as String? ?? '',
        type: json['type'] as String? ?? 'group',
        memberCount: (json['member_count'] as num?)?.toInt() ?? 0,
        username: json['username'] as String?,
        description: json['description'] as String? ?? '',
      );

  final String chatId;
  final String title;
  final String type;
  final int memberCount;
  final String? username;
  final String description;

  bool get isChannel => type == 'channel';
}

/// An invite link (§16).
class InviteLink {
  const InviteLink({
    required this.id,
    required this.slug,
    required this.url,
    required this.usageCount,
    this.name = '',
    this.memberLimit,
    this.expiresAt,
  });

  factory InviteLink.fromJson(Map<String, dynamic> json) => InviteLink(
        id: json['id'] as String,
        slug: json['slug'] as String,
        url: json['url'] as String? ?? '',
        usageCount: (json['usage_count'] as num?)?.toInt() ?? 0,
        name: json['name'] as String? ?? '',
        memberLimit: (json['member_limit'] as num?)?.toInt(),
        expiresAt: json['expires_at'] == null
            ? null
            : DateTime.parse(json['expires_at'] as String).toLocal(),
      );

  final String id;
  final String slug;
  final String url;
  final int usageCount;
  final String name;
  final int? memberLimit;
  final DateTime? expiresAt;
}

/// Someone waiting to be let into a group whose joins need approval.
class JoinRequest {
  const JoinRequest({
    required this.userId,
    required this.displayName,
    this.username,
  });

  factory JoinRequest.fromJson(Map<String, dynamic> json) => JoinRequest(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        username: json['username'] as String?,
      );

  final String userId;
  final String displayName;
  final String? username;
}

/// Groups, channels and their administration (§14–§16).
class GroupsRepository {
  GroupsRepository(this._api);

  final ApiClient _api;

  Future<String> create({
    required ChatKind kind,
    required String title,
    String description = '',
    bool isPublic = false,
    String? username,
    List<String> memberIds = const <String>[],
  }) async {
    final Map<String, dynamic> result = await _api.post<Map<String, dynamic>>(
      '/chats',
      body: <String, dynamic>{
        'type': kind.wire,
        'title': title,
        'description': description,
        'is_public': isPublic,
        if (username != null && username.isNotEmpty) 'username': username,
        'member_ids': memberIds,
      },
    );
    return result['chat_id'] as String;
  }

  Future<List<DiscoverableChat>> discover({
    String query = '',
    String? type,
  }) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats/discover',
      query: <String, dynamic>{
        if (query.isNotEmpty) 'q': query,
        if (type != null) 'type': type,
      },
    );
    return <DiscoverableChat>[
      for (final dynamic entry
          in data['chats'] as List<dynamic>? ?? const <dynamic>[])
        DiscoverableChat.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<List<Member>> members(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/members');
    return <Member>[
      for (final dynamic entry
          in data['members'] as List<dynamic>? ?? const <dynamic>[])
        Member.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<void> addMembers(String chatId, List<String> userIds) =>
      _api.post<Map<String, dynamic>>(
        '/chats/$chatId/members',
        body: <String, dynamic>{'user_ids': userIds},
      );

  Future<void> removeMember(String chatId, String userId) =>
      _api.delete<Map<String, dynamic>>('/chats/$chatId/members/$userId');

  Future<void> setRole(String chatId, String userId, String role) =>
      _api.put<Map<String, dynamic>>(
        '/chats/$chatId/members/$userId/role',
        body: <String, dynamic>{'role': role},
      );

  Future<void> leave(String chatId) =>
      _api.post<Map<String, dynamic>>('/chats/$chatId/leave');

  /// Joins a public chat. Returns false when the join needs approval first.
  Future<bool> join(String chatId) async {
    final Map<String, dynamic> result =
        await _api.post<Map<String, dynamic>>('/chats/$chatId/join');
    return result['joined'] as bool? ?? false;
  }

  Future<List<InviteLink>> inviteLinks(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/invite-links');
    return <InviteLink>[
      for (final dynamic entry
          in data['links'] as List<dynamic>? ?? const <dynamic>[])
        InviteLink.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<InviteLink> createInviteLink(
    String chatId, {
    String name = '',
    int? memberLimit,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$chatId/invite-links',
      body: <String, dynamic>{
        'name': name,
        if (memberLimit != null) 'member_limit': memberLimit,
      },
    );
    return InviteLink.fromJson(data);
  }

  Future<void> revokeInviteLink(String chatId, String linkId) =>
      _api.delete<Map<String, dynamic>>('/chats/$chatId/invite-links/$linkId');

  Future<List<JoinRequest>> joinRequests(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/join-requests');
    return <JoinRequest>[
      for (final dynamic entry
          in data['requests'] as List<dynamic>? ?? const <dynamic>[])
        JoinRequest.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<void> resolveJoinRequest(
    String chatId,
    String userId, {
    required bool approve,
  }) =>
      _api.post<Map<String, dynamic>>(
        '/chats/$chatId/join-requests/$userId',
        body: <String, dynamic>{'approve': approve},
      );

  Future<ChatSettings> settings(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/settings');
    return ChatSettings.fromJson(data);
  }

  Future<void> updateSettings(String chatId, ChatSettings settings) =>
      _api.put<dynamic>('/chats/$chatId/settings', body: settings.toJson());

  /// Hands the chat to someone else.
  ///
  /// This is the one action that cannot be undone from the app: the caller
  /// stops being the owner the moment it succeeds, so the screen that offers
  /// it asks twice.
  Future<void> transferOwnership(String chatId, String userId) =>
      _api.post<dynamic>(
        '/chats/$chatId/transfer-ownership',
        body: <String, dynamic>{'user_id': userId},
      );

  /// Reach figures for specific channel posts.
  ///
  /// The server answers per message id rather than for the channel as a whole,
  /// so the caller passes the posts it is showing. Asking with no ids returns
  /// nothing, which is why the screen never calls this with an empty list.
  Future<List<PostStatistics>> postStatistics(
    String chatId,
    List<String> messageIds,
  ) async {
    if (messageIds.isEmpty) {
      return const <PostStatistics>[];
    }
    final String query =
        messageIds.map((String id) => 'message_id=$id').join('&');
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/statistics?$query');
    return <PostStatistics>[
      for (final dynamic entry
          in data['statistics'] as List<dynamic>? ?? const <dynamic>[])
        PostStatistics.fromJson(entry as Map<String, dynamic>),
    ];
  }

  // ------------------------------------------------------------ role bundles

  Future<List<GroupRole>> roles(String chatId) async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/$chatId/roles');
    return <GroupRole>[
      for (final dynamic entry
          in data['roles'] as List<dynamic>? ?? const <dynamic>[])
        GroupRole.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<GroupRole> createRole(
    String chatId, {
    required String name,
    required Map<String, bool> permissions,
    int rank = 0,
  }) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/$chatId/roles',
      body: <String, dynamic>{
        'name': name,
        'permissions': permissions,
        'rank': rank,
      },
    );
    return GroupRole.fromJson(data['role'] as Map<String, dynamic>);
  }

  Future<GroupRole> updateRole(
    String chatId,
    String roleId, {
    required String name,
    required Map<String, bool> permissions,
    int rank = 0,
  }) async {
    final Map<String, dynamic> data = await _api.put<Map<String, dynamic>>(
      '/chats/$chatId/roles/$roleId',
      body: <String, dynamic>{
        'name': name,
        'permissions': permissions,
        'rank': rank,
      },
    );
    return GroupRole.fromJson(data['role'] as Map<String, dynamic>);
  }

  Future<void> deleteRole(String chatId, String roleId) =>
      _api.delete<dynamic>('/chats/$chatId/roles/$roleId');

  /// Gives a member a named role, or takes it away when [roleId] is null.
  Future<void> assignRole(String chatId, String userId, String? roleId) =>
      _api.put<dynamic>(
        '/chats/$chatId/members/$userId/role-bundle',
        body: <String, dynamic>{'role_id': roleId},
      );
}

/// Reach figures for one channel post (§15).
class PostStatistics {
  const PostStatistics({
    required this.messageId,
    required this.viewCount,
    required this.forwardCount,
    required this.reactionCount,
    required this.commentCount,
  });

  factory PostStatistics.fromJson(Map<String, dynamic> json) => PostStatistics(
        messageId: json['message_id'] as String,
        viewCount: (json['view_count'] as num?)?.toInt() ?? 0,
        forwardCount: (json['forward_count'] as num?)?.toInt() ?? 0,
        reactionCount: (json['reaction_count'] as num?)?.toInt() ?? 0,
        commentCount: (json['comment_count'] as num?)?.toInt() ?? 0,
      );

  final String messageId;
  final int viewCount;
  final int forwardCount;
  final int reactionCount;
  final int commentCount;
}

/// A named bundle of permissions that members can be given.
///
/// It sits between the chat-wide defaults and a member's own overrides, so
/// "moderator" can be defined once rather than re-entered for every person who
/// holds it.
class GroupRole {
  const GroupRole({
    required this.id,
    required this.name,
    required this.permissions,
    this.rank = 0,
    this.memberCount = 0,
  });

  factory GroupRole.fromJson(Map<String, dynamic> json) => GroupRole(
        id: json['id'] as String,
        name: json['name'] as String? ?? '',
        permissions: <String, bool>{
          for (final MapEntry<String, dynamic> entry
              in (json['permissions'] as Map<String, dynamic>? ??
                      <String, dynamic>{})
                  .entries)
            entry.key: entry.value as bool? ?? false,
        },
        rank: (json['rank'] as num?)?.toInt() ?? 0,
        memberCount: (json['member_count'] as num?)?.toInt() ?? 0,
      );

  final String id;
  final String name;
  final Map<String, bool> permissions;
  final int rank;
  final int memberCount;
}

/// The administrative knobs on a group or channel (§14, §15).
///
/// The type-specific fields are nullable and omitted when absent, which is
/// what makes an update that never mentions them leave them alone. A whole
/// object that sent `false` for a setting it had never heard of would switch
/// off commenting on every channel it touched.
class ChatSettings {
  const ChatSettings({
    this.slowModeSeconds = 0,
    this.historyVisibleToNew = true,
    this.joinRequiresApproval = false,
    this.maxMembers = 200000,
    this.autoDeleteSeconds = 0,
    this.signatureEnabled,
    this.commentsEnabled,
    this.discussionChatId,
    this.stickerSet,
    this.linkedChannelId,
    this.isForum,
    this.isBroadcast,
  });

  factory ChatSettings.fromJson(Map<String, dynamic> json) => ChatSettings(
        slowModeSeconds: (json['slow_mode_seconds'] as num?)?.toInt() ?? 0,
        historyVisibleToNew: json['history_visible_to_new'] as bool? ?? true,
        joinRequiresApproval: json['join_requires_approval'] as bool? ?? false,
        maxMembers: (json['max_members'] as num?)?.toInt() ?? 200000,
        autoDeleteSeconds: (json['auto_delete_seconds'] as num?)?.toInt() ?? 0,
        signatureEnabled: json['signature_enabled'] as bool?,
        commentsEnabled: json['comments_enabled'] as bool?,
        discussionChatId: json['discussion_chat_id'] as String?,
        stickerSet: json['sticker_set'] as String?,
        linkedChannelId: json['linked_channel_id'] as String?,
        isForum: json['is_forum'] as bool?,
        isBroadcast: json['is_broadcast'] as bool?,
      );

  final int slowModeSeconds;
  final bool historyVisibleToNew;
  final bool joinRequiresApproval;
  final int maxMembers;
  final int autoDeleteSeconds;

  /// Channels only: put the posting admin's name on each post.
  final bool? signatureEnabled;

  /// Channels only: whether readers may comment, which only does anything
  /// once a discussion group is linked.
  final bool? commentsEnabled;

  /// Channels only, read-only. Linking has its own endpoint because it needs
  /// authority over both chats.
  final String? discussionChatId;

  /// Groups only: the slug of the group's own sticker set.
  final String? stickerSet;

  /// Groups only, read-only.
  final String? linkedChannelId;
  final bool? isForum;

  /// Groups only: only staff may post.
  final bool? isBroadcast;

  Map<String, dynamic> toJson() => <String, dynamic>{
        'slow_mode_seconds': slowModeSeconds,
        'history_visible_to_new': historyVisibleToNew,
        'join_requires_approval': joinRequiresApproval,
        'max_members': maxMembers,
        'auto_delete_seconds': autoDeleteSeconds,
        if (signatureEnabled != null) 'signature_enabled': signatureEnabled,
        if (commentsEnabled != null) 'comments_enabled': commentsEnabled,
        if (stickerSet != null) 'sticker_set': stickerSet,
        if (isBroadcast != null) 'is_broadcast': isBroadcast,
      };

  ChatSettings copyWith({
    bool? signatureEnabled,
    bool? commentsEnabled,
    String? stickerSet,
    bool? isBroadcast,
  }) =>
      ChatSettings(
        slowModeSeconds: slowModeSeconds,
        historyVisibleToNew: historyVisibleToNew,
        joinRequiresApproval: joinRequiresApproval,
        maxMembers: maxMembers,
        autoDeleteSeconds: autoDeleteSeconds,
        signatureEnabled: signatureEnabled ?? this.signatureEnabled,
        commentsEnabled: commentsEnabled ?? this.commentsEnabled,
        discussionChatId: discussionChatId,
        stickerSet: stickerSet ?? this.stickerSet,
        linkedChannelId: linkedChannelId,
        isForum: isForum,
        isBroadcast: isBroadcast ?? this.isBroadcast,
      );
}

final Provider<GroupsRepository> groupsRepositoryProvider =
    Provider<GroupsRepository>(
  (Ref ref) => GroupsRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<List<Member>, String> chatMembersProvider =
    FutureProvider.family<List<Member>, String>(
  (Ref ref, String chatId) =>
      ref.watch(groupsRepositoryProvider).members(chatId),
);

final FutureProviderFamily<List<DiscoverableChat>, String> discoverProvider =
    FutureProvider.family<List<DiscoverableChat>, String>(
  (Ref ref, String query) =>
      ref.watch(groupsRepositoryProvider).discover(query: query),
);

final FutureProviderFamily<List<JoinRequest>, String> joinRequestsProvider =
    FutureProvider.family<List<JoinRequest>, String>(
  (Ref ref, String chatId) =>
      ref.watch(groupsRepositoryProvider).joinRequests(chatId),
);

final FutureProviderFamily<ChatSettings, String> chatSettingsProvider =
    FutureProvider.family<ChatSettings, String>(
  (Ref ref, String chatId) =>
      ref.watch(groupsRepositoryProvider).settings(chatId),
);

final FutureProviderFamily<List<GroupRole>, String> chatRolesProvider =
    FutureProvider.family<List<GroupRole>, String>(
  (Ref ref, String chatId) =>
      ref.watch(groupsRepositoryProvider).roles(chatId),
);
