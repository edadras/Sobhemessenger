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

  /// Grants and revokes operator roles.
  ///
  /// The panel could show which roles someone held and not change them, so an
  /// operator was appointed by editing the database. What each role may do is
  /// the role's business — the panel names it, the server holds the permission
  /// list, and a role added there appears here without a release.
  Future<void> _manageRoles(Map<String, dynamic> user) async {
    final AdminApi api = ref.read(adminApiProvider);
    final String userId = user['id'] as String;
    final List<String> held = <String>[
      for (final dynamic role
          in user['admin_roles'] as List<dynamic>? ?? const <dynamic>[])
        role as String,
    ];

    final _RoleChange? change = await showDialog<_RoleChange>(
      context: context,
      builder: (BuildContext context) => _RolesDialog(
        name: user['display_name'] as String? ?? userId,
        held: held,
      ),
    );
    if (change == null) {
      return;
    }

    try {
      if (change.grant) {
        await api.grantRole(userId, change.role);
      } else {
        await api.revokeRole(userId, change.role);
      }
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

  /// Shows the anti-spam score held against an account, and offers to lift it.
  ///
  /// The score is what silently restricts someone before any moderator has
  /// looked at them, so an account that "cannot send messages for no reason"
  /// is answered here rather than by guessing. Lifting is a moderation write
  /// and is refused for an operator without the permission.
  Future<void> _showSpamScore(Map<String, dynamic> user) async {
    final AdminApi api = ref.read(adminApiProvider);
    final String userId = user['id'] as String;

    final Map<String, dynamic> result;
    try {
      result = await api.spamScore(userId);
    } on AdminApiException catch (error) {
      if (mounted) {
        ScaffoldMessenger.of(context)
            .showSnackBar(SnackBar(content: Text(error.message)));
      }
      return;
    }
    if (!mounted) {
      return;
    }

    final bool lift = await showDialog<bool>(
          context: context,
          builder: (BuildContext context) => _SpamScoreDialog(
            name: user['display_name'] as String? ?? userId,
            result: result,
          ),
        ) ??
        false;
    if (!lift) {
      return;
    }

    try {
      await api.liftSpamScore(userId);
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
                  DropdownMenuItem<String>(
                    value: 'active',
                    child: Text('فعال'),
                  ),
                  DropdownMenuItem<String>(
                    value: 'restricted',
                    child: Text('محدود'),
                  ),
                  DropdownMenuItem<String>(
                    value: 'banned',
                    child: Text('مسدود'),
                  ),
                ],
                onChanged: (String? value) =>
                    setState(() => _status = value ?? ''),
              ),
            ],
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<String>('$_query|$_status|$_reloadToken'),
              future: api.users(query: _query, status: _status),
              emptyMessage: 'کاربری یافت نشد',
              builder: (BuildContext context, List<dynamic> users) =>
                  ListView.separated(
                itemCount: users.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> user =
                      users[index] as Map<String, dynamic>;
                  return _UserRow(
                    user: user,
                    onChangeStatus: (String status) =>
                        _changeStatus(user['id'] as String, status),
                    onShowSpamScore: () => _showSpamScore(user),
                    onManageRoles: () => _manageRoles(user),
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
  const _UserRow({
    required this.user,
    required this.onChangeStatus,
    required this.onShowSpamScore,
    required this.onManageRoles,
  });

  final Map<String, dynamic> user;
  final void Function(String status) onChangeStatus;
  final VoidCallback onShowSpamScore;
  final VoidCallback onManageRoles;

  @override
  Widget build(BuildContext context) {
    final String status = user['status'] as String? ?? 'active';
    final List<dynamic> roles =
        user['admin_roles'] as List<dynamic>? ?? const <dynamic>[];

    return ListTile(
      leading: CircleAvatar(child: Text(_initial(user))),
      title: Row(
        children: <Widget>[
          Text(user['display_name'] as String? ?? '—'),
          if (user['username'] != null) ...<Widget>[
            const SizedBox(width: 8),
            Text(
              '@${user['username']}',
              style: Theme.of(context).textTheme.bodySmall,
            ),
          ],
          if (roles.isNotEmpty) ...<Widget>[
            const SizedBox(width: 8),
            Chip(
              label: Text(
                roles.join(', '),
                style: Theme.of(context).textTheme.labelSmall,
              ),
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
          IconButton(
            onPressed: onShowSpamScore,
            icon: const Icon(Icons.report_gmailerrorred_outlined),
            tooltip: 'امتیاز ضدهرزنامه',
          ),
          IconButton(
            onPressed: onManageRoles,
            icon: const Icon(Icons.admin_panel_settings_outlined),
            tooltip: 'نقش‌های اپراتوری',
          ),
          const SizedBox(width: 8),
          PopupMenuButton<String>(
            onSelected: onChangeStatus,
            itemBuilder: (BuildContext context) =>
                const <PopupMenuEntry<String>>[
              PopupMenuItem<String>(value: 'active', child: Text('فعال‌سازی')),
              PopupMenuItem<String>(
                value: 'restricted',
                child: Text('محدودسازی'),
              ),
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
        decoration: const InputDecoration(
          labelText: 'دلیل (در گزارش ممیزی ثبت می‌شود)',
        ),
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

/// What the anti-spam system holds against one account.
///
/// The threshold is shown next to the score because a number on its own says
/// nothing: 40 means very different things depending on where the line is, and
/// the operator should not have to remember it.
class _SpamScoreDialog extends StatelessWidget {
  const _SpamScoreDialog({required this.name, required this.result});

  final String name;
  final Map<String, dynamic> result;

  @override
  Widget build(BuildContext context) {
    final ColorScheme colors = Theme.of(context).colorScheme;
    final Map<String, dynamic> score =
        result['score'] as Map<String, dynamic>? ?? <String, dynamic>{};
    final bool restricted = result['restricted'] as bool? ?? false;
    final int threshold = (result['threshold'] as num?)?.toInt() ?? 0;
    final int value = (score['score'] as num?)?.toInt() ?? 0;
    final String reason = score['reason'] as String? ?? '';
    final String? until = score['restricted_until'] as String?;

    return AlertDialog(
      title: Text('امتیاز ضدهرزنامه — $name'),
      content: Column(
        mainAxisSize: MainAxisSize.min,
        crossAxisAlignment: CrossAxisAlignment.start,
        children: <Widget>[
          Text('امتیاز: $value از آستانهٔ $threshold'),
          const SizedBox(height: 8),
          Text(
            restricted ? 'این حساب هم‌اکنون محدود است' : 'محدودیتی در کار نیست',
            style: TextStyle(
              color: restricted ? colors.error : colors.primary,
            ),
          ),
          if (until != null) ...<Widget>[
            const SizedBox(height: 8),
            Text('تا: $until'),
          ],
          if (reason.isNotEmpty) ...<Widget>[
            const SizedBox(height: 8),
            Text('دلیل: $reason'),
          ],
        ],
      ),
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(context).pop(false),
          child: const Text('بستن'),
        ),
        // Offered only when there is something to lift. A button that does
        // nothing on an unrestricted account teaches the operator to ignore it.
        if (restricted || value > 0)
          FilledButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: const Text('برداشتن محدودیت'),
          ),
      ],
    );
  }
}

class _RoleChange {
  const _RoleChange({required this.role, required this.grant});

  final String role;
  final bool grant;
}

/// Operator roles on one account.
///
/// The keys match `admin_roles` in the schema, which is where the permissions
/// behind each one live. Naming them here and nowhere else would be a second
/// list to keep in step; what this holds is only the label a person reads.
class _RolesDialog extends StatelessWidget {
  const _RolesDialog({required this.name, required this.held});

  final String name;
  final List<String> held;

  static const Map<String, String> _roles = <String, String>{
    'super_admin': 'مدیر ارشد — دسترسی کامل',
    'administrator': 'مدیر سامانه',
    'moderator': 'ناظر — گزارش‌ها و محرومیت‌ها',
    'news_editor': 'سردبیر اخبار',
    'news_author': 'نویسندهٔ اخبار',
    'support': 'پشتیبانی — فقط خواندن',
    'analytics': 'تحلیل — فقط سنجه‌ها',
  };

  @override
  Widget build(BuildContext context) {
    return AlertDialog(
      title: Text('نقش‌های اپراتوری — $name'),
      content: SizedBox(
        width: 460,
        child: Column(
          mainAxisSize: MainAxisSize.min,
          children: <Widget>[
            for (final MapEntry<String, String> role in _roles.entries)
              ListTile(
                leading: Icon(
                  held.contains(role.key)
                      ? Icons.check_circle
                      : Icons.circle_outlined,
                ),
                title: Text(role.value),
                subtitle: Text(
                  role.key,
                  textDirection: TextDirection.ltr,
                  style: Theme.of(context).textTheme.bodySmall,
                ),
                // One change per visit. Batching them would mean deciding what
                // to do when the third of four is refused, and each grant is
                // its own audit entry anyway.
                onTap: () => Navigator.of(context).pop(
                  _RoleChange(
                    role: role.key,
                    grant: !held.contains(role.key),
                  ),
                ),
              ),
          ],
        ),
      ),
      actions: <Widget>[
        TextButton(
          onPressed: () => Navigator.of(context).pop(),
          child: const Text('بستن'),
        ),
      ],
    );
  }
}
