import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// One chat folder (§12).
///
/// A folder is a saved filter, not a container: a chat can appear in several,
/// and filing one does not take it out of the main list. The rule flags admit
/// whole categories; [includedChatIds] and [excludedChatIds] then pin
/// individual chats in or keep them out — the second being the only way to say
/// "all my groups except that one".
class ChatFolder {
  const ChatFolder({
    required this.id,
    required this.title,
    this.emoji = '',
    this.position = 0,
    this.includeContacts = false,
    this.includeNonContacts = false,
    this.includeGroups = false,
    this.includeChannels = false,
    this.includeBots = false,
    this.excludeMuted = false,
    this.excludeRead = false,
    this.excludeArchived = true,
    this.includedChatIds = const <String>[],
    this.excludedChatIds = const <String>[],
    this.unreadCount = 0,
    this.chatCount = 0,
  });

  factory ChatFolder.fromJson(Map<String, dynamic> json) => ChatFolder(
        id: json['id'] as String,
        title: json['title'] as String? ?? '',
        emoji: json['emoji'] as String? ?? '',
        position: (json['position'] as num?)?.toInt() ?? 0,
        includeContacts: json['include_contacts'] as bool? ?? false,
        includeNonContacts: json['include_non_contacts'] as bool? ?? false,
        includeGroups: json['include_groups'] as bool? ?? false,
        includeChannels: json['include_channels'] as bool? ?? false,
        includeBots: json['include_bots'] as bool? ?? false,
        excludeMuted: json['exclude_muted'] as bool? ?? false,
        excludeRead: json['exclude_read'] as bool? ?? false,
        excludeArchived: json['exclude_archived'] as bool? ?? true,
        includedChatIds: _ids(json['included_chat_ids']),
        excludedChatIds: _ids(json['excluded_chat_ids']),
        unreadCount: (json['unread_count'] as num?)?.toInt() ?? 0,
        chatCount: (json['chat_count'] as num?)?.toInt() ?? 0,
      );

  final String id;
  final String title;
  final String emoji;
  final int position;

  final bool includeContacts;
  final bool includeNonContacts;
  final bool includeGroups;
  final bool includeChannels;
  final bool includeBots;
  final bool excludeMuted;
  final bool excludeRead;
  final bool excludeArchived;

  final List<String> includedChatIds;
  final List<String> excludedChatIds;

  /// The tab badge: the total across the chats this folder currently matches.
  final int unreadCount;
  final int chatCount;

  ChatFolder copyWith({
    String? title,
    String? emoji,
    bool? includeContacts,
    bool? includeNonContacts,
    bool? includeGroups,
    bool? includeChannels,
    bool? includeBots,
    bool? excludeMuted,
    bool? excludeRead,
    bool? excludeArchived,
  }) =>
      ChatFolder(
        id: id,
        title: title ?? this.title,
        emoji: emoji ?? this.emoji,
        position: position,
        includeContacts: includeContacts ?? this.includeContacts,
        includeNonContacts: includeNonContacts ?? this.includeNonContacts,
        includeGroups: includeGroups ?? this.includeGroups,
        includeChannels: includeChannels ?? this.includeChannels,
        includeBots: includeBots ?? this.includeBots,
        excludeMuted: excludeMuted ?? this.excludeMuted,
        excludeRead: excludeRead ?? this.excludeRead,
        excludeArchived: excludeArchived ?? this.excludeArchived,
        includedChatIds: includedChatIds,
        excludedChatIds: excludedChatIds,
        unreadCount: unreadCount,
        chatCount: chatCount,
      );

  Map<String, dynamic> toJson() => <String, dynamic>{
        'title': title,
        'emoji': emoji,
        'position': position,
        'include_contacts': includeContacts,
        'include_non_contacts': includeNonContacts,
        'include_groups': includeGroups,
        'include_channels': includeChannels,
        'include_bots': includeBots,
        'exclude_muted': excludeMuted,
        'exclude_read': excludeRead,
        'exclude_archived': excludeArchived,
      };

  static List<String> _ids(dynamic raw) => <String>[
        for (final dynamic id in raw as List<dynamic>? ?? const <dynamic>[])
          id as String,
      ];
}

/// Chat folders (§12).
///
/// None of this is queued offline: a folder is a filter over the server's view
/// of the chat list, and a filter that existed only on one device would show
/// different chats on the next one.
class FoldersRepository {
  FoldersRepository(this._api);

  final ApiClient _api;

  Future<List<ChatFolder>> list() async {
    final Map<String, dynamic> data =
        await _api.get<Map<String, dynamic>>('/chats/folders');
    return <ChatFolder>[
      for (final dynamic entry
          in data['folders'] as List<dynamic>? ?? const <dynamic>[])
        ChatFolder.fromJson(entry as Map<String, dynamic>),
    ];
  }

  Future<String> create(ChatFolder folder) async {
    final Map<String, dynamic> data = await _api.post<Map<String, dynamic>>(
      '/chats/folders',
      body: folder.toJson(),
    );
    return data['folder_id'] as String;
  }

  /// Replaces the whole definition, which is how it is edited: one small
  /// object on one screen.
  Future<void> update(ChatFolder folder) => _api.patch<dynamic>(
        '/chats/folders/${folder.id}',
        body: folder.toJson(),
      );

  Future<void> delete(String folderId) =>
      _api.delete<dynamic>('/chats/folders/$folderId');

  /// [mode] is `include` to pin a chat in whatever the rules say, or
  /// `exclude` to keep it out whatever they say.
  Future<void> setChat(String folderId, String chatId, String mode) =>
      _api.put<dynamic>(
        '/chats/folders/$folderId/chats/$chatId',
        body: <String, dynamic>{'mode': mode},
      );

  Future<void> removeChat(String folderId, String chatId) =>
      _api.delete<dynamic>('/chats/folders/$folderId/chats/$chatId');

  Future<void> reorder(List<String> folderIds) => _api.put<dynamic>(
        '/chats/folders/order',
        body: <String, dynamic>{'order': folderIds},
      );

  /// Which chats a folder currently matches.
  ///
  /// The list on screen streams from the local database, which is what makes
  /// it work offline; the folder rules are the server's — they reach for
  /// contacts, bot flags and mute state this device does not keep in one
  /// place. So the server is asked which chats belong, and the local rows are
  /// filtered to that set. A folder therefore needs the network the first time
  /// it is opened, and the main list never does.
  Future<Set<String>> chatIds(String folderId) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/chats',
      query: <String, String>{'folder_id': folderId, 'limit': '100'},
    );
    return <String>{
      for (final dynamic entry
          in data['chats'] as List<dynamic>? ?? const <dynamic>[])
        (entry as Map<String, dynamic>)['id'] as String,
    };
  }
}

final Provider<FoldersRepository> foldersRepositoryProvider =
    Provider<FoldersRepository>(
  (Ref ref) => FoldersRepository(ref.watch(apiClientProvider)),
);

final FutureProvider<List<ChatFolder>> chatFoldersProvider =
    FutureProvider<List<ChatFolder>>(
  (Ref ref) => ref.watch(foldersRepositoryProvider).list(),
);

/// The folder the chat list is currently showing. Null is the main list.
final StateProvider<String?> selectedFolderProvider =
    StateProvider<String?>((Ref ref) => null);

final FutureProviderFamily<Set<String>, String> folderChatIdsProvider =
    FutureProvider.family<Set<String>, String>(
  (Ref ref, String folderId) =>
      ref.watch(foldersRepositoryProvider).chatIds(folderId),
);
