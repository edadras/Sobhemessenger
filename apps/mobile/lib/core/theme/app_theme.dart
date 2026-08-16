import 'package:flutter/material.dart';

import 'design_tokens.dart';

/// Builds the Material themes from the design tokens (§43).
///
/// Widgets read colours through [SobhTheme.palette] rather than
/// `Theme.of(context).colorScheme`, because several chat surfaces (bubbles,
/// unread badges, presence dots) have no Material equivalent and would
/// otherwise be hard-coded.
abstract final class AppTheme {
  const AppTheme._();

  static ThemeData light() => _build(SobhColors.light, Brightness.light);

  static ThemeData dark() => _build(SobhColors.dark, Brightness.dark);

  static ThemeData _build(SobhPalette palette, Brightness brightness) {
    final ColorScheme scheme = ColorScheme(
      brightness: brightness,
      primary: palette.primary,
      onPrimary: palette.onPrimary,
      secondary: palette.secondary,
      onSecondary: palette.onPrimary,
      error: palette.error,
      onError: palette.onPrimary,
      surface: palette.surface,
      onSurface: palette.textPrimary,
    );

    final TextTheme textTheme = _textTheme(palette);

    return ThemeData(
      useMaterial3: true,
      brightness: brightness,
      colorScheme: scheme,
      scaffoldBackgroundColor: palette.background,
      textTheme: textTheme,
      // Vazirmatn renders Persian and Arabic correctly, including the
      // zero-width non-joiner that Latin fonts drop.
      fontFamily: 'Vazirmatn',
      extensions: <ThemeExtension<dynamic>>[SobhTheme(palette: palette)],
      appBarTheme: AppBarTheme(
        backgroundColor: palette.surface,
        foregroundColor: palette.textPrimary,
        elevation: 0,
        scrolledUnderElevation: 1,
        centerTitle: false,
        titleTextStyle: textTheme.titleMedium,
      ),
      cardTheme: CardTheme(
        color: palette.surface,
        elevation: 0,
        margin: EdgeInsets.zero,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          side: BorderSide(color: palette.outline),
        ),
      ),
      inputDecorationTheme: InputDecorationTheme(
        filled: true,
        fillColor: palette.surfaceVariant,
        contentPadding: const EdgeInsets.symmetric(
          horizontal: SobhSpacing.lg,
          vertical: SobhSpacing.md,
        ),
        border: OutlineInputBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          borderSide: BorderSide.none,
        ),
        enabledBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          borderSide: BorderSide(color: palette.outline),
        ),
        focusedBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          borderSide: BorderSide(color: palette.primary, width: 2),
        ),
        errorBorder: OutlineInputBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
          borderSide: BorderSide(color: palette.error),
        ),
        hintStyle: textTheme.bodyMedium?.copyWith(color: palette.textDisabled),
      ),
      filledButtonTheme: FilledButtonThemeData(
        style: FilledButton.styleFrom(
          minimumSize: const Size.fromHeight(SobhSizes.minTapTarget),
          shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(SobhRadius.md),
          ),
          textStyle: textTheme.labelLarge,
        ),
      ),
      textButtonTheme: TextButtonThemeData(
        style: TextButton.styleFrom(
          minimumSize:
              const Size(SobhSizes.minTapTarget, SobhSizes.minTapTarget),
        ),
      ),
      dividerTheme:
          DividerThemeData(color: palette.outline, space: 1, thickness: 1),
      snackBarTheme: SnackBarThemeData(
        backgroundColor: palette.textPrimary,
        contentTextStyle:
            textTheme.bodyMedium?.copyWith(color: palette.surface),
        behavior: SnackBarBehavior.floating,
        shape: RoundedRectangleBorder(
          borderRadius: BorderRadius.circular(SobhRadius.md),
        ),
      ),
      listTileTheme: ListTileThemeData(
        iconColor: palette.textSecondary,
        textColor: palette.textPrimary,
        minVerticalPadding: SobhSpacing.md,
      ),
      bottomNavigationBarTheme: BottomNavigationBarThemeData(
        backgroundColor: palette.surface,
        selectedItemColor: palette.primary,
        unselectedItemColor: palette.textSecondary,
        type: BottomNavigationBarType.fixed,
      ),
    );
  }

  static TextTheme _textTheme(SobhPalette palette) {
    final TextStyle primary = TextStyle(color: palette.textPrimary);
    final TextStyle secondary = TextStyle(color: palette.textSecondary);

    return TextTheme(
      headlineLarge: primary.copyWith(
        fontSize: 28,
        fontWeight: FontWeight.w700,
        height: 1.3,
      ),
      headlineMedium: primary.copyWith(
        fontSize: 22,
        fontWeight: FontWeight.w700,
        height: 1.3,
      ),
      titleLarge: primary.copyWith(
        fontSize: 19,
        fontWeight: FontWeight.w600,
        height: 1.35,
      ),
      titleMedium: primary.copyWith(
        fontSize: 16,
        fontWeight: FontWeight.w600,
        height: 1.4,
      ),
      titleSmall: primary.copyWith(
        fontSize: 14,
        fontWeight: FontWeight.w600,
        height: 1.4,
      ),
      bodyLarge: primary.copyWith(fontSize: 16, height: 1.5),
      bodyMedium: primary.copyWith(fontSize: 14, height: 1.5),
      bodySmall: secondary.copyWith(fontSize: 12, height: 1.45),
      labelLarge: primary.copyWith(fontSize: 15, fontWeight: FontWeight.w600),
      labelMedium:
          secondary.copyWith(fontSize: 13, fontWeight: FontWeight.w500),
      labelSmall: secondary.copyWith(fontSize: 11, fontWeight: FontWeight.w500),
    );
  }
}

/// Carries the SOBH palette through the widget tree.
@immutable
class SobhTheme extends ThemeExtension<SobhTheme> {
  const SobhTheme({required this.palette});

  final SobhPalette palette;

  /// The palette for the current theme. Widgets call this instead of reaching
  /// for a raw colour.
  static SobhPalette of(BuildContext context) =>
      Theme.of(context).extension<SobhTheme>()?.palette ?? SobhColors.light;

  @override
  SobhTheme copyWith({SobhPalette? palette}) =>
      SobhTheme(palette: palette ?? this.palette);

  // The palette is a pair of discrete constants rather than a continuous
  // space, so crossfading between themes snaps at the midpoint.
  @override
  SobhTheme lerp(ThemeExtension<SobhTheme>? other, double t) {
    if (other is! SobhTheme) {
      return this;
    }
    return t < 0.5 ? this : other;
  }
}
