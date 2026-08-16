import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// User administration: search, inspect and change account status (§31).
class UsersPage extends ConsumerStatefulWidget {
  const UsersPage({super.key});

  @override
  ConsumerState<UsersPage> createState() => _UsersPageState();
}

class _UsersPageState extends ConsumerState<UsersPage> {
  final TextEditingController _search = TextEditingController();
  String _query = '';
  String _status = '';
  int _reloadToken = 0;

  @override
  void dispose() {
    _search.dispose();
    super.dispose();
  }

  void _reload() => setState(() => _reloadToken++);

  Future<void> _changeStatus(String userId, String status) async {
    final AdminApi api = ref.read(adminApiProvider);
    final String? reason = await showDialog<String>(
      context: context,
      builder: (BuildContext context) => _ReasonDialog(status: status),
    );
    if (reason == null) {
      return;
    }

    try {
      await api.setUserStatus(userId, status, reason);
      if (mounted) {
        _reload();
      }
    } on AdminApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context).showSnackBar(
          SnackBar(content: Text(error.message)),
        );
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
          Text('کاربران', style: Theme.of(context).textTheme.headlineSmall),
          const SizedBox(height: 16),
          Row(
            children: <Widget>[
              Expanded(
                child: TextField(
                  controller: _search,
                  decoration: const InputDecoration(
                    hintText: 'جستجو بر اساس نام کاربری، نام یا شماره',
                    prefixIcon: Icon(Icons.search),
                  ),
                  onSubmitted: (String value) => setState(() => _query = value),
                ),
              ),
              const SizedBox(width: 16),
              DropdownButton<String>(
                value: _status,
                items: const <DropdownMenuItem<String>>[
                  DropdownMenuItem<String>(value: '', child: Text('همه')),
                  DropdownMenuItem<String>(value: 'active', child: Text('فعال')),
                  DropdownMenuItem<String>(value: 'restricted', child: Text('محدود')),
                  DropdownMenuItem<String>(value: 'banned', child: Text('مسدود')),
                ],
                onChanged: (String? value) => setState(() => _status = value ?? ''),
              ),
            ],
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<String>('$_query|$_status|$_reloadToken'),
              future: api.users(query: _query, status: _status),
              emptyMessage: 'کاربری یافت نشد',
              builder: (BuildContext context, List<dynamic> users) => ListView.separated(
                itemCount: users.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> user = users[index] as Map<String, dynamic>;
                  return _UserRow(
                    user: user,
                    onChangeStatus: (String status) =>
                        _changeStatus(user['id'] as String, status),
                  );
                },
              ),
            ),
          ),
        ],
      ),
    );
  }
}

class _UserRow extends StatelessWidget {
  const _UserRow({required this.user, required this.onChangeStatus});

  final Map<String, dynamic> user;
  final void Function(String status) onChangeStatus;

  @override
  Widget build(BuildContext context) {
    final String status = user['status'] as String? ?? 'active';
    final List<dynamic> roles = user['admin_roles'] as List<dynamic>? ?? const <dynamic>[];

    return ListTile(
      leading: CircleAvatar(child: Text(_initial(user))),
      title: Row(
        children: <Widget>[
          Text(user['display_name'] as String? ?? '—'),
          if (user['username'] != null) ...<Widget>[
            const SizedBox(width: 8),
            Text('@${user['username']}', style: Theme.of(context).textTheme.bodySmall),
          ],
          if (roles.isNotEmpty) ...<Widget>[
            const SizedBox(width: 8),
            Chip(
              label: Text(roles.join(', '), style: Theme.of(context).textTheme.labelSmall),
              visualDensity: VisualDensity.compact,
            ),
          ],
        ],
      ),
      subtitle: Text(
        // The phone number arrives masked from the API; the panel never has
        // the full number to leak (§60).
        '${user['phone_masked']} · ${user['device_count']} دستگاه · '
        '${user['message_count']} پیام',
      ),
      trailing: Row(
        mainAxisSize: MainAxisSize.min,
        children: <Widget>[
          _StatusChip(status: status),
          const SizedBox(width: 8),
          PopupMenuButton<String>(
            onSelected: onChangeStatus,
            itemBuilder: (BuildContext context) => const <PopupMenuEntry<String>>[
              PopupMenuItem<String>(value: 'active', child: Text('فعال‌سازی')),
              PopupMenuItem<String>(value: 'restricted', child: Text('محدودسازی')),
              PopupMenuItem<String>(value: 'banned', child: Text('مسدودسازی')),
            ],
          ),
        ],
      ),
    );
  }

  static String _initial(Map<String, dynamic> user) {
    final String name = user['display_name'] as String? ?? '';
    return name.isEmpty ? '?' : name.characters.first;
  }
}

class _StatusChip extends StatelessWidget {
  const _StatusChip({required this.status});

  final String status;

  @override
  Widget build(BuildContext context) {
    final ColorScheme colors = Theme.of(context).colorScheme;
    final (String label, Color color) = switch (status) {
      'banned' => ('مسدود', colors.error),
      'restricted' => ('محدود', colors.tertiary),
      'deleted' => ('حذف‌شده', colors.outline),
      _ => ('فعال', colors.primary),
    };

    return Chip(
      label: Text(label),
      side: BorderSide(color: color),
      visualDensity: VisualDensity.compact,
    );
  }
}

/// Every status change requires a reason, because it lands in the audit log
/// and "why" is the part an investigation actually needs.
class _ReasonDialog extends StatefulWidget {
  const _ReasonDialog({required this.status});

  final String status;

  @override
  State<_ReasonDialog> createState() => _ReasonDialogState();
}

class _ReasonDialogState extends State<_ReasonDialog> {
  final TextEditingController _controller = TextEditingController();

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text('تغییر وضعیت به «${widget.status}»'),
      content: TextField(
        controller: _controller,
        autofocus: true,
        decoration: const InputDecoration(labelText: 'دلیل (در گزارش ممیزی ثبت می‌شود)'),
      ),
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('انصراف'),
        ),
        FilledButton(
          onPressed: () => Navigator.of(context).pop(_controller.text.trim()),
          child: const Text('تأیید'),
        ),
      ],
    );
  }
}
