import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/app_theme.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../../media/data/media_repository.dart';
import '../data/stickers_repository.dart';

/// Picks a sticker to send, and returns its media id (§12).
///
/// Returns null when the sheet is dismissed without a choice, which is the
/// common case and not an error.
Future<String?> showStickerPicker(BuildContext context) =>
    showModalBottomSheet<String>(
      context: context,
      isScrollControlled: true,
      builder: (BuildContext context) => const _StickerPicker(),
    );

class _StickerPicker extends ConsumerStatefulWidget {
  const _StickerPicker();

  @override
  ConsumerState<_StickerPicker> createState() => _StickerPickerState();
}

class _StickerPickerState extends ConsumerState<_StickerPicker> {
  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);
    final AsyncValue<List<StickerSet>> sets =
        ref.watch(installedStickerSetsProvider);

    return DraggableScrollableSheet(
      initialChildSize: 0.6,
      maxChildSize: 0.9,
      expand: false,
      builder: (BuildContext context, ScrollController controller) => Column(
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.all(SobhSpacing.md),
            child: Row(
              children: <Widget>[
                Expanded(
                  child: Text(
                    l10n.stickersTitle,
                    style: Theme.of(context).textTheme.titleMedium,
                  ),
                ),
                IconButton(
                  tooltip: l10n.stickersBrowse,
                  icon: const Icon(Icons.search),
                  onPressed: () async {
                    await Navigator.of(context).push(
                      MaterialPageRoute<void>(
                        builder: (_) => const StickerStoreScreen(),
                      ),
                    );
                    ref.invalidate(installedStickerSetsProvider);
                  },
                ),
              ],
            ),
          ),
          const Divider(height: 1),
          Expanded(
            child: sets.when(
              loading: () => const SobhLoading(),
              error: (Object error, StackTrace _) => SobhErrorState(
                error: error,
                onRetry: () => ref.invalidate(installedStickerSetsProvider),
              ),
              data: (List<StickerSet> rows) {
                if (rows.isEmpty) {
                  return SobhEmptyState(
                    icon: Icons.emoji_emotions_outlined,
                    title: l10n.stickersNoneTitle,
                    body: l10n.stickersNoneMessage,
                  );
                }
                return ListView.builder(
                  controller: controller,
                  itemCount: rows.length,
                  itemBuilder: (BuildContext context, int index) =>
                      _SetSection(set: rows[index]),
                );
              },
            ),
          ),
        ],
      ),
    );
  }
}

class _SetSection extends StatelessWidget {
  const _SetSection({required this.set});

  final StickerSet set;

  @override
  Widget build(BuildContext context) => Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: <Widget>[
          Padding(
            padding: const EdgeInsets.fromLTRB(
              SobhSpacing.lg,
              SobhSpacing.md,
              SobhSpacing.lg,
              SobhSpacing.sm,
            ),
            child: Text(
              set.title,
              style: Theme.of(context).textTheme.titleSmall?.copyWith(
                    color: SobhTheme.of(context).textSecondary,
                  ),
            ),
          ),
          GridView.builder(
            shrinkWrap: true,
            physics: const NeverScrollableScrollPhysics(),
            padding: const EdgeInsets.symmetric(horizontal: SobhSpacing.md),
            gridDelegate: const SliverGridDelegateWithFixedCrossAxisCount(
              crossAxisCount: 4,
              mainAxisSpacing: SobhSpacing.sm,
              crossAxisSpacing: SobhSpacing.sm,
            ),
            itemCount: set.stickers.length,
            itemBuilder: (BuildContext context, int index) {
              final Sticker sticker = set.stickers[index];
              return InkWell(
                borderRadius: BorderRadius.circular(SobhRadius.md),
                onTap: () => Navigator.of(context).pop(sticker.mediaId),
                child: _StickerImage(sticker: sticker),
              );
            },
          ),
        ],
      );
}

class _StickerImage extends ConsumerWidget {
  const _StickerImage({required this.sticker});

