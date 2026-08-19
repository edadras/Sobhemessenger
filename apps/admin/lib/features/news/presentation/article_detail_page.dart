import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// The two things an article carries besides its text: its picture gallery and
/// its translations (§25).
///
/// Both were in the schema with no way to reach them, which meant an article
/// could be published in one language with a single image and nothing else was
/// possible. They live on one screen because they are the same job — preparing
/// a story for publication — and splitting them would mean two round trips
/// through the article list to do it.
class ArticleDetailPage extends ConsumerStatefulWidget {
  const ArticleDetailPage({
    super.key,
    required this.articleId,
    required this.title,
  });

  final String articleId;
  final String title;

  @override
  ConsumerState<ArticleDetailPage> createState() => _ArticleDetailPageState();
}

class _ArticleDetailPageState extends ConsumerState<ArticleDetailPage> {
  int _reloadToken = 0;

  void _reload() => setState(() => _reloadToken++);

  void _report(AdminApiException error) {
    if (mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(error.message)));
    }
  }

  // ------------------------------------------------------------- gallery

  Future<void> _addImage(List<dynamic> current) async {
    final _GalleryDraft? draft = await showDialog<_GalleryDraft>(
      context: context,
      builder: (BuildContext context) => const _GalleryItemDialog(),
    );
    if (draft == null) {
      return;
    }

    // The endpoint replaces the whole gallery, so an addition is the existing
    // list plus one — sending only the new image would delete the rest.
    final List<Map<String, dynamic>> items = <Map<String, dynamic>>[
      for (final dynamic entry in current)
        <String, dynamic>{
          'media_id': (entry as Map<String, dynamic>)['media_id'],
          'position': entry['position'],
          'caption': entry['caption'] ?? '',
        },
      <String, dynamic>{
        'media_id': draft.mediaId,
        'position': current.length,
        'caption': draft.caption,
      },
    ];

    try {
      await ref
          .read(adminApiProvider)
          .replaceGallery(widget.articleId, items);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  Future<void> _removeImage(List<dynamic> current, int index) async {
    // Positions are renumbered from zero so removing the middle image does not
    // leave a gap the reader's gallery would have to interpret.
    final List<Map<String, dynamic>> items = <Map<String, dynamic>>[];
    for (int i = 0; i < current.length; i++) {
      if (i == index) {
        continue;
      }
      final Map<String, dynamic> entry = current[i] as Map<String, dynamic>;
      items.add(<String, dynamic>{
        'media_id': entry['media_id'],
        'position': items.length,
        'caption': entry['caption'] ?? '',
      });
    }

    try {
      await ref
          .read(adminApiProvider)
          .replaceGallery(widget.articleId, items);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  // -------------------------------------------------------- translations

  Future<void> _editTranslation([Map<String, dynamic>? existing]) async {
    final _TranslationDraft? draft = await showDialog<_TranslationDraft>(
      context: context,
      builder: (BuildContext context) =>
          _TranslationDialog(existing: existing),
    );
    if (draft == null) {
      return;
    }

    try {
      await ref.read(adminApiProvider).saveTranslation(
            widget.articleId,
            draft.locale,
            title: draft.title,
            subtitle: draft.subtitle,
            body: draft.body,
            source: draft.source,
          );
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  Future<void> _approve(String locale) async {
    try {
      await ref
          .read(adminApiProvider)
          .approveTranslation(widget.articleId, locale);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  Future<void> _deleteTranslation(String locale) async {
    try {
      await ref
          .read(adminApiProvider)
          .deleteTranslation(widget.articleId, locale);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  @override
  Widget build(BuildContext context) {
    final AdminApi api = ref.watch(adminApiProvider);

    return Scaffold(
      appBar: AppBar(title: Text(widget.title)),
      body: Padding(
        padding: const EdgeInsets.all(24),
        child: Row(
          crossAxisAlignment: CrossAxisAlignment.start,
          children: <Widget>[
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: <Widget>[
                  Text(
                    'گالری تصاویر',
                    style: Theme.of(context).textTheme.titleMedium,
                  ),
                  const SizedBox(height: 8),
                  Expanded(
                    child: AsyncSection<List<dynamic>>(
                      key: ValueKey<String>('gallery|$_reloadToken'),
                      future: api.articleGallery(widget.articleId),
                      emptyMessage: 'تصویری افزوده نشده',
                      builder:
                          (BuildContext context, List<dynamic> items) =>
                              _GalleryList(
                        items: items,
                        onRemove: (int index) => _removeImage(items, index),
                      ),
                    ),
                  ),
                  const SizedBox(height: 8),
                  // The gallery has to be read before it can be added to,
                  // because the endpoint replaces it. Fetching it here rather
                  // than reusing the list above keeps the button honest when
                  // the list is still loading or has failed.
                  OutlinedButton.icon(
                    onPressed: () async {
                      try {
                        await _addImage(
                          await api.articleGallery(widget.articleId),
                        );
                      } on AdminApiException catch (error) {
                        _report(error);
                      }
                    },
                    icon: const Icon(Icons.add_photo_alternate_outlined),
                    label: const Text('افزودن تصویر'),
                  ),
                ],
              ),
            ),
            const VerticalDivider(width: 32),
            Expanded(
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.start,
                children: <Widget>[
                  Text(
                    'ترجمه‌ها',
                    style: Theme.of(context).textTheme.titleMedium,
                  ),
                  const SizedBox(height: 8),
                  Expanded(
                    child: AsyncSection<List<dynamic>>(
                      key: ValueKey<String>('translations|$_reloadToken'),
                      future: api.translations(widget.articleId),
                      emptyMessage: 'ترجمه‌ای ثبت نشده',
                      builder: (BuildContext context, List<dynamic> rows) =>
                          ListView.separated(
                        itemCount: rows.length,
                        separatorBuilder: (_, __) => const Divider(height: 1),
                        itemBuilder: (BuildContext context, int index) {
                          final Map<String, dynamic> row =
                              rows[index] as Map<String, dynamic>;
                          return _TranslationRow(
                            translation: row,
                            onEdit: () => _editTranslation(row),
                            onApprove: () =>
                                _approve(row['locale'] as String),
                            onDelete: () =>
                                _deleteTranslation(row['locale'] as String),
                          );
                        },
                      ),
                    ),
                  ),
                  const SizedBox(height: 8),
                  OutlinedButton.icon(
                    onPressed: () => _editTranslation(),
                    icon: const Icon(Icons.translate),
                    label: const Text('افزودن ترجمه'),
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

class _GalleryList extends StatelessWidget {
  const _GalleryList({required this.items, required this.onRemove});

  final List<dynamic> items;
  final void Function(int index) onRemove;

  @override
  Widget build(BuildContext context) => ListView.separated(
        itemCount: items.length,
        separatorBuilder: (_, __) => const Divider(height: 1),
        itemBuilder: (BuildContext context, int index) {
          final Map<String, dynamic> item = items[index] as Map<String, dynamic>;
          return ListTile(
            leading: CircleAvatar(child: Text('${index + 1}')),
            title: Text(
              item['caption'] as String? ?? 'بدون توضیح',
              maxLines: 2,
              overflow: TextOverflow.ellipsis,
            ),
            subtitle: Text(
              item['media_id'] as String? ?? '',
              textDirection: TextDirection.ltr,
              style: Theme.of(context).textTheme.bodySmall,
            ),
            trailing: IconButton(
              onPressed: () => onRemove(index),
              icon: const Icon(Icons.delete_outline),
            ),
          );
        },
      );
}

class _TranslationRow extends StatelessWidget {
  const _TranslationRow({
    required this.translation,
    required this.onEdit,
    required this.onApprove,
    required this.onDelete,
  });

  final Map<String, dynamic> translation;
  final VoidCallback onEdit;
  final VoidCallback onApprove;
  final VoidCallback onDelete;

  @override
  Widget build(BuildContext context) {
    final ColorScheme colors = Theme.of(context).colorScheme;
    final bool approved = translation['approved_by'] != null;
    final String source = translation['source'] as String? ?? 'human';

    return ListTile(
      leading: CircleAvatar(
        child: Text((translation['locale'] as String? ?? '??').toUpperCase()),
      ),
      title: Text(translation['title'] as String? ?? '—'),
      subtitle: Row(
        children: <Widget>[
          // Which of the two it is decides whether a reader is looking at a
          // translation or at a draft of one, so it is on the row rather than
          // buried in the editor.
          Chip(
            label: Text(source == 'machine' ? 'ماشینی' : 'انسانی'),
            visualDensity: VisualDensity.compact,
          ),
          const SizedBox(width: 8),
          Chip(
            label: Text(approved ? 'تأییدشده' : 'در انتظار تأیید'),
            side: BorderSide(
              color: approved ? colors.primary : colors.tertiary,
            ),
            visualDensity: VisualDensity.compact,
          ),
        ],
      ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          if (!approved)
            TextButton(onPressed: onApprove, child: const Text('تأیید')),
          IconButton(onPressed: onEdit, icon: const Icon(Icons.edit_outlined)),
          IconButton(
            onPressed: onDelete,
            icon: const Icon(Icons.delete_outline),
          ),
        ],
      ),
    );
  }
}

class _GalleryDraft {
  const _GalleryDraft({required this.mediaId, required this.caption});

  final String mediaId;
  final String caption;
}

class _GalleryItemDialog extends StatefulWidget {
  const _GalleryItemDialog();

  @override
  State<_GalleryItemDialog> createState() => _GalleryItemDialogState();
}

class _GalleryItemDialogState extends State<_GalleryItemDialog> {
  final TextEditingController _mediaId = TextEditingController();
  final TextEditingController _caption = TextEditingController();

  @override
  void dispose() {
    _mediaId.dispose();
    _caption.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
        title: const Text('افزودن تصویر'),
        content: SizedBox(
          width: 420,
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: <Widget>[
              TextField(
                controller: _mediaId,
                autofocus: true,
                textDirection: TextDirection.ltr,
                decoration: const InputDecoration(
                  labelText: 'شناسهٔ رسانه',
                  helperText: 'همان شناسه‌ای که هنگام بارگذاری داده می‌شود',
                ),
                onChanged: (_) => setState(() {}),
              ),
              const SizedBox(height: 12),
              TextField(
                controller: _caption,
                decoration: const InputDecoration(labelText: 'توضیح تصویر'),
              ),
            ],
          ),
        ),
        actions: <Widget>[
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('انصراف'),
          ),
          FilledButton(
            onPressed: _mediaId.text.trim().isEmpty
                ? null
                : () => Navigator.of(context).pop(
                      _GalleryDraft(
                        mediaId: _mediaId.text.trim(),
                        caption: _caption.text.trim(),
                      ),
                    ),
            child: const Text('افزودن'),
          ),
        ],
      );
}

class _TranslationDraft {
  const _TranslationDraft({
    required this.locale,
    required this.title,
    required this.subtitle,
    required this.body,
    required this.source,
  });

  final String locale;
  final String title;
  final String subtitle;
  final String body;
  final String source;
}

class _TranslationDialog extends StatefulWidget {
  const _TranslationDialog({this.existing});

  final Map<String, dynamic>? existing;

  @override
  State<_TranslationDialog> createState() => _TranslationDialogState();
}

class _TranslationDialogState extends State<_TranslationDialog> {
  late final TextEditingController _title = TextEditingController(
    text: widget.existing?['title'] as String? ?? '',
  );
  late final TextEditingController _subtitle = TextEditingController(
    text: widget.existing?['subtitle'] as String? ?? '',
  );
  late final TextEditingController _body = TextEditingController(
    text: widget.existing?['body'] as String? ?? '',
  );
  late String _locale = widget.existing?['locale'] as String? ?? 'en';
  late String _source = widget.existing?['source'] as String? ?? 'human';

  /// The locales the app itself ships. Offering a free-text field would let a
  /// translation be filed under a language no client can ever ask for.
  static const List<String> _locales = <String>['fa', 'en', 'ar', 'tr'];

  @override
  void dispose() {
    _title.dispose();
    _subtitle.dispose();
    _body.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) => AlertDialog(
        title: Text(
          widget.existing == null ? 'افزودن ترجمه' : 'ویرایش ترجمه',
        ),
        content: SizedBox(
          width: 520,
          child: SingleChildScrollView(
            child: Column(
              mainAxisSize: MainAxisSize.min,
              crossAxisAlignment: CrossAxisAlignment.start,
              children: <Widget>[
                Row(
                  children: <Widget>[
                    DropdownButton<String>(
                      value: _locale,
                      // The locale is the key of the record, so changing it on
                      // an existing translation would create a second one
                      // rather than rename this.
                      onChanged: widget.existing != null
                          ? null
                          : (String? value) =>
                              setState(() => _locale = value ?? 'en'),
                      items: <DropdownMenuItem<String>>[
                        for (final String locale in _locales)
                          DropdownMenuItem<String>(
                            value: locale,
                            child: Text(locale.toUpperCase()),
                          ),
                      ],
                    ),
                    const SizedBox(width: 24),
                    DropdownButton<String>(
                      value: _source,
                      onChanged: (String? value) =>
                          setState(() => _source = value ?? 'human'),
                      items: const <DropdownMenuItem<String>>[
                        DropdownMenuItem<String>(
                          value: 'human',
                          child: Text('ترجمهٔ انسانی'),
                        ),
                        DropdownMenuItem<String>(
                          value: 'machine',
                          child: Text('ترجمهٔ ماشینی'),
                        ),
                      ],
                    ),
                  ],
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _title,
                  decoration: const InputDecoration(labelText: 'عنوان'),
                  onChanged: (_) => setState(() {}),
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _subtitle,
                  decoration: const InputDecoration(labelText: 'زیرعنوان'),
                ),
                const SizedBox(height: 12),
                TextField(
                  controller: _body,
                  maxLines: 8,
                  decoration: const InputDecoration(labelText: 'متن'),
                ),
              ],
            ),
          ),
        ),
        actions: <Widget>[
          TextButton(
            onPressed: () => Navigator.of(context).pop(),
            child: const Text('انصراف'),
          ),
          FilledButton(
            onPressed: _title.text.trim().isEmpty
                ? null
                : () => Navigator.of(context).pop(
                      _TranslationDraft(
                        locale: _locale,
                        title: _title.text.trim(),
                        subtitle: _subtitle.text.trim(),
                        body: _body.text,
                        source: _source,
                      ),
                    ),
            child: const Text('ذخیره'),
          ),
        ],
      );
}
