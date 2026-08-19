import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';

import '../../../core/network/api_client.dart';
import '../../auth/session_controller.dart';

/// A poll and its options (§17).
class Poll {
  const Poll({
    required this.id,
    required this.chatId,
    required this.question,
    required this.totalVoters,
    required this.options,
    this.isAnonymous = false,
    this.allowsMultiple = false,
    this.isQuiz = false,
    this.correctOption,
    this.closedAt,
    this.myVotes = const <String>[],
  });

  factory Poll.fromJson(Map<String, dynamic> json) => Poll(
        id: json['id'] as String,
        chatId: json['chat_id'] as String,
        question: json['question'] as String? ?? '',
        totalVoters: (json['total_voters'] as num?)?.toInt() ?? 0,
        options: <PollOption>[
          for (final dynamic option
              in json['options'] as List<dynamic>? ?? const <dynamic>[])
            PollOption.fromJson(option as Map<String, dynamic>),
        ],
        isAnonymous: json['is_anonymous'] as bool? ?? false,
        allowsMultiple: json['allows_multiple'] as bool? ?? false,
        isQuiz: json['is_quiz'] as bool? ?? false,
        correctOption: (json['correct_option'] as num?)?.toInt(),
        closedAt: json['closed_at'] == null
            ? null
            : DateTime.parse(json['closed_at'] as String).toLocal(),
        myVotes: <String>[
          for (final dynamic vote
              in json['my_votes'] as List<dynamic>? ?? const <dynamic>[])
            vote as String,
        ],
      );

  final String id;
  final String chatId;
  final String question;
  final int totalVoters;
  final List<PollOption> options;
  final bool isAnonymous;
  final bool allowsMultiple;
  final bool isQuiz;
  final int? correctOption;
  final DateTime? closedAt;
  final List<String> myVotes;

  bool get isClosed => closedAt != null;
  bool get hasVoted => myVotes.isNotEmpty;

  /// Results stay hidden until the viewer has voted or the poll has closed —
  /// showing them earlier would bias the vote.
  bool get showsResults => hasVoted || isClosed;

  double shareOf(PollOption option) =>
      totalVoters == 0 ? 0 : option.voteCount / totalVoters;
}

class PollOption {
  const PollOption({
    required this.id,
    required this.position,
    required this.text,
    required this.voteCount,
  });

  factory PollOption.fromJson(Map<String, dynamic> json) => PollOption(
        id: json['id'] as String,
        position: (json['position'] as num?)?.toInt() ?? 0,
        text: json['text'] as String? ?? '',
        voteCount: (json['vote_count'] as num?)?.toInt() ?? 0,
      );

  final String id;
  final int position;
  final String text;
  final int voteCount;
}

class PollsRepository {
  PollsRepository(this._api);

  final ApiClient _api;
  static const Uuid _uuid = Uuid();

  Future<Poll> create({
    required String chatId,
    required String question,
    required List<String> options,
    bool isAnonymous = false,
    bool allowsMultiple = false,
    bool isQuiz = false,
    int? correctOption,
  }) async =>
      Poll.fromJson(
        await _api.post<Map<String, dynamic>>(
          '/polls',
          body: <String, dynamic>{
            'chat_id': chatId,
            // A poll creates a message, so it carries a client id for the same
            // idempotency reason every other send does.
            'client_message_id': _uuid.v4(),
            'question': question,
            'options': options,
            'is_anonymous': isAnonymous,
            'allows_multiple': allowsMultiple,
            'is_quiz': isQuiz,
            if (correctOption != null) 'correct_option': correctOption,
          },
        ),
      );

  Future<Poll> get(String pollId) async =>
      Poll.fromJson(await _api.get<Map<String, dynamic>>('/polls/$pollId'));

  /// Casts or replaces a vote. An empty list retracts it.
  ///
  /// The server replaces the whole set transactionally, so a change of mind on
  /// a multiple-choice poll cannot leave a half-updated vote behind.
  Future<Poll> vote(String pollId, List<String> optionIds) async =>
      Poll.fromJson(
        await _api.post<Map<String, dynamic>>(
          '/polls/$pollId/vote',
          body: <String, dynamic>{'option_ids': optionIds},
        ),
      );

  Future<Poll> close(String pollId) async => Poll.fromJson(
        await _api.post<Map<String, dynamic>>('/polls/$pollId/close'),
      );

  /// Who chose one option (§18).
  ///
  /// Only for a poll that is not anonymous — the server refuses otherwise,
  /// which is the guarantee an anonymous poll makes. The screen does not offer
  /// this for one, so the refusal is a backstop rather than the normal path.
  Future<List<PollVoter>> voters(String pollId, String optionId) async {
    final Map<String, dynamic> data = await _api.get<Map<String, dynamic>>(
      '/polls/$pollId/options/$optionId/voters',
    );
    return <PollVoter>[
      for (final dynamic entry
          in data['voters'] as List<dynamic>? ?? const <dynamic>[])
        PollVoter.fromJson(entry as Map<String, dynamic>),
    ];
  }
}

/// One person who chose an option, and when.
class PollVoter {
  const PollVoter({
    required this.userId,
    required this.displayName,
    required this.votedAt,
    this.username,
    this.avatarMediaId,
  });

  factory PollVoter.fromJson(Map<String, dynamic> json) => PollVoter(
        userId: json['user_id'] as String,
        displayName: json['display_name'] as String? ?? '',
        votedAt: DateTime.parse(json['voted_at'] as String).toLocal(),
        username: json['username'] as String?,
        avatarMediaId: json['avatar_media_id'] as String?,
      );

  final String userId;
  final String displayName;
  final DateTime votedAt;
  final String? username;
  final String? avatarMediaId;
}

final Provider<PollsRepository> pollsRepositoryProvider =
    Provider<PollsRepository>(
  (Ref ref) => PollsRepository(ref.watch(apiClientProvider)),
);

final FutureProviderFamily<Poll, String> pollProvider =
    FutureProvider.family<Poll, String>(
  (Ref ref, String pollId) => ref.watch(pollsRepositoryProvider).get(pollId),
);

final FutureProviderFamily<List<PollVoter>, (String, String)>
    pollVotersProvider =
    FutureProvider.family<List<PollVoter>, (String, String)>(
  (Ref ref, (String, String) key) =>
      ref.watch(pollsRepositoryProvider).voters(key.$1, key.$2),
);
