import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// The bylines articles can be filed under (§25).
///
/// An author is not an account. A wire service, a desk, or a columnist who has
/// never signed in still needs a byline, so linking to a user is optional —
/// and when it is linked, following the author in the app follows this record
/// rather than the person's chat profile.
class AuthorsPage extends ConsumerStatefulWidget {
  const AuthorsPage({super.key});

  @override
  ConsumerState<AuthorsPage> createState() => _AuthorsPageState();
}

class _AuthorsPageState extends ConsumerState<AuthorsPage> {
  int _reloadToken = 0;

  void _reload() => setState(() => _reloadToken++);

  Future<void> _edit([Map<String, dynamic>? existing]) async {
    final _AuthorDraft? draft = await showDialog<_AuthorDraft>(
      context: context,
      builder: (BuildContext context) => _AuthorDialog(existing: existing),
    );
    if (draft == null) {
      return;
    }

    try {
      await ref.read(adminApiProvider).saveAuthor(
            id: existing?['id'] as String?,
            displayName: draft.displayName,
            userId: draft.userId,
            bio: draft.bio,
            isActive: draft.isActive,
          );
      if (mounted) {
        _reload();
      }
    } on AdminApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AdminApi api = ref.watch(adminApiProvider);

    return Padding(
      padding: const EdgeInsets.all(24),
      child: Column(
        crossAxisAlignment: CrossAxisAlignment.start,
        children: <Widget>[
          Row(
            children: <Widget>[
              Text(
                'نویسندگان',
                style: Theme.of(context).textTheme.headlineSmall,
              ),
              const Spacer(),
              FilledButton.icon(
                onPressed: _edit,
                icon: const Icon(Icons.add),
                label: const Text('نویسندهٔ تازه'),
              ),
            ],
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<int>(_reloadToken),
              future: api.authors(),
              emptyMessage: 'هنوز نویسنده‌ای ثبت نشده',
              builder: (BuildContext context, List<dynamic> authors) =>
                  ListView.separated(
                itemCount: authors.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> author =
                      authors[index] as Map<String, dynamic>;
                  final bool active = author['is_active'] as bool? ?? true;
                  return ListTile(
                    leading: CircleAvatar(
                      child: Text(_initial(author['display_name'] as String?)),
                    ),
                    title: Text(author['display_name'] as String? ?? '—'),
                    subtitle: Text(
                      author['bio'] as String? ??
                          (author['user_id'] == null
                              ? 'بدون حساب کاربری'
                              : 'متصل به یک حساب'),
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                    ),
                    trailing: Row(
                      mainAxisSize: MainAxisSize.min,
                      children: <Widget>[
                        // An author is retired rather than deleted: articles
                        // they wrote keep their byline, and an inactive author
                        // simply stops being offered for new ones.
                        Chip(
                          label: Text(active ? 'فعال' : 'بازنشسته'),
                          visualDensity: VisualDensity.compact,
                        ),
                        IconButton(
                          onPressed: () => _edit(author),
                          icon: const Icon(Icons.edit_outlined),
                        ),
                      ],
                    ),
                  );
                },
              ),
            ),
          ),
        ],
      ),
    );
  }

  static String _initial(String? name) =>
      (name == null || name.isEmpty) ? '?' : name.characters.first;
}

class _AuthorDraft {
  const _AuthorDraft({
    required this.displayName,
    required this.bio,
    required this.isActive,
    this.userId,
  });

  final String displayName;
  final String bio;
  final bool isActive;
  final String? userId;
}

class _AuthorDialog extends StatefulWidget {
  const _AuthorDialog({this.existing});

  final Map<String, dynamic>? existing;

  @override
  State<_AuthorDialog> createState() => _AuthorDialogState();
}

class _AuthorDialogState extends State<_AuthorDialog> {
  late final TextEditingController _name = TextEditingController(
    text: widget.existing?['display_name'] as String? ?? '',
  );
  late final TextEditingController _bio = TextEditingController(
    text: widget.existing?['bio'] as String? ?? '',
  );
  late final TextEditingController _userId = TextEditingController(
    text: widget.existing?['user_id'] as String? ?? '',
  );
  late bool _active = widget.existing?['is_active'] as bool? ?? true;

  @override
  void dispose() {
    _name.dispose();
    _bio.dispose();
    _userId.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text(widget.existing == null ? 'نویسندهٔ تازه' : 'ویرایش نویسنده'),
      content: SizedBox(
        width: 420,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            TextField(
              controller: _name,
              autofocus: true,
              decoration: const InputDecoration(labelText: 'نام نمایشی'),
              onChanged: (_) => setState(() {}),
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _bio,
              maxLines: 3,
              decoration: const InputDecoration(labelText: 'معرفی کوتاه'),
            ),
            const SizedBox(height: 12),
            TextField(
              controller: _userId,
              textDirection: TextDirection.ltr,
              decoration: const InputDecoration(
                labelText: 'شناسهٔ کاربر (اختیاری)',
                helperText: 'برای نویسنده‌ای که حساب دارد',
              ),
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
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('انصراف'),
        ),
        FilledButton(
          onPressed: _name.text.trim().isEmpty
              ? null
              : () {
                  final String userId = _userId.text.trim();
                  Navigator.of(context).pop(
                    _AuthorDraft(
                      displayName: _name.text.trim(),
                      bio: _bio.text.trim(),
                      isActive: _active,
                      userId: userId.isEmpty ? null : userId,
                    ),
                  );
                },
          child: const Text('ذخیره'),
        ),
      ],
    );
  }
}
