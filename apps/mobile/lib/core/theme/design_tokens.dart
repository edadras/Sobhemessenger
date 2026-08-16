import 'package:flutter/widgets.dart';

/// Design tokens for SOBH (§43).
///
/// Every colour, spacing step, radius and duration in the app comes from here.
/// Nothing is hard-coded in a widget, so the whole product can be re-themed
/// from one file and a dark variant cannot drift out of sync with the light one.
abstract final class SobhColors {
  const SobhColors._();

  // Brand. "SOBH" means morning: the palette is built around dawn light.
  static const Color brandPrimary = Color(0xFFE8813A);
  static const Color brandPrimaryDark = Color(0xFFC96721);
  static const Color brandSecondary = Color(0xFF1F6F8B);
  static const Color brandAccent = Color(0xFFF2B544);

  // Typed as the interface, not the private implementation: callers only ever
  // need the palette contract, and exposing the private class would leak it
  // into the public API.
  static const SobhPalette light = _Light();
  static const SobhPalette dark = _Dark();
}

/// Semantic slots. A widget asks for `surface` or `textSecondary`; it never
/// asks for a hex value.
abstract interface class SobhPalette {
  Color get primary;
  Color get onPrimary;
  Color get secondary;
  Color get background;
  Color get surface;
  Color get surfaceVariant;
  Color get outline;
  Color get textPrimary;
  Color get textSecondary;
  Color get textDisabled;
  Color get error;
  Color get success;
  Color get warning;
  Color get info;

  /// Chat-specific slots, which do not map onto Material's scheme.
  Color get bubbleOutgoing;
  Color get bubbleIncoming;
  Color get bubbleOutgoingText;
  Color get bubbleIncomingText;
  Color get chatBackground;
  Color get unreadBadge;
  Color get onlineIndicator;
}

final class _Light implements SobhPalette {
  const _Light();

  @override
  Color get primary => SobhColors.brandPrimary;
  @override
  Color get onPrimary => const Color(0xFFFFFFFF);
  @override
  Color get secondary => SobhColors.brandSecondary;
  @override
  Color get background => const Color(0xFFFAFAFA);
  @override
  Color get surface => const Color(0xFFFFFFFF);
  @override
  Color get surfaceVariant => const Color(0xFFF1F3F5);
  @override
  Color get outline => const Color(0xFFDDE1E5);
  @override
  Color get textPrimary => const Color(0xFF16191C);
  @override
  Color get textSecondary => const Color(0xFF5C6670);
  @override
  Color get textDisabled => const Color(0xFF9BA4AD);
  @override
  Color get error => const Color(0xFFD03434);
  @override
  Color get success => const Color(0xFF2E9E5B);
  @override
  Color get warning => const Color(0xFFE0A100);
  @override
  Color get info => const Color(0xFF2A7FB8);
  @override
  Color get bubbleOutgoing => const Color(0xFFFFE8D6);
  @override
  Color get bubbleIncoming => const Color(0xFFFFFFFF);
  @override
  Color get bubbleOutgoingText => const Color(0xFF16191C);
  @override
  Color get bubbleIncomingText => const Color(0xFF16191C);
  @override
  Color get chatBackground => const Color(0xFFF6F4F1);
  @override
  Color get unreadBadge => SobhColors.brandPrimary;
  @override
  Color get onlineIndicator => const Color(0xFF2E9E5B);
}

final class _Dark implements SobhPalette {
  const _Dark();

  @override
  Color get primary => const Color(0xFFF29B5C);
  @override
  Color get onPrimary => const Color(0xFF201005);
  @override
  Color get secondary => const Color(0xFF56A5BE);
  @override
  Color get background => const Color(0xFF0F1114);
  @override
  Color get surface => const Color(0xFF181B1F);
  @override
  Color get surfaceVariant => const Color(0xFF22262B);
  @override
  Color get outline => const Color(0xFF333A41);
  @override
  Color get textPrimary => const Color(0xFFF2F4F6);
  @override
  Color get textSecondary => const Color(0xFFA7B0B9);
  @override
  Color get textDisabled => const Color(0xFF6B747D);
  @override
  Color get error => const Color(0xFFF06A6A);
  @override
  Color get success => const Color(0xFF5BC286);
  @override
  Color get warning => const Color(0xFFF0C04A);
  @override
  Color get info => const Color(0xFF5EA8D8);
  @override
  Color get bubbleOutgoing => const Color(0xFF3A2A1C);
  @override
  Color get bubbleIncoming => const Color(0xFF22262B);
  @override
  Color get bubbleOutgoingText => const Color(0xFFF2F4F6);
  @override
  Color get bubbleIncomingText => const Color(0xFFF2F4F6);
  @override
  Color get chatBackground => const Color(0xFF121417);
  @override
  Color get unreadBadge => const Color(0xFFF29B5C);
  @override
  Color get onlineIndicator => const Color(0xFF5BC286);
}

/// A 4pt spacing scale. Layout code uses these names, never raw numbers.
abstract final class SobhSpacing {
  const SobhSpacing._();

  static const double xxs = 2;
  static const double xs = 4;
  static const double sm = 8;
  static const double md = 12;
  static const double lg = 16;
  static const double xl = 24;
  static const double xxl = 32;
  static const double xxxl = 48;
}

abstract final class SobhRadius {
  const SobhRadius._();

  static const double sm = 6;
  static const double md = 10;
  static const double lg = 16;
  static const double xl = 24;
  static const double round = 999;
}

abstract final class SobhDuration {
  const SobhDuration._();

  static const Duration instant = Duration(milliseconds: 100);
  static const Duration fast = Duration(milliseconds: 180);
  static const Duration normal = Duration(milliseconds: 260);
  static const Duration slow = Duration(milliseconds: 400);
}

abstract final class SobhSizes {
  const SobhSizes._();

  /// Minimum tap target (§45). Nothing interactive may be smaller.
  static const double minTapTarget = 48;
  static const double avatarSmall = 32;
  static const double avatarMedium = 44;
  static const double avatarLarge = 96;
  static const double iconSmall = 16;
  static const double iconMedium = 24;
  static const double iconLarge = 32;
  static const double maxBubbleWidthFraction = 0.78;
}