  final Sticker sticker;

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AsyncValue<String> url = ref.watch(
      mediaUrlProvider((mediaId: sticker.mediaId, variant: null)),
    );

    return url.when(
      loading: () => const Center(
        child: SizedBox(
          width: SobhSizes.iconMedium,
          height: SobhSizes.iconMedium,
          child: CircularProgressIndicator(strokeWidth: 2),
        ),
      ),
      // A sticker that will not load still has its emoji, which is more useful
      // than an empty square.
      error: (Object _, StackTrace __) => Center(
        child: Text(
          sticker.emoji,
          style: const TextStyle(fontSize: SobhSizes.iconLarge),
        ),
      ),
      data: (String resolved) => Image.network(
        resolved,
        fit: BoxFit.contain,
        errorBuilder: (BuildContext context, Object _, StackTrace? __) =>
            Center(
          child: Text(
            sticker.emoji,
            style: const TextStyle(fontSize: SobhSizes.iconLarge),
          ),
        ),
      ),
    );
  }
}

/// Finding and installing sticker sets.
class StickerStoreScreen extends ConsumerStatefulWidget {
  const StickerStoreScreen({super.key});

  @override
  ConsumerState<StickerStoreScreen> createState() => _StickerStoreScreenState();
}

class _StickerStoreScreenState extends ConsumerState<StickerStoreScreen> {
  final TextEditingController _query = TextEditingController();
  Future<List<StickerSet>>? _results;

  @override
  void initState() {
    super.initState();
    _search('');
  }

  @override
  void dispose() {
    _query.dispose();
    super.dispose();
  }

  void _search(String query) => setState(() {
        _results = ref.read(stickersRepositoryProvider).search(query);
      });

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Scaffold(
      appBar: AppBar(
        title: TextField(
          controller: _query,
          autofocus: true,
          textInputAction: TextInputAction.search,
          decoration: InputDecoration(
            hintText: l10n.stickersSearchHint,
            border: InputBorder.none,
          ),
          onSubmitted: _search,
        ),
      ),
      body: FutureBuilder<List<StickerSet>>(
        future: _results,
        builder: (
          BuildContext context,
          AsyncSnapshot<List<StickerSet>> snapshot,
        ) {
          if (snapshot.connectionState == ConnectionState.waiting) {
            return const SobhLoading();
          }
          if (snapshot.hasError) {
            return SobhErrorState(
              error: snapshot.error!,
              onRetry: () => _search(_query.text),
            );
          }

          final List<StickerSet> rows = snapshot.data ?? const <StickerSet>[];
          if (rows.isEmpty) {
            return SobhEmptyState(
              icon: Icons.search_off,
              title: l10n.stickersNoResultsTitle,
              body: l10n.stickersNoResultsMessage,
            );
          }

          return ListView.builder(
            itemCount: rows.length,
            itemBuilder: (BuildContext context, int index) {
              final StickerSet set = rows[index];
              return ListTile(
                leading: const Icon(Icons.emoji_emotions_outlined),
                title: Text(set.title),
                subtitle: Text('@${set.slug}'),
                trailing: set.isAdded
                    ? TextButton(
                        onPressed: () => _toggle(set, install: false),
                        child: Text(l10n.commonRemove),
                      )
                    : FilledButton(
                        onPressed: () => _toggle(set, install: true),
                        child: Text(l10n.commonAdd),
                      ),
              );
            },
          );
        },
      ),
    );
  }

  Future<void> _toggle(StickerSet set, {required bool install}) async {
    final AppLocalizations l10n = AppLocalizations.of(context);
    try {
      final StickersRepository repository =
          ref.read(stickersRepositoryProvider);
      if (install) {
        await repository.add(set.id);
      } else {
        await repository.remove(set.id);
      }
      ref.invalidate(installedStickerSetsProvider);
      if (mounted) {
        _search(_query.text);
      }
    } on ApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(
            content: Text(error.isOffline ? l10n.errorNetwork : error.message),
          ),
        );
      }
    }
  }
}
