import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// The categories articles are filed under (§25).
///
/// A category is one thing with a name in each language the app ships, not one
/// category per translation — which is why the editor takes four names and one
/// slug. The slug is what the upsert keys on and what a URL carries, so it is
/// fixed once the category exists.
class CategoriesPage extends ConsumerStatefulWidget {
  const CategoriesPage({super.key});

  @override
  ConsumerState<CategoriesPage> createState() => _CategoriesPageState();
}

class _CategoriesPageState extends ConsumerState<CategoriesPage> {
  int _reloadToken = 0;

  void _reload() => setState(() => _reloadToken++);

  void _report(AdminApiException error) {
    if (mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(error.message)));
    }
  }

  Future<void> _edit([Map<String, dynamic>? existing]) async {
    final _CategoryDraft? draft = await showDialog<_CategoryDraft>(
      context: context,
      builder: (BuildContext context) => _CategoryDialog(existing: existing),
    );
    if (draft == null) {
      return;
    }

    try {
      await ref.read(adminApiProvider).saveCategory(
            slug: draft.slug,
            names: draft.names,
            position: draft.position,
            isActive: draft.isActive,
          );
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  Future<void> _delete(Map<String, dynamic> category) async {
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: const Text('حذف دسته؟'),
            // The server refuses a category that still has articles, so this
            // says what will happen rather than pre-judging it here.
            content: Text(
              'دستهٔ «${category['name'] ?? category['slug']}» حذف می‌شود. '
              'اگر مطلبی زیر آن باشد، سرور حذف را رد می‌کند.',
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: const Text('انصراف'),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: const Text('حذف'),
              ),
            ],
          ),
        ) ??
        false;
    if (!confirmed) {
      return;
    }

    try {
      await ref.read(adminApiProvider).deleteCategory(category['id'] as String);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  @override
  Widget build(BuildContext context) {
    final AdminApi api = ref.watch(adminApiProvider);

    return Scaffold(
      appBar: AppBar(
        title: const Text('دسته‌های خبری'),
        actions: <Widget>[
          IconButton(
            onPressed: _edit,
            icon: const Icon(Icons.add),
            tooltip: 'دستهٔ تازه',
          ),
        ],
      ),
      body: Padding(
        padding: const EdgeInsets.all(24),
        child: AsyncSection<List<dynamic>>(
          key: ValueKey<int>(_reloadToken),
          future: api.categories(),
          emptyMessage: 'دسته‌ای تعریف نشده',
          builder: (BuildContext context, List<dynamic> categories) =>
              ListView.separated(
            itemCount: categories.length,
            separatorBuilder: (_, __) => const Divider(height: 1),
            itemBuilder: (BuildContext context, int index) {
              final Map<String, dynamic> category =
                  categories[index] as Map<String, dynamic>;
              final bool active = category['is_active'] as bool? ?? true;
              return ListTile(
                leading: CircleAvatar(
                  child: Text('${category['position'] ?? 0}'),
                ),
                title: Text(
                  category['name'] as String? ??
                      category['slug'] as String? ??
                      '—',
                ),
                subtitle: Text(
                  category['slug'] as String? ?? '',
                  textDirection: TextDirection.ltr,
                ),
                trailing: Row(
                  mainAxisSize: MainAxisSize.min,
                  children: <Widget>[
                    if (!active)
                      const Chip(
                        label: Text('غیرفعال'),
                        visualDensity: VisualDensity.compact,
                      ),
                    IconButton(
                      onPressed: () => _edit(category),
                      icon: const Icon(Icons.edit_outlined),
                    ),
                    IconButton(
                      onPressed: () => _delete(category),
                      icon: const Icon(Icons.delete_outline),
                    ),
                  ],
                ),
              );
            },
          ),
        ),
      ),
    );
  }
}

class _CategoryDraft {
  const _CategoryDraft({
    required this.slug,
    required this.names,
    required this.position,
    required this.isActive,
  });

  final String slug;
  final Map<String, String> names;
  final int position;
  final bool isActive;
}

class _CategoryDialog extends StatefulWidget {
  const _CategoryDialog({this.existing});

  final Map<String, dynamic>? existing;

  @override
  State<_CategoryDialog> createState() => _CategoryDialogState();
}

class _CategoryDialogState extends State<_CategoryDialog> {
  /// The locales the app ships. A name in a language no client asks for would
  /// be stored and never read.
  static const List<String> _locales = <String>['fa', 'en', 'ar', 'tr'];

  late final TextEditingController _slug = TextEditingController(
    text: widget.existing?['slug'] as String? ?? '',
  );
  late final Map<String, TextEditingController> _names =
      <String, TextEditingController>{
    for (final String locale in _locales)
      locale: TextEditingController(
        text: (widget.existing?['names'] as Map<String, dynamic>?)?[locale]
                as String? ??
            '',
      ),
  };
  late int _position = (widget.existing?['position'] as num?)?.toInt() ?? 0;
  late bool _active = widget.existing?['is_active'] as bool? ?? true;

  @override
  void dispose() {
    _slug.dispose();
    for (final TextEditingController controller in _names.values) {
      controller.dispose();
    }
    super.dispose();
  }

  /// A category needs its slug and at least the Persian name — the app's own
  /// language, and the one every reader falls back to.
  bool get _complete =>
      _slug.text.trim().isNotEmpty && _names['fa']!.text.trim().isNotEmpty;

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text(widget.existing == null ? 'دستهٔ تازه' : 'ویرایش دسته'),
      content: SizedBox(
        width: 460,
        child: SingleChildScrollView(
          child: Column(
            mainAxisSize: MainAxisSize.min,
            children: <Widget>[
              TextField(
                controller: _slug,
                // The slug keys the upsert and appears in URLs, so changing it
                // on an existing category would create a second one.
                enabled: widget.existing == null,
                autofocus: widget.existing == null,
                textDirection: TextDirection.ltr,
                decoration: const InputDecoration(
                  labelText: 'شناسهٔ نشانی (slug)',
                  helperText: 'در نشانی اینترنتی ظاهر می‌شود و بعداً ثابت است',
                ),
                onChanged: (_) => setState(() {}),
              ),
              for (final String locale in _locales) ...<Widget>[
                const SizedBox(height: 12),
                TextField(
                  controller: _names[locale],
                  decoration: InputDecoration(
                    labelText: 'نام (${locale.toUpperCase()})',
                  ),
                  onChanged: (_) => setState(() {}),
                ),
              ],
              const SizedBox(height: 16),
              Text('ترتیب نمایش: $_position'),
              Slider(
                value: _position.toDouble(),
                max: 50,
                divisions: 50,
                onChanged: (double value) =>
                    setState(() => _position = value.round()),
              ),
              SwitchListTile(
                value: _active,
                onChanged: (bool value) => setState(() => _active = value),
                title: const Text('فعال'),
                contentPadding: EdgeInsets.zero,
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
          onPressed: _complete
              ? () => Navigator.of(context).pop(
                    _CategoryDraft(
                      slug: _slug.text.trim(),
                      names: <String, String>{
                        for (final MapEntry<String, TextEditingController> e
                            in _names.entries)
                          if (e.value.text.trim().isNotEmpty)
                            e.key: e.value.text.trim(),
                      },
                      position: _position,
                      isActive: _active,
                    ),
                  )
              : null,
          child: const Text('ذخیره'),
        ),
      ],
    );
  }
}
