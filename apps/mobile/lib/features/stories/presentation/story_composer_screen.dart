import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:image_picker/image_picker.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../media/data/media_repository.dart';
import '../../media/presentation/attachment_picker.dart';
import '../data/stories_repository.dart';
import 'close_friends_screen.dart';

/// The background colours offered for a text story.
///
/// They are drawn from the palette rather than invented here, so a story
/// composed today still looks like SOBH after a re-theme (§84.21).
List<Color> _backgrounds(SobhPalette palette) => <Color>[
      palette.primary,
      palette.secondary,
      palette.info,
      palette.success,
      palette.warning,
      palette.surfaceVariant,
    ];

/// Composes a story (§17).
class StoryComposerScreen extends ConsumerStatefulWidget {
  const StoryComposerScreen({super.key});

  @override
  ConsumerState<StoryComposerScreen> createState() =>
      _StoryComposerScreenState();
}

class _StoryComposerScreenState extends ConsumerState<StoryComposerScreen> {
  final TextEditingController _caption = TextEditingController();

  PickedAttachment? _attachment;
  int _backgroundIndex = 0;
  String _privacy = 'contacts';
  bool _posting = false;
  String? _error;

  @override
  void dispose() {
    _caption.dispose();
    super.dispose();
  }

  Future<void> _pick() async {
    final PickedAttachment? picked = await const AttachmentPicker().pickImage(
      source: ImageSource.gallery,
    );
    if (picked != null && mounted) {
      setState(() => _attachment = picked);
    }
  }

