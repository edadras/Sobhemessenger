import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import 'admin_shell.dart';

/// The operator overview (§62).
class DashboardPage extends ConsumerWidget {
  const DashboardPage({super.key});

  @override
  Widget build(BuildContext context, WidgetRef ref) {
    final AdminApi api = ref.watch(adminApiProvider);

    return Padding(
      padding: const EdgeInsets.all(24),
      child: AsyncSection<Map<String, dynamic>>(
        future: api.dashboard(),
        builder: (BuildContext context, Map<String, dynamic> data) {
          final Map<String, dynamic> users = _section(data, 'users');
          final Map<String, dynamic> messages = _section(data, 'messages');
          final Map<String, dynamic> chats = _section(data, 'chats');
          final Map<String, dynamic> media = _section(data, 'media');
          final Map<String, dynamic> news = _section(data, 'news');
          final Map<String, dynamic> moderation = _section(data, 'moderation');

          return ListView(
            children: <Widget>[
              Text('داشبورد', style: Theme.of(context).textTheme.headlineSmall),
              const SizedBox(height: 24),
              Wrap(
                spacing: 16,
                runSpacing: 16,
                children: <Widget>[
                  _StatCard('کاربران', _number(users['total']), Icons.people),
                  _StatCard('کاربران فعال امروز', _number(users['daily_active']), Icons.bolt),
                  _StatCard('کاربران جدید امروز', _number(users['new_today']), Icons.person_add),
                  _StatCard('پیام‌های امروز', _number(messages['today']), Icons.forum),
                  _StatCard('کل پیام‌ها', _number(messages['total']), Icons.chat),
                  _StatCard('گروه‌ها', _number(chats['groups']), Icons.groups),
                  _StatCard('کانال‌ها', _number(chats['channels']), Icons.campaign),
                  _StatCard('حجم رسانه', _bytes(media['total_bytes']), Icons.sd_storage),
                  _StatCard('اخبار منتشرشده', _number(news['published']), Icons.article),
                  _StatCard('پیش‌نویس اخبار', _number(news['drafts']), Icons.edit_note),
                  _StatCard('گزارش‌های باز', _number(moderation['open_reports']), Icons.flag,
                      highlight: (moderation['open_reports'] as num? ?? 0) > 0),
                  _StatCard('مسدودهای فعال', _number(moderation['active_bans']), Icons.block),
                ],
              ),
              const SizedBox(height: 32),
              Text('روند ۳۰ روز گذشته', style: Theme.of(context).textTheme.titleMedium),
              const SizedBox(height: 12),
              SizedBox(
                height: 240,
                child: AsyncSection<List<dynamic>>(
                  future: api.timeSeries('new_users'),
                  emptyMessage: 'داده‌ای برای نمایش وجود ندارد',
                  builder: (BuildContext context, List<dynamic> points) =>
                      _SeriesTable(points: points, label: 'کاربران جدید'),
                ),
              ),
            ],
          );
        },
      ),
    );
  }

  static Map<String, dynamic> _section(Map<String, dynamic> data, String key) =>
      data[key] as Map<String, dynamic>? ?? const <String, dynamic>{};

  static String _number(Object? value) {
    final int number = (value as num?)?.toInt() ?? 0;
    // Group digits so a seven-figure count stays readable at a glance.
    final String digits = number.toString();
    final StringBuffer buffer = StringBuffer();
    for (int i = 0; i < digits.length; i++) {
      if (i > 0 && (digits.length - i) % 3 == 0) {
        buffer.write(',');
      }
      buffer.write(digits[i]);
    }
    return buffer.toString();
  }

  static String _bytes(Object? value) {
    double size = ((value as num?) ?? 0).toDouble();
    const List<String> units = <String>['B', 'KB', 'MB', 'GB', 'TB'];
    int unit = 0;
    while (size >= 1024 && unit < units.length - 1) {
      size /= 1024;
      unit++;
    }
    return '${size.toStringAsFixed(1)} ${units[unit]}';
  }
}

class _StatCard extends StatelessWidget {
  const _StatCard(this.label, this.value, this.icon, {this.highlight = false});

  final String label;
  final String value;
  final IconData icon;
  final bool highlight;

  @override
  Widget build(BuildContext context) {
    final ColorScheme colors = Theme.of(context).colorScheme;

    return SizedBox(
      width: 220,
      child: Card(
        color: highlight ? colors.errorContainer : null,
        child: Padding(
          padding: const EdgeInsets.all(16),
          child: Column(
            crossAxisAlignment: CrossAxisAlignment.start,
            children: <Widget>[
              Row(
                children: <Widget>[
                  Icon(icon, size: 20, color: colors.primary),
                  const SizedBox(width: 8),
                  Expanded(
                    child: Text(label, style: Theme.of(context).textTheme.bodySmall),
                  ),
                ],
              ),
              const SizedBox(height: 12),
              Text(value, style: Theme.of(context).textTheme.headlineSmall),
            ],
          ),
        ),
      ),
    );
  }
}

/// A compact table rather than a chart: the series is short, and a table is
/// readable without a plotting dependency in the critical path.
class _SeriesTable extends StatelessWidget {
  const _SeriesTable({required this.points, required this.label});

  final List<dynamic> points;
  final String label;

  @override
  Widget build(BuildContext context) {
    return ListView.builder(
      scrollDirection: Axis.horizontal,
      itemCount: points.length,
      itemBuilder: (BuildContext context, int index) {
        final Map<String, dynamic> point = points[index] as Map<String, dynamic>;
        final int value = (point['value'] as num?)?.toInt() ?? 0;
        final int maxValue = points.fold<int>(1, (int previous, dynamic item) {
          final int candidate =
              ((item as Map<String, dynamic>)['value'] as num?)?.toInt() ?? 0;
          return candidate > previous ? candidate : previous;
        });

        return Padding(
          padding: const EdgeInsets.symmetric(horizontal: 4),
          child: Column(
            mainAxisAlignment: MainAxisAlignment.end,
            children: <Widget>[
              Text('$value', style: Theme.of(context).textTheme.labelSmall),
              const SizedBox(height: 4),
              Container(
                width: 24,
                height: 160 * (value / maxValue),
                decoration: BoxDecoration(
                  color: Theme.of(context).colorScheme.primary,
                  borderRadius: const BorderRadius.vertical(top: Radius.circular(4)),
                ),
              ),
              const SizedBox(height: 4),
              SizedBox(
                width: 40,
                child: Text(
                  '${point['day']}'.split('T').first.substring(5),
                  style: Theme.of(context).textTheme.labelSmall,
                  textAlign: TextAlign.center,
                ),
              ),
            ],
          ),
        );
      },
    );
  }
}
