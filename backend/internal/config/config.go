// Package config loads every runtime setting from the environment (§84 rule 11).
// Nothing here reads a file that could carry a committed secret, and the
// production profile refuses to start on a default credential.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Environment string

const (
	EnvDevelopment Environment = "development"
	EnvStaging     Environment = "staging"
	EnvProduction  Environment = "production"
)

type Config struct {
	Env             Environment
	ServiceName     string
	NodeID          string
	HTTPAddr        string
	MetricsAddr     string
	PublicBaseURL   string
	ShutdownTimeout time.Duration
	TrustedProxies  []string

	Postgres   Postgres
	Redis      Redis
	NATS       NATS
	Storage    Storage
	Search     Search
	Auth       Auth
	SMS        SMS
	Push       Push
	Media      Media
	Calls      Calls
	RateLimits RateLimits
	Log        Log
}

type Postgres struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	StatementCache  bool
}

type Redis struct {
	Addr     string
	Password string
	DB       int
	PoolSize int
}

type NATS struct {
	URL           string
	StreamName    string
	MaxReconnects int
	ReconnectWait time.Duration
}

type Storage struct {
	Endpoint     string
	AccessKey    string
	SecretKey    string
	UseSSL       bool
	Region       string
	MediaBucket  string
	PublicBucket string
	ExportBucket string
	// CDN in front of the media gateway (§73); empty falls back to presigned URLs.
	CDNBaseURL string
}

type Search struct {
	Addresses []string
	Username  string
	Password  string
	IndexPfx  string
	Enabled   bool
}

type Auth struct {
	// HS256 secret for access tokens. Rotated via JWT_SIGNING_KEYS (kid:secret).
	SigningKeys       map[string]string
	ActiveKeyID       string
	AccessTokenTTL    time.Duration
	RefreshTokenTTL   time.Duration
	Issuer            string
	Audience          string
	OTPLength         int
	OTPTTL            time.Duration
	OTPMaxAttempts    int
	PhoneHashPepper   []byte
	MaxDevicesPerUser int
}

type SMS struct {
	Provider string
	BaseURL  string
	APIKey   string
	Sender   string
	Timeout  time.Duration
	// Development only: echo the OTP in the API response instead of sending it.
	EchoCodes bool
}

type Push struct {
	FCMEndpoint     string
	FCMCredentials  string
	APNsEndpoint    string
	APNsKeyID       string
	APNsTeamID      string
	APNsBundleID    string
	APNsPrivateKey  string
	WorkerBatchSize int
}

type Media struct {
	MaxUploadBytes    int64
	MaxImageBytes     int64
	MaxAvatarBytes    int64
	PartSize          int64
	UploadSessionTTL  time.Duration
	PresignTTL        time.Duration
	AllowedImageMIME  []string
	AllowedVideoMIME  []string
	AllowedAudioMIME  []string
	AllowedFileMIME   []string
	ClamAVAddr        string
	VirusScanRequired bool
}

type Calls struct {
	STUNServers          []string
	TURNServers          []string
	TURNSecret           string
	TURNCredentialTTL    time.Duration
	MaxGroupParticipants int
}

type RateLimits struct {
	OTPPerPhonePerHour int
	OTPPerIPPerHour    int
	LoginPerIPPerHour  int
	APIPerUserPerMin   int
	APIPerIPPerMin     int
	MessagesPerMin     int
	UploadsPerHour     int
}

