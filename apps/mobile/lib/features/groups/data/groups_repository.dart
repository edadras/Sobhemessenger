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
