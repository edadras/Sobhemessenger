import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';

import '../../../core/admin_api.dart';
import '../../../main.dart';
import '../../dashboard/presentation/admin_shell.dart';

/// Runtime feature switches (§68).
class FlagsPage extends ConsumerStatefulWidget {
  const FlagsPage({super.key});

  @override
  ConsumerState<FlagsPage> createState() => _FlagsPageState();
}

class _FlagsPageState extends ConsumerState<FlagsPage> {
  int _reloadToken = 0;

  Future<void> _toggle(String key, bool enabled, int rollout) async {
    final AdminApi api = ref.read(adminApiProvider);
    try {
      await api.setFeatureFlag(key, enabled, rollout);
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
          Text('قابلیت‌ها', style: Theme.of(context).textTheme.headlineSmall),
          const SizedBox(height: 8),
          Text(
            'تغییرات بلافاصله روی همه سرورها اعمال می‌شود',
            style: Theme.of(context).textTheme.bodySmall,
          ),
          const SizedBox(height: 16),
          Expanded(
            child: AsyncSection<List<dynamic>>(
              key: ValueKey<int>(_reloadToken),
              future: api.featureFlags(),
              builder: (BuildContext context, List<dynamic> flags) => ListView.separated(
                itemCount: flags.length,
                separatorBuilder: (_, __) => const Divider(height: 1),
                itemBuilder: (BuildContext context, int index) {
                  final Map<String, dynamic> flag = flags[index] as Map<String, dynamic>;
                  final bool enabled = flag['enabled'] as bool? ?? false;
                  final int rollout = (flag['rollout_percent'] as num?)?.toInt() ?? 100;

                  return SwitchListTile(
                    title: Text(flag['key'] as String),
                    subtitle: Text(
                      flag['description'] as String? ?? '',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                    ),
                    secondary: rollout < 100
                        ? Chip(label: Text('$rollout٪'))
                        : null,
                    value: enabled,
                    onChanged: (bool value) =>
                        _toggle(flag['key'] as String, value, rollout),
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