  Future<void> _post() async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);

    setState(() {
      _posting = true;
      _error = null;
    });

    try {
      String? mediaId;
      if (_attachment != null) {
        // The bytes go up first; the story then references the object, so a
        // failed post never leaves a half-uploaded story behind.
        final Media media = await ref.read(mediaRepositoryProvider).upload(
              file: _attachment!.file,
              kind: MediaKind.image,
              mimeType: _attachment!.mimeType,
            );
        mediaId = media.id;
      }

      await ref.read(storiesRepositoryProvider).post(
            // 'image', not 'photo': that is what the server accepts and what
            // the CHECK on stories.type allows. The composer said 'photo', so
            // every picture story was refused as an unsupported type.
            type: mediaId == null ? 'text' : 'image',
            caption: _caption.text.trim(),
            background: mediaId == null
                ? _hex(_backgrounds(palette)[_backgroundIndex])
                : '',
            mediaId: mediaId,
            privacy: _privacy,
          );

      ref.invalidate(storyFeedProvider);
      if (mounted) {
        Navigator.of(context).pop();
      }
    } on ApiException catch (error) {
      if (mounted) {
        setState(
          () => _error = error.isOffline ? l10n.errorNetwork : error.message,
        );
      }
    } finally {
      if (mounted) {
        setState(() => _posting = false);
      }
    }
  }

  static String _hex(Color color) =>
      '#${(color.toARGB32() & 0xFFFFFF).toRadixString(16).padLeft(6, '0')}';

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final SobhPalette palette = SobhTheme.of(context);
    final List<Color> backgrounds = _backgrounds(palette);
    final bool canPost =
        !_posting && (_attachment != null || _caption.text.trim().isNotEmpty);

    return Scaffold(
      appBar: AppBar(
        title: Text(l10n.storiesCreate),
        actions: <Widget>[
          TextButton(
            onPressed: canPost ? _post : null,
            child: Text(l10n.storiesPost),
          ),
        ],
      ),
      body: Column(
        children: <Widget>[
          Expanded(
            child: Container(
              width: double.infinity,
              margin: const EdgeInsets.all(SobhSpacing.lg),
              decoration: BoxDecoration(
                color: _attachment == null
                    ? backgrounds[_backgroundIndex]
                    : palette.surfaceVariant,
                borderRadius: BorderRadius.circular(SobhRadius.lg),
                image: _attachment == null
                    ? null
                    : DecorationImage(
                        image: FileImage(_attachment!.file),
                        fit: BoxFit.cover,
                      ),
              ),
              alignment: Alignment.center,
              padding: const EdgeInsets.all(SobhSpacing.xl),
              child: TextField(
                controller: _caption,
                textAlign: TextAlign.center,
                maxLines: null,
                style: Theme.of(context).textTheme.headlineSmall,
                decoration: InputDecoration(
                  hintText: l10n.storiesCaptionHint,
                  border: InputBorder.none,
                ),
                onChanged: (_) => setState(() {}),
              ),
            ),
          ),

          if (_error != null)
            Padding(
              padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.lg),
              child: Text(_error!, style: TextStyle(color: palette.error)),
            ),

          // Background choice only makes sense for a text story; over an image
          // it would do nothing.
          if (_attachment == null)
            SizedBox(
              height: SobhSizes.avatarMedium,
              child: ListView.separated(
                scrollDirection: Axis.horizontal,
                padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.lg),
                itemCount: backgrounds.length,
                separatorBuilder: (_, __) =>
                    const SizedBox(width: SobhSpacing.sm),
                itemBuilder: (BuildContext context, int index) =>
                    GestureDetector(
                  onTap: () => setState(() => _backgroundIndex = index),
                  child: Container(
                    width: SobhSizes.avatarSmall,
                    decoration: BoxDecoration(
                      color: backgrounds[index],
                      shape: BoxShape.circle,
                      border: Border.all(
                        color: index == _backgroundIndex
                            ? palette.textPrimary
                            : palette.outline,
                        width: index == _backgroundIndex ? 2 : 1,
                      ),
                    ),
                  ),
                ),
              ),
            ),

          Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child: Row(
              children: <Widget>[
                IconButton(
                  onPressed: _pick,
                  icon: const Icon(Icons.photo_library_outlined),
                  tooltip: l10n.mediaGallery,
                ),
                if (_attachment != null)
                  IconButton(
                    onPressed: () => setState(() => _attachment = null),
                    icon: const Icon(Icons.close),
                    tooltip: l10n.commonRemove,
                  ),
                const Spacer(),
                DropdownButton<String>(
                  value: _privacy,
                  onChanged: (String? value) =>
                      setState(() => _privacy = value ?? 'contacts'),
                  items: <DropdownMenuItem<String>>[
                    DropdownMenuItem<String>(
                      value: 'everyone',
                      child: Text(l10n.privacyEveryone),
                    ),
                    DropdownMenuItem<String>(
                      value: 'contacts',
                      child: Text(l10n.privacyContacts),
                    ),
                    DropdownMenuItem<String>(
                      value: 'close_friends',
                      child: Text(l10n.storiesCloseFriends),
                    ),
                  ],
                ),
                // The option was already offered, with no way to say who is on
                // the list. Choosing an audience the user cannot see or change
                // is not a privacy setting.
                if (_privacy == 'close_friends')
                  IconButton(
                    onPressed: () => Navigator.of(context).push(
                      MaterialPageRoute<void>(
                        builder: (_) => const CloseFriendsScreen(),
                      ),
                    ),
                    icon: const Icon(Icons.group_outlined),
                    tooltip: l10n.storiesCloseFriendsEdit,
                  ),
              ],
            ),
          ),
          if (_posting) const LinearProgressIndicator(),
        ],
      ),
    );
  }
}

/// Who watched a story, for its author (§17).
class StoryViewersSheet extends ConsumerWidget {
  const StoryViewersSheet({super.key, required this.storyId});

  final String storyId;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<StoryViewer>> viewers = ref.watch(
      storyViewersProvider(storyId),
    );

    return SafeArea(
      child: Column(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.all(SobhSpacing.lg),
            child: Text(
              l10n.storiesViewersTitle,
              style: Theme.of(context).textTheme.titleMedium,
            ),
          ),
          viewers.when(
            loading: () => const Padding(
              padding: EdgeInsets.all(SobhSpacing.xl),
              child: CircularProgressIndicator(),
            ),
            error: (Object error, StackTrace _) => Padding(
              padding: const EdgeInsets.all(SobhSpacing.xl),
              child: Text(l10n.errorGeneric),
            ),
            data: (List<StoryViewer> rows) => Flexible(
              child: ListView.builder(
                shrinkWrap: true,
                itemCount: rows.length,
                itemBuilder: (BuildContext context, int index) => ListTile(
                  title: Text(rows[index].displayName),
                  trailing: rows[index].reaction == null
                      ? null
                      : Text(rows[index].reaction!),
                ),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
