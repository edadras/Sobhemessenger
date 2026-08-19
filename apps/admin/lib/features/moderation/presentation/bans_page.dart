import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// Bans, and the search indices behind them (§31).
///
/// A ban is not the same thing as an account status. Setting an account to
/// `banned` closes that account; a ban record is a rule with a scope and an
/// end date — this person out of that chat until Friday — and it is the ban
/// records the enforcement path actually reads. The panel had the first and
/// not the second, so a moderator could only ever reach for the blunt one.
class BansPage extends ConsumerStatefulWidget {
  const BansPage({super.key});

  @override
  ConsumerState<BansPage> createState() => _BansPageState();
}

class _BansPageState extends ConsumerState<BansPage> {
  int _reloadToken = 0;

  void _reload() => setState(() => _reloadToken++);

  void _report(AdminApiException error) {
    if (mounted) {
      ScaffoldMessenger.of(context)
          .showSnackBar(SnackBar(content: Text(error.message)));
    }
  }

  Future<void> _create() async {
    final _BanDraft? draft = await showDialog<_BanDraft>(
      context: context,
      builder: (BuildContext context) => const _BanDialog(),
    );
    if (draft == null) {
      return;
    }

    try {
      await ref.read(adminApiProvider).createBan(
            scope: draft.scope,
            reason: draft.reason,
            userId: draft.userId,
            chatId: draft.chatId,
            expiresAt: draft.expiresAt,
          );
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  Future<void> _lift(String banId) async {
    try {
      await ref.read(adminApiProvider).liftBan(banId);
      _reload();
    } on AdminApiException catch (error) {
      _report(error);
    }
  }

  /// Rebuilding an index walks the whole table, so it asks first — and says
  /// what it is for, because "reindex" on its own invites clicking.
  Future<void> _reindex(String index) async {
    final bool confirmed = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => AlertDialog(
            title: Text('بازسازی نمایهٔ «$index»'),
            content: const Text(
              'کل جدول از پایگاه داده خوانده و دوباره نمایه‌سازی می‌شود. '
              'پایگاه داده مرجع است و نمایه رونوشتی از آن؛ این کار برای وقتی '
              'است که رونوشت عقب مانده باشد — پس از بازیابی پشتیبان، یا بازه‌ای '
              'که نمایه‌ساز بالا نبوده. روی داده‌های زیاد کند است.',
            ),
            actions: <Widget>[
              TextButton(
                onPressed: () => Navigator.of(context).pop(false),
                child: const Text('انصراف'),
              ),
              FilledButton(
                onPressed: () => Navigator.of(context).pop(true),
                child: const Text('بازسازی'),
              ),
            ],
          ),
        ) ??
        false;
    if (!confirmed) {
      return;
    }

    try {
      await ref.read(adminApiProvider).reindex(index);
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text('بازسازی نمایهٔ «$index» آغاز شد')),
        );
      }
    } on AdminApiException catch (error) {
      _report(error);
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
                'محرومیت‌ها',
                style: Theme.of(context).textTheme.headlineSmall,
              ),
              const Spacer(),
              PopupMenuButton<String>(
                onSelected: _reindex,
                icon: const Icon(Icons.manage_search),
                tooltip: 'بازسازی نمایهٔ جست‌وجو',
                itemBuilder: (BuildContext context) =>
                    const <PopupMenuEntry<String>>[
                  PopupMenuItem<String>(
                    value: 'messages',
                    child: Text('پیام‌ها'),
                  ),
                  PopupMenuItem<String>(value: 'users', child: Text('کاربران')),
                  PopupMenuItem<String>(value: 'chats', child: Text('گفتگوها')),
                  PopupMenuItem<String>(value: 'news', child: Text('اخبار')),
                ],
              ),
              const SizedBox(width: 8),
              FilledButton.icon(
                onPressed: _create,
                icon: const Icon(Icons.gavel),
                label: const Text('محرومیت تازه'),
              ),
            ],
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<int>(_reloadToken),
              future: api.bans(),
              emptyMessage: 'محرومیتی ثبت نشده',
              builder: (BuildContext context, List<dynamic> bans) =>
                  ListView.separated(
                itemCount: bans.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> ban =
                      bans[index] as Map<String, dynamic>;
                  final String? expires = ban['expires_at'] as String?;
                  return ListTile(
                    leading: const Icon(Icons.gavel),
                    title: Text(_describe(ban)),
                    subtitle: Text(
                      <String>[
                        ban['reason'] as String? ?? '—',
                        // Indefinite and "until Friday" are different
                        // decisions, so the row says which this is.
                        if (expires == null)
                          'بدون پایان'
                        else
                          'تا $expires',
                      ].join(' · '),
                    ),
                    trailing: TextButton(
                      onPressed: () => _lift(ban['id'] as String),
                      child: const Text('برداشتن'),
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

  static String _describe(Map<String, dynamic> ban) {
    final String scope = ban['scope'] as String? ?? 'global';
    final String? userId = ban['user_id'] as String?;
    final String? chatId = ban['chat_id'] as String?;
    return switch (scope) {
      'chat' => 'گفتگوی $chatId',
      'chat_user' => 'کاربر $userId در گفتگوی $chatId',
      _ => 'کاربر ${userId ?? '—'} در کل سامانه',
    };
  }
}

class _BanDraft {
  const _BanDraft({
    required this.scope,
    required this.reason,
    this.userId,
    this.chatId,
    this.expiresAt,
  });

  final String scope;
  final String reason;
  final String? userId;
  final String? chatId;
  final DateTime? expiresAt;
}

class _BanDialog extends StatefulWidget {
  const _BanDialog();

  @override
  State<_BanDialog> createState() => _BanDialogState();
}

class _BanDialogState extends State<_BanDialog> {
  final TextEditingController _userId = TextEditingController();
  final TextEditingController _chatId = TextEditingController();
  final TextEditingController _reason = TextEditingController();
  String _scope = 'global';
  int _days = 0;

  @override
  void dispose() {
    _userId.dispose();
    _chatId.dispose();
    _reason.dispose();
    super.dispose();
  }

  bool get _needsUser => _scope != 'chat';
  bool get _needsChat => _scope != 'global';

  bool get _complete =>
      _reason.text.trim().isNotEmpty &&
      (!_needsUser || _userId.text.trim().isNotEmpty) &&
      (!_needsChat || _chatId.text.trim().isNotEmpty);

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: const Text('محرومیت تازه'),
      content: SizedBox(
        width: 460,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          crossAxisAlignment: CrossAxisAlignment.start,
          children: <Widget>[
            SegmentedButton<String>(
              segments: const <ButtonSegment<String>>[
                ButtonSegment<String>(
                  value: 'global',
                  label: Text('کل سامانه'),
                ),
                ButtonSegment<String>(value: 'chat', label: Text('یک گفتگو')),
                ButtonSegment<String>(
                  value: 'chat_user',
                  label: Text('در یک گفتگو'),
                ),
              ],
              selected: <String>{_scope},
              onSelectionChanged: (Set<String> value) =>
                  setState(() => _scope = value.first),
            ),
            const SizedBox(height: 16),
            if (_needsUser)
              TextField(
                controller: _userId,
                textDirection: TextDirection.ltr,
                decoration: const InputDecoration(labelText: 'شناسهٔ کاربر'),
                onChanged: (_) => setState(() {}),
              ),
            if (_needsChat) ...<Widget>[
              const SizedBox(height: 12),
              TextField(
                controller: _chatId,
                textDirection: TextDirection.ltr,
                decoration: const InputDecoration(labelText: 'شناسهٔ گفتگو'),
                onChanged: (_) => setState(() {}),
              ),
            ],
            const SizedBox(height: 12),
            TextField(
              controller: _reason,
              decoration: const InputDecoration(
                labelText: 'دلیل (در گزارش ممیزی ثبت می‌شود)',
              ),
              onChanged: (_) => setState(() {}),
            ),
            const SizedBox(height: 16),
            // Zero means indefinite, which is the default because a moderator
            // choosing a duration should have to choose it.
            Text('مدت: ${_days == 0 ? 'بدون پایان' : '$_days روز'}'),
            Slider(
              value: _days.toDouble(),
              max: 90,
              divisions: 90,
              onChanged: (double value) =>
                  setState(() => _days = value.round()),
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
          onPressed: _complete
              ? () => Navigator.of(context).pop(
                    _BanDraft(
                      scope: _scope,
                      reason: _reason.text.trim(),
                      userId: _needsUser ? _userId.text.trim() : null,
                      chatId: _needsChat ? _chatId.text.trim() : null,
                      expiresAt: _days == 0
                          ? null
                          : DateTime.now().add(Duration(days: _days)),
                    ),
                  )
              : null,
          child: const Text('اعمال'),
        ),
      ],
    );
  }
}
