import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A community: rooms grouped into sections (§16).
class Community {
  const Community({
    required this.id,
    required this.title,
    required this.memberCount,
    required this.isPublic,
    this.description = '',
    this.username,
    this.role = '',
    this.rooms = const <CommunityRoom>[],
  });

  factory Community.fromJson(Map<String, dynamic> json) => Community(
        id: json['id'] as String,
        title: json['title'] as String? ?? '',
        memberCount: (json['member_count'] as num?)?.toInt() ?? 0,
        isPublic: json['is_public'] as bool? ?? false,
        description: json['description'] as String? ?? '',
        username: json['username'] as String?,
        role: json['role'] as String? ?? '',
        rooms: <CommunityRoom>[
          for (final dynamic room
              in json['rooms'] as List<dynamic>? ?? const <dynamic>[])
            CommunityRoom.fromJson(room as Map<String, dynamic>),
        ],
      );

  final String id;
  final String title;
  final int memberCount;
  final bool isPublic;
  final String description;
  final String? username;
  final String role;
  final List<CommunityRoom> rooms;

  bool get isMember => role.isNotEmpty;

  /// Whether this member may arrange the community's rooms. The server checks
  /// the same thing; this only decides whether the control is offered.
  bool get canArrange => role == 'owner' || role == 'admin';

  /// Rooms grouped by section, in the order the server returned them, so the
  /// author's arrangement survives round-tripping.
  Map<String, List<CommunityRoom>> get roomsBySection {
    final Map<String, List<CommunityRoom>> grouped =
        <String, List<CommunityRoom>>{};
    for (final CommunityRoom room in rooms) {
      grouped.putIfAbsent(room.section, () => <CommunityRoom>[]).add(room);
    }
    return grouped;
  }
}

class CommunityRoom {
  const CommunityRoom({
    required this.chatId,
    required this.chatType,
    required this.title,
    required this.section,
    required this.memberCount,
    required this.isMember,
  });

  factory CommunityRoom.fromJson(Map<String, dynamic> json) => CommunityRoom(
        chatId: json['chat_id'] as String,
        chatType: json['chat_type'] as String? ?? 'group',
        title: json['title'] as String? ?? '',
        section: json['section'] as String? ?? '',
        memberCount: (json['member_count'] as num?)?.toInt() ?? 0,
        isMember: json['is_member'] as bool? ?? false,
      );

  final String chatId;
  final String chatType;
  final String title;
  final String section;
  final int memberCount;
  final bool isMember;
}

class CommunitiesRepository {
  CommunitiesRepository(this._api);

  final ApiClient _api;

  Future<List<Community>> list() async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/communities',
    );
    return <Community>[
      for (final dynamic entry
          in data['communities'] as List<dynamic>? ?? const <dynamic>[])
        Community.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<Community> get(String communityId) async => Community.fromJson(
        await _api.get<Map<String, dynamic>>('/communities/$communityId'),
      );

  Future<Community> create({
    required String title,
    String description = '',
    bool isPublic = false,
  }) async =>
      Community.fromJson(
        await _api.post<Map<String, dynamic>>(
          '/communities',
          body: <String, dynamic>{
            'title': title,
            'description': description,
            'is_public': isPublic,
          },
        ),
      );

  /// Joins a community, which also joins its default rooms.
  Future<void> join(String communityId) =>
      _api.post<Map<String, dynamic>>('/communities/$communityId/join');

  /// Adds an existing group or channel to the community.
  Future<void> addRoom(
    String communityId,
    String chatId, {
    String section = 'general',
    int position = 0,
  }) =>
      _api.post<dynamic>(
        '/communities/$communityId/rooms',
        body: <String, dynamic>{
          'chat_id': chatId,
          'section': section,
          'position': position,
        },
      );

  /// Takes a room out of the community.
  ///
  /// The chat itself survives — this unfiles it, it does not delete it. A
  /// community is an arrangement of chats, so removing one from the
  /// arrangement should not destroy the conversation in it.
  Future<void> removeRoom(String communityId, String chatId) =>
      _api.delete<dynamic>('/communities/$communityId/rooms/$chatId');

  Future<void> leave(String communityId) =>
      _api.post<Map<String, dynamic>>('/communities/$communityId/leave');
}

final Provider<CommunitiesRepository> communitiesRepositoryProvider =
    Provider<CommunitiesRepository>(
  (Ref ref) => CommunitiesRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<Community>> communityListProvider =
    FutureProvider<List<Community>>(
  (Ref ref) => ref.watch(communitiesRepositoryProvider).list(),
);

final FutureProviderFamily<Community, String> communityProvider =
    FutureProvider.family<Community, String>(
  (Ref ref, String id) => ref.watch(communitiesRepositoryProvider).get(id),
);
