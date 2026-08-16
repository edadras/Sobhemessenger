import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// The newsroom queue (§25).
class EditorialPage extends ConsumerStatefulWidget {
  const EditorialPage({super.key});

  @override
  ConsumerState<EditorialPage> createState() => _EditorialPageState();
}

class _EditorialPageState extends ConsumerState<EditorialPage> {
  String _status = 'review';
  int _reloadToken = 0;

  Future<void> _setStatus(String articleId, String status) async {
    final AdminApi api = ref.read(adminApiProvider);
    try {
      await api.setArticleStatus(articleId, status);
      if (mounted) {
        setState(() => _reloadToken++);
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
              Text('اخبار', style: Theme.of(context).textTheme.headlineSmall),
              const Spacer(),
              SegmentedButton<String>(
                segments: const <ButtonSegment<String>>[
                  ButtonSegment<String>(value: 'draft', label: Text('پیش‌نویس')),
                  ButtonSegment<String>(value: 'review', label: Text('بازبینی')),
                  ButtonSegment<String>(value: 'scheduled', label: Text('زمان‌بندی')),
                  ButtonSegment<String>(value: 'published', label: Text('منتشرشده')),
                ],
                selected: <String>{_status},
                onSelectionChanged: (Set<String> selection) =>
                    setState(() => _status = selection.first),
              ),
            ],
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<String>('$_status|$_reloadToken'),
              future: api.editorialArticles(status: _status),
              emptyMessage: 'مطلبی در این وضعیت وجود ندارد',
              builder: (BuildContext context, List<dynamic> articles) => ListView.separated(
                itemCount: articles.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> article =
                      articles[index] as Map<String, dynamic>;
                  return ListTile(
                    leading: article['is_breaking'] == true
                        ? const Icon(Icons.priority_high, color: Colors.red)
                        : const Icon(Icons.article_outlined),
                    title: Text(article['title'] as String? ?? '—'),
                    subtitle: Text(
                      '${article['category_name'] ?? 'بدون دسته'} · '
                      '${article['author_name'] ?? 'بدون نویسنده'} · '
                      '${article['reading_minutes']} دقیقه',
                    ),
                    trailing: _actionsFor(article),
                  );
                },
              ),
            ),
          ),
        ],
      ),
    );
  }

  /// The available actions follow the editorial state machine, so the panel
  /// never offers a transition the API would reject.
  Widget? _actionsFor(Map<String, dynamic> article) {
    final String id = article['id'] as String;
    return switch (_status) {
      'draft' => TextButton(
          onPressed: () => _setStatus(id, 'review'),
          child: const Text('ارسال برای بازبینی'),
        ),
      'review' => Row(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            TextButton(
              onPressed: () => _setStatus(id, 'draft'),
              child: const Text('بازگشت به پیش‌نویس'),
            ),
            FilledButton(
              onPressed: () => _setStatus(id, 'published'),
              child: const Text('انتشار'),
            ),
          ],
        ),
      'scheduled' => FilledButton(
          onPressed: () => _setStatus(id, 'published'),
          child: const Text('انتشار فوری'),
        ),
      'published' => TextButton(
          onPressed: () => _setStatus(id, 'archived'),
          child: const Text('بایگانی'),
        ),
      _ => null,
    };
  }
}
