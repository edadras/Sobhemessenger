/// Runtime configuration, supplied at build time with `--dart-define` so no
/// endpoint or key is baked into the source (§84.11).
class AppConfig {
  const AppConfig({
    required this.apiBaseUrl,
    required this.wsUrl,
    required this.appVersion,
    required this.environment,
  });

  /// Reads the configuration the app was compiled with.
  ///
  ///     flutter run --dart-define=SOBH_API_BASE_URL=https://api.sobh.app \
  ///                 --dart-define=SOBH_WS_URL=wss://api.sobh.app/ws
  factory AppConfig.fromEnvironment() {
    const String api = String.fromEnvironment(
      'SOBH_API_BASE_URL',
      defaultValue: 'http://localhost:8080',
    );
    const String ws = String.fromEnvironment(
      'SOBH_WS_URL',
      defaultValue: 'ws://localhost:8080/ws',
    );
    return const AppConfig(
      apiBaseUrl: api,
      wsUrl: ws,
      appVersion: String.fromEnvironment('SOBH_APP_VERSION', defaultValue: '1.0.0'),
      environment: String.fromEnvironment('SOBH_ENV', defaultValue: 'development'),
    );
  }

  final String apiBaseUrl;
  final String wsUrl;
  final String appVersion;
  final String environment;

  /// WebSocket contract version this build speaks (§67).
  static const int protocolVersion = 1;

  bool get isProduction => environment == 'production';
}
