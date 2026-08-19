import 'package:flutter/material.dart';
import 'package:flutter_riverpod/flutter_riverpod.dart';
import 'package:go_router/go_router.dart';

import '../../../core/localization/generated/app_localizations.dart';
import '../../../core/network/api_exception.dart';
import '../../../core/theme/design_tokens.dart';
import '../../../core/widgets/async_states.dart';
import '../data/groups_repository.dart';

/// Opening an invite link (§16).
///
/// The app could create these links and not redeem one, so an invite was a
/// string somebody could send and nobody could use. This is the receiving end.
///
/// It accepts a whole URL or a bare slug, because what arrives in a message is
/// a link and what someone reads aloud is the last part of it. Refusing the
/// form the person happens to have would be a rule serving nobody.
class JoinByLinkScreen extends ConsumerStatefulWidget {
  const JoinByLinkScreen({super.key, this.slug});

  /// Supplied when the app was opened on a link rather than by someone
  /// pasting one, in which case the join runs immediately.
  final String? slug;

  @override
  ConsumerState<JoinByLinkScreen> createState() => _JoinByLinkScreenState();
}

class _JoinByLinkScreenState extends ConsumerState<JoinByLinkScreen> {
  late final TextEditingController _link =
      TextEditingController(text: widget.slug ?? '');
  bool _working = false;
  String? _problem;

  /// True once a join has been filed for approval, so the screen can say so
  /// instead of navigating to a room the person cannot post in yet.
  bool _pending = false;

  @override
  void initState() {
    super.initState();
    if (widget.slug != null && widget.slug!.isNotEmpty) {
      WidgetsBinding.instance.addPostFrameCallback((_) => _join());
    }
  }

  @override
  void dispose() {
    _link.dispose();
    super.dispose();
  }

  /// The slug is the last non-empty path segment: `sobh.app/join/AbC123` and
  /// `AbC123` are the same invitation.
  static String slugFrom(String raw) {
    final String trimmed = raw.trim();
    if (trimmed.isEmpty) {
      return '';
    }
    final Uri? parsed = Uri.tryParse(trimmed);
    final List<String> segments = parsed?.pathSegments
            .where((String segment) => segment.isNotEmpty)
            .toList() ??
        const <String>[];
    if (segments.isNotEmpty) {
      return segments.last;
    }
    return trimmed;
  }

  Future<void> _join() async {
    final String slug = slugFrom(_link.text);
    if (slug.isEmpty || _working) {
      return;
    }

    final AppLocalizations l10n = AppLocalizations.of(context);
    setState(() {
      _working = true;
      _problem = null;
    });

    try {
      final JoinOutcome outcome =
          await ref.read(groupsRepositoryProvider).joinByInvite(slug);
      if (!mounted) {
        return;
      }
      if (outcome.pending) {
        setState(() => _pending = true);
        return;
      }
      // Straight into the conversation. Landing back on this screen after
      // joining would leave the person to find the chat themselves.
      context.go('/chats/${outcome.chatId}');
    } on ApiException catch (error) {
      if (mounted) {
        setState(() {
          _problem = error.isOffline ? l10n.errorNetwork : error.message;
        });
      }
    } finally {
      if (mounted) {
        setState(() => _working = false);
      }
    }
  }

  @override
  Widget build(BuildContext context) {
    final AppLocalizations l10n = AppLocalizations.of(context);

    return Scaffold(
      appBar: AppBar(title: Text(l10n.joinByLinkTitle)),
      body: _pending
          ? SobhEmptyState(
              icon: Icons.hourglass_top_outlined,
              title: l10n.joinByLinkPendingTitle,
              body: l10n.joinByLinkPendingBody,
            )
          : Padding(
              padding: const EdgeInsets.all(SobhSpacing.lg),
              child: Column(
                crossAxisAlignment: CrossAxisAlignment.stretch,
                children: <Widget>[
                  Text(l10n.joinByLinkBody),
                  const SizedBox(height: SobhSpacing.lg),
                  TextField(
                    controller: _link,
                    autofocus: widget.slug == null,
                    textDirection: TextDirection.ltr,
                    decoration: InputDecoration(
                      labelText: l10n.joinByLinkField,
                      prefixIcon: const Icon(Icons.link),
                      errorText: _problem,
                    ),
                    onChanged: (_) {
                      if (_problem != null) {
                        setState(() => _problem = null);
                      }
                    },
                    onSubmitted: (_) => _join(),
                  ),
                  const SizedBox(height: SobhSpacing.lg),
                  FilledButton(
                    onPressed: _working ? null : _join,
                    child: Text(l10n.joinByLinkAction),
                  ),
                  if (_working)
                    const Padding(
                      padding: EdgeInsets.only(top: SobhSpacing.lg),
                      child: LinearProgressIndicator(),
                    ),
                ],
              ),
            ),
    );
  }
}
