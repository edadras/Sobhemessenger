import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// The moderation queue (§34).
class ReportsPage extends ConsumerStatefulWidget {
  const ReportsPage({super.key});

  @override
  ConsumerState<ReportsPage> createState() => _ReportsPageState();
}

class _ReportsPageState extends ConsumerState<ReportsPage> {
  String _status = 'open';
  int _reloadToken = 0;

  Future<void> _resolve(String reportId, String status) async {
    final AdminApi api = ref.read(adminApiProvider);
    try {
      await api.resolveReport(reportId, status, '');
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
              Text('گزارش‌ها', style: Theme.of(context).textTheme.headlineSmall),
              const Spacer(),
              SegmentedButton<String>(
                segments: const <ButtonSegment<String>>[
                  ButtonSegment<String>(value: 'open', label: Text('باز')),
                  ButtonSegment<String>(value: 'reviewing', label: Text('در بررسی')),
                  ButtonSegment<String>(value: 'actioned', label: Text('اقدام‌شده')),
                  ButtonSegment<String>(value: 'dismissed', label: Text('رد شده')),
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
              future: api.reports(status: _status),
              emptyMessage: 'گزارشی در این وضعیت وجود ندارد',
              builder: (BuildContext context, List<dynamic> reports) => ListView.separated(
                itemCount: reports.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> report = reports[index] as Map<String, dynamic>;
                  return ListTile(
                    leading: Icon(_iconFor(report['reason'] as String? ?? '')),
                    title: Text('${report['target_type']} · ${report['reason']}'),
                    subtitle: Text(
                      report['detail'] as String? ?? 'بدون توضیح',
                      maxLines: 2,
                      overflow: TextOverflow.ellipsis,
                    ),
                    trailing: _status == 'open' || _status == 'reviewing'
                        ? Row(
                            mainAxisSize: MainAxisSize.min,
                            children: <Widget>[
                              TextButton(
                                onPressed: () =>
                                    _resolve(report['id'] as String, 'dismissed'),
                                child: const Text('رد'),
                              ),
                              FilledButton(
                                onPressed: () =>
                                    _resolve(report['id'] as String, 'actioned'),
                                child: const Text('اقدام'),
                              ),
                            ],
                          )
                        : null,
                  );
                },
              ),
            ),
          ),
        ],
      ),
    );
  }

  static IconData _iconFor(String reason) => switch (reason) {
        'spam' => Icons.report_gmailerrorred,
        'violence' => Icons.warning_amber,
        'child_abuse' => Icons.gpp_bad,
        'fraud' => Icons.money_off,
        'illegal' => Icons.gavel,
        _ => Icons.flag_outlined,
      };
}
