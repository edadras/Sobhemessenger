import 'dart:async';

import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../data/stories_repository.dart';

/// How long a story stays on screen before advancing.
const Duration _storyDuration = Duration(seconds: 5);

/// Full-screen story playback (§17).
///
/// A view is recorded once per story as it is shown. The server counts a
/// viewer once, so re-opening the same story does not inflate the count.
class StoryViewerScreen extends ConsumerStatefulWidget {
  const StoryViewerScreen({
    super.key,
    required this.stories,
    this.initialIndex = 0,
  });

  final List<Story> stories;
  final int initialIndex;

  @override
  ConsumerState<StoryViewerScreen> createState() => _StoryViewerScreenState();
}

class _StoryViewerScreenState extends ConsumerState<StoryViewerScreen>
    with SingleTickerProviderStateMixin {
  late final PageController _pages =
      PageController(initialPage: widget.initialIndex);
  late final AnimationController _progress = AnimationController(
    vsync: this,
    duration: _storyDuration,
  )..addStatusListener(_onSegmentFinished);

  int _index = 0;

  @override
  void initState() {
    super.initState();
    _index = widget.initialIndex;
    _show(_index);
  }

  @override
  void dispose() {
    _progress.dispose();
    _pages.dispose();
    super.dispose();
  }

  void _onSegmentFinished(AnimationStatus status) {
    if (status == AnimationStatus.completed) {
      _advance();
    }
  }

  void _show(int index) {
    _progress
      ..reset()
      ..forward();

    // Recording a view must never block playback or surface an error over the
    // story: it is telemetry the author sees, not something the viewer did.
    unawaited(
      ref
          .read(storiesRepositoryProvider)
          .markViewed(widget.stories[index].id)
          .catchError((Object _) {}),
    );
  }

  void _advance() {
    if (_index + 1 >= widget.stories.length) {
      Navigator.of(context).maybePop();
      return;
    }
    _pages.nextPage(duration: SobhDuration.fast, curve: Curves.easeOut);
  }

  void _back() {
    if (_index == 0) {
      _show(0);
      return;
    }
    _pages.previousPage(duration: SobhDuration.fast, curve: Curves.easeOut);
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    return Scaffold(
      backgroundColor: palette.chatBackground,
      body: SafeArea(
        child: Stack(
          children: <Widget>[
            GestureDetector(
              // Tapping the leading third goes back, the rest advances — the
              // gesture every story implementation has trained users on.
              onTapUp: (TapUpDetails details) {
                final double width = MediaQuery.sizeOf(context).width;
                details.globalPosition.dx < width / 3 ? _back() : _advance();
              },
              onLongPressStart: (_) => _progress.stop(),
              onLongPressEnd: (_) => _progress.forward(),
              child: PageView.builder(
                controller: _pages,
                itemCount: widget.stories.length,
                onPageChanged: (int index) {
                  setState(() => _index = index);
                  _show(index);
                },
                itemBuilder: (BuildContext context, int index) =>
                    _StoryPage(story: widget.stories[index]),
              ),
            ),
            Positioned(
              top: SobhSpacing.sm,
              left: SobhSpacing.lg,
              right: SobhSpacing.lg,
              child: Column(
                children: <Widget>[
                  Row(
                    children: <Widget>[
                      for (int i = 0; i < widget.stories.length; i++)
                        Expanded(
                          child: Padding(
                            padding: const EdgeInsets.symmetric(
                              horizontal: SobhSpacing.xxs,
                            ),
                            child: AnimatedBuilder(
                              animation: _progress,
                              builder: (BuildContext context, Widget? _) =>
                                  LinearProgressIndicator(
                                value: i < _index
                                    ? 1
                                    : i == _index
                                        ? _progress.value
                                        : 0,
                                minHeight: SobhSpacing.xxs,
                                backgroundColor: palette.outline,
                              ),
                            ),
                          ),
                        ),
                    ],
                  ),
                  const SizedBox(height: SobhSpacing.sm),
                  Row(
                    children: <Widget>[
                      Expanded(
                        child: Text(
                          widget.stories[_index].authorName,
                          style: Theme.of(context).textTheme.titleSmall,
                        ),
                      ),
                      Text(
                        l10n.storiesViewers(widget.stories[_index].viewCount),
                        style: Theme.of(context).textTheme.labelSmall,
                      ),
                      IconButton(
                        icon: const Icon(Icons.close),
                        onPressed: () => Navigator.of(context).maybePop(),
                      ),
                    ],
                  ),
                ],
              ),
            ),
          ],
        ),
      ),
    );
  }
}

class _StoryPage extends StatelessWidget {
  const _StoryPage({required this.story});

  final Story story;

  @override
  Widget build(BuildContext context) {
    final SobhPalette palette = SobhTheme.of(context);

    return Container(
      alignment: Alignment.center,
      padding: const EdgeInsets.all(SobhSpacing.xxl),
      color: story.background.isEmpty
          ? null
          : _parseColor(story.background, palette),
      child: Text(
        story.caption,
        textAlign: TextAlign.center,
        style: Theme.of(context).textTheme.headlineSmall,
      ),
    );
  }

  /// The background is an author-chosen hex colour. Anything unparseable falls
  /// back to the theme rather than throwing inside a build.
  static Color _parseColor(String value, SobhPalette palette) {
    final String hex = value.replaceFirst('#', '');
    final int? parsed = int.tryParse(hex, radix: 16);
    if (parsed == null || (hex.length != 6 && hex.length != 8)) {
      return palette.surfaceVariant;
    }
    return Color(hex.length == 6 ? 0xFF000000 | parsed : parsed);
  }
}
