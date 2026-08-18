import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:uuid/uuid.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../data/comments_repository.dart';

/// The comments on one channel post (§15).
///
/// Comments are ordinary replies in the channel's linked discussion group, so
/// nothing here is a special kind of message. A reader who is not yet in that
/// group is joined by commenting, which is why the composer is offered without
/// asking anyone to find and join a second chat first.
class CommentsSheet extends ConsumerStatefulWidget {
  const CommentsSheet({
    required this.channelId,
    required this.postId,
    super.key,
  });

  final String channelId;
  final String postId;

  /// Opens the sheet over the current screen.
  static Future<void> show(
    BuildContext context, {
    required String channelId,
    required String postId,
  }) =>
      showModalBottomSheet<void>(
        context: context,
        isScrollControlled: true,
        useSafeArea: true,
        builder: (BuildContext context) => CommentsSheet(
          channelId: channelId,
          postId: postId,
        ),
      );

  @override
  ConsumerState<CommentsSheet> createState() => _CommentsSheetState();
}

class _CommentsSheetState extends ConsumerState<CommentsSheet> {
  static const Uuid _uuid = Uuid();
  final TextEditingController _composer = TextEditingController();

  CommentPage? _page;
  ApiException? _failure;
  bool _loading = true;
  bool _sending = false;

  @override
  void initState() {
    super.initState();
    _load();
  }

  @override
  void dispose() {
    _composer.dispose();
    super.dispose();
  }

  Future<void> _load() async {
    try {
      final CommentPage page = await ref
          .read(commentsRepositoryProvider)
          .comments(widget.channelId, widget.postId);
      if (mounted) {
        setState(() {
          _page = page;
          _failure = null;
          _loading = false;
        });
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(() {
          _failure = error;
          _loading = false;
        });
      }
    }
  }

  Future<void> _send() async {
    final String text = _composer.text.trim();
    if (text.isEmpty) {
      return;
    }
    setState(() => _sending = true);

    try {
      await ref.read(commentsRepositoryProvider).comment(
            channelId: widget.channelId,
            postId: widget.postId,
            clientMessageId: _uuid.v4(),
            content: text,
          );
      _composer.clear();
      await _load();
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    } finally {
      if (mounted) {
        setState(() => _sending = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Padding(
      padding: EdgeInsets.only(
        bottom: MediaQuery.of(context).viewInsets.bottom,
      ),
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          ListTile(
            title: Text(
              _page == null
                  ? l10n.commentsTitle
                  : l10n.commentsCount(_page!.thread.commentCount),
              style: Theme.of(context).textTheme.titleMedium,
            ),
          ),
          const Divider(height: 1),
          Flexible(child: _body(l10n)),
          const Divider(height: 1),
          Padding(
            padding: const EdgeInsets.all(SobhSpacing.md),
            child: Row(
              children: <Widget>[
                Expanded(
                  child: TextField(
                    controller: _composer,
                    minLines: 1,
                    maxLines: 4,
                    decoration: InputDecoration(hintText: l10n.commentsHint),
                  ),
                ),
                IconButton(
                  onPressed: _sending ? null : _send,
                  icon: const Icon(Icons.send),
                ),
              ],
            ),
          ),
        ],
      ),
    );
  }

  Widget _body(AppLocalizations l10n) {
    if (_loading) {
      return const Padding(
        padding: EdgeInsets.all(SobhSpacing.xl),
        child: Center(child: CircularProgressIndicator()),
      );
    }

    // The two refusals worth naming are the two a reader can do nothing
    // about: comments switched off, and no discussion group at all.
    if (_failure != null) {
      final String message = switch (_failure!.statusCode) {
        403 => l10n.commentsDisabled,
        404 => l10n.commentsNoGroup,
        _ => _failure!.isOffline ? l10n.errorNetwork : _failure!.message,
      };
      return Padding(
        padding: const EdgeInsets.all(SobhSpacing.xl),
        child: Text(message, textAlign: TextAlign.center),
      );
    }

    final List<Comment> comments = _page?.comments ?? const <Comment>[];
    if (comments.isEmpty) {
      return Padding(
        padding: const EdgeInsets.all(SobhSpacing.xl),
        child: Text(l10n.commentsEmpty, textAlign: TextAlign.center),
      );
    }

    return ListView.separated(
      shrinkWrap: true,
      reverse: true,
      itemCount: comments.length,
      separatorBuilder: (_, __) => const Divider(height: 1),
      itemBuilder: (BuildContext context, int index) {
        final Comment comment = comments[index];
        return ListTile(
          dense: true,
          title: Text(comment.content),
          subtitle: Text(
            TimeOfDay.fromDateTime(comment.createdAt).format(context),
            style: Theme.of(context).textTheme.labelSmall,
          ),
        );
      },
    );
  }
}