type Log struct {
	Level  string
	Format string
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	env := Environment(getString("SOBH_ENV", string(EnvDevelopment)))
	switch env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		return nil, fmt.Errorf("config: SOBH_ENV must be development, staging or production (got %q)", env)
	}

	signingKeys, activeKID, err := parseSigningKeys(getString("JWT_SIGNING_KEYS", ""), getString("JWT_ACTIVE_KEY_ID", ""))
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Env:             env,
		ServiceName:     getString("SERVICE_NAME", "sobh-api"),
		NodeID:          getString("NODE_ID", defaultNodeID()),
		HTTPAddr:        getString("HTTP_ADDR", ":8080"),
		MetricsAddr:     getString("METRICS_ADDR", ":9090"),
		PublicBaseURL:   getString("PUBLIC_BASE_URL", "http://localhost:8080"),
		ShutdownTimeout: getDuration("SHUTDOWN_TIMEOUT", 25*time.Second),
		TrustedProxies:  getStringSlice("TRUSTED_PROXIES", nil),

		Postgres: Postgres{
			DSN:             getString("POSTGRES_DSN", ""),
			MaxConns:        int32(getInt("POSTGRES_MAX_CONNS", 32)),
			MinConns:        int32(getInt("POSTGRES_MIN_CONNS", 4)),
			MaxConnLifetime: getDuration("POSTGRES_CONN_LIFETIME", time.Hour),
			MaxConnIdleTime: getDuration("POSTGRES_CONN_IDLE", 30*time.Minute),
			StatementCache:  getBool("POSTGRES_STATEMENT_CACHE", true),
		},
		Redis: Redis{
			Addr:     getString("REDIS_ADDR", "localhost:6379"),
			Password: getString("REDIS_PASSWORD", ""),
			DB:       getInt("REDIS_DB", 0),
			PoolSize: getInt("REDIS_POOL_SIZE", 32),
		},
		NATS: NATS{
			URL:           getString("NATS_URL", "nats://localhost:4222"),
			StreamName:    getString("NATS_STREAM", "SOBH"),
			MaxReconnects: getInt("NATS_MAX_RECONNECTS", -1),
			ReconnectWait: getDuration("NATS_RECONNECT_WAIT", 2*time.Second),
		},
		Storage: Storage{
			Endpoint:     getString("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey:    getString("MINIO_ACCESS_KEY", ""),
			SecretKey:    getString("MINIO_SECRET_KEY", ""),
			UseSSL:       getBool("MINIO_USE_SSL", false),
			Region:       getString("MINIO_REGION", "us-east-1"),
			MediaBucket:  getString("MINIO_MEDIA_BUCKET", "sobh-media"),
			PublicBucket: getString("MINIO_PUBLIC_BUCKET", "sobh-public"),
			ExportBucket: getString("MINIO_EXPORT_BUCKET", "sobh-exports"),
			CDNBaseURL:   getString("CDN_BASE_URL", ""),
		},
		Search: Search{
			Addresses: getStringSlice("OPENSEARCH_ADDRESSES", []string{"http://localhost:9200"}),
			Username:  getString("OPENSEARCH_USERNAME", ""),
			Password:  getString("OPENSEARCH_PASSWORD", ""),
			IndexPfx:  getString("OPENSEARCH_INDEX_PREFIX", "sobh"),
			Enabled:   getBool("OPENSEARCH_ENABLED", true),
		},
		Auth: Auth{
			SigningKeys:       signingKeys,
			ActiveKeyID:       activeKID,
			AccessTokenTTL:    getDuration("ACCESS_TOKEN_TTL", 15*time.Minute),
			RefreshTokenTTL:   getDuration("REFRESH_TOKEN_TTL", 60*24*time.Hour),
			Issuer:            getString("JWT_ISSUER", "sobh"),
			Audience:          getString("JWT_AUDIENCE", "sobh-app"),
			OTPLength:         getInt("OTP_LENGTH", 6),
			OTPTTL:            getDuration("OTP_TTL", 5*time.Minute),
			OTPMaxAttempts:    getInt("OTP_MAX_ATTEMPTS", 5),
			PhoneHashPepper:   []byte(getString("PHONE_HASH_PEPPER", "")),
			MaxDevicesPerUser: getInt("MAX_DEVICES_PER_USER", 20),
		},
		SMS: SMS{
			Provider:  getString("SMS_PROVIDER", "log"),
			BaseURL:   getString("SMS_BASE_URL", ""),
			APIKey:    getString("SMS_API_KEY", ""),
			Sender:    getString("SMS_SENDER", "SOBH"),
			Timeout:   getDuration("SMS_TIMEOUT", 10*time.Second),
			EchoCodes: getBool("SMS_ECHO_CODES", false),
		},
		Push: Push{
			FCMEndpoint:     getString("FCM_ENDPOINT", "https://fcm.googleapis.com/v1"),
			FCMCredentials:  getString("FCM_CREDENTIALS_JSON", ""),
			APNsEndpoint:    getString("APNS_ENDPOINT", "https://api.push.apple.com"),
			APNsKeyID:       getString("APNS_KEY_ID", ""),
			APNsTeamID:      getString("APNS_TEAM_ID", ""),
			APNsBundleID:    getString("APNS_BUNDLE_ID", "app.sobh.messenger"),
			APNsPrivateKey:  getString("APNS_PRIVATE_KEY", ""),
			WorkerBatchSize: getInt("PUSH_WORKER_BATCH", 200),
		},
		Media: Media{
			MaxUploadBytes:   getInt64("MEDIA_MAX_UPLOAD_BYTES", 2<<30), // 2 GiB
			MaxImageBytes:    getInt64("MEDIA_MAX_IMAGE_BYTES", 32<<20),
			MaxAvatarBytes:   getInt64("MEDIA_MAX_AVATAR_BYTES", 8<<20),
			PartSize:         getInt64("MEDIA_PART_SIZE", 8<<20),
			UploadSessionTTL: getDuration("MEDIA_UPLOAD_SESSION_TTL", 24*time.Hour),
			PresignTTL:       getDuration("MEDIA_PRESIGN_TTL", 15*time.Minute),
			AllowedImageMIME: getStringSlice("MEDIA_ALLOWED_IMAGE_MIME",
				[]string{"image/jpeg", "image/png", "image/webp", "image/gif", "image/heic"}),
			AllowedVideoMIME: getStringSlice("MEDIA_ALLOWED_VIDEO_MIME",
				[]string{"video/mp4", "video/quicktime", "video/webm"}),
			AllowedAudioMIME: getStringSlice("MEDIA_ALLOWED_AUDIO_MIME",
				[]string{"audio/ogg", "audio/opus", "audio/mpeg", "audio/mp4", "audio/aac", "audio/wav"}),
			AllowedFileMIME: getStringSlice("MEDIA_ALLOWED_FILE_MIME",
				[]string{"application/pdf", "application/zip", "text/plain",
					"application/msword",
					"application/vnd.openxmlformats-officedocument.wordprocessingml.document",
					"application/vnd.ms-excel",
					"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"}),
			ClamAVAddr:        getString("CLAMAV_ADDR", ""),
			VirusScanRequired: getBool("VIRUS_SCAN_REQUIRED", false),
		},
		Calls: Calls{
			STUNServers:          getStringSlice("STUN_SERVERS", []string{"stun:stun.l.google.com:19302"}),
			TURNServers:          getStringSlice("TURN_SERVERS", nil),
			TURNSecret:           getString("TURN_SECRET", ""),
			TURNCredentialTTL:    getDuration("TURN_CREDENTIAL_TTL", time.Hour),
			MaxGroupParticipants: getInt("MAX_GROUP_CALL_PARTICIPANTS", 25),
		},
		RateLimits: RateLimits{
			OTPPerPhonePerHour: getInt("RL_OTP_PER_PHONE_HOUR", 5),
			OTPPerIPPerHour:    getInt("RL_OTP_PER_IP_HOUR", 20),
			LoginPerIPPerHour:  getInt("RL_LOGIN_PER_IP_HOUR", 60),
			APIPerUserPerMin:   getInt("RL_API_PER_USER_MIN", 600),
			APIPerIPPerMin:     getInt("RL_API_PER_IP_MIN", 300),
			MessagesPerMin:     getInt("RL_MESSAGES_PER_MIN", 100),
			UploadsPerHour:     getInt("RL_UPLOADS_PER_HOUR", 500),
		},
		Log: Log{
			Level:  getString("LOG_LEVEL", "info"),
			Format: getString("LOG_FORMAT", "json"),
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) IsProduction() bool { return c.Env == EnvProduction }

func (c *Config) validate() error {
	var problems []string

	if c.Postgres.DSN == "" {
		problems = append(problems, "POSTGRES_DSN is required")
	}
	if len(c.Auth.SigningKeys) == 0 {
		problems = append(problems, "JWT_SIGNING_KEYS is required (format: kid:secret[,kid:secret])")
	}
	if c.Auth.OTPLength < 4 || c.Auth.OTPLength > 10 {
		problems = append(problems, "OTP_LENGTH must be between 4 and 10")
	}
	if c.Media.PartSize < 5<<20 {
		// S3-compatible multipart uploads reject parts below 5 MiB.
		problems = append(problems, "MEDIA_PART_SIZE must be at least 5 MiB")
	}

	if c.IsProduction() {
		if len(c.Auth.PhoneHashPepper) < 32 {
			problems = append(problems, "PHONE_HASH_PEPPER must be at least 32 bytes in production")
		}
		if c.SMS.EchoCodes {
			problems = append(problems, "SMS_ECHO_CODES must be false in production")
		}
		if c.SMS.Provider == "log" {
			problems = append(problems, "SMS_PROVIDER must be a real provider in production")
		}
		if c.Storage.AccessKey == "" || c.Storage.SecretKey == "" {
			problems = append(problems, "MINIO_ACCESS_KEY and MINIO_SECRET_KEY are required in production")
		}
		if !strings.HasPrefix(c.PublicBaseURL, "https://") {
			problems = append(problems, "PUBLIC_BASE_URL must use https in production")
		}
		for kid, secret := range c.Auth.SigningKeys {
			if len(secret) < 32 {
				problems = append(problems, fmt.Sprintf("signing key %q must be at least 32 bytes", kid))
			}
		}
		if len(c.Calls.TURNServers) > 0 && c.Calls.TURNSecret == "" {
			problems = append(problems, "TURN_SECRET is required when TURN_SERVERS is set")
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return nil
}

// parseSigningKeys reads "kid:secret,kid:secret" and picks the active key so
// tokens can be rotated without invalidating those already in flight (§33).
func parseSigningKeys(raw, activeKID string) (map[string]string, string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, "", nil
	}
	keys := make(map[string]string)
	var first string
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		kid, secret, ok := strings.Cut(pair, ":")
		if !ok || kid == "" || secret == "" {
			return nil, "", errors.New("config: JWT_SIGNING_KEYS entries must look like kid:secret")
		}
		if first == "" {
			first = kid
		}
		keys[kid] = secret
	}
	if len(keys) == 0 {
		return nil, "", errors.New("config: JWT_SIGNING_KEYS contained no usable entries")
	}
	if activeKID == "" {
		activeKID = first
	}
	if _, ok := keys[activeKID]; !ok {
		return nil, "", fmt.Errorf("config: JWT_ACTIVE_KEY_ID %q is not present in JWT_SIGNING_KEYS", activeKID)
	}
	return keys, activeKID, nil
}

func defaultNodeID() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "sobh-node"
}

func getString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func getStringSlice(key string, def []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(raw) == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func getInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func getInt64(key string, def int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			return n
		}
	}
	return def
}

func getBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok {
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			return b
		}
	}
	return def
}

func getDuration(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			return d
		}
	}
	return def
}
