package auth

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/security"
)

// Service implements the login flow of §11:
//
//	request OTP -> rate limit -> SMS -> verify -> (optional 2FA) -> tokens
type Service struct {
	repo    *Repository
	tokens  *TokenService
	limiter *ratelimit.Limiter
	rules   ratelimit.Rules
	sms     SMSSender
	cache   *cache.Client
	cfg     config.Auth
	smsCfg  config.SMS
	metrics *observability.Metrics
	logger  *slog.Logger

	// email is optional; see SetEmailSender. Account recovery by email is a
	// feature a deployment may not have a transport for, and the service
	// refuses to enrol an address rather than accepting one it could never
	// send to.
	email     EmailSender
	emailEcho bool
}

func NewService(
	repo *Repository,
	tokens *TokenService,
	limiter *ratelimit.Limiter,
	rules ratelimit.Rules,
	sms SMSSender,
	cacheClient *cache.Client,
	cfg config.Auth,
	smsCfg config.SMS,
	metrics *observability.Metrics,
	logger *slog.Logger,
) *Service {
	return &Service{
		repo: repo, tokens: tokens, limiter: limiter, rules: rules, sms: sms,
		cache: cacheClient, cfg: cfg, smsCfg: smsCfg, metrics: metrics, logger: logger,
	}
}

// RequestOTPInput describes a login start.
type RequestOTPInput struct {
	Phone string
	IP    string
}

// RequestOTPResult tells the client how long to wait and whether the account
// will need a second factor.
type RequestOTPResult struct {
	ExpiresIn      int  `json:"expires_in"`
	ResendAfter    int  `json:"resend_after"`
	Length         int  `json:"length"`
	TwoStepPending bool `json:"two_step_required"`
	// DebugCode is populated only when SMS_ECHO_CODES is on, which config
	// refuses to allow in production.
	DebugCode string `json:"debug_code,omitempty"`
}

const resendCooldown = 60 * time.Second

// RequestOTP issues a verification code.
//
// The response is deliberately identical for known and unknown numbers: it
// never reveals whether an account exists (§33).
func (s *Service) RequestOTP(ctx context.Context, in RequestOTPInput) (*RequestOTPResult, error) {
	phone, err := NormalizePhone(in.Phone)
	if err != nil {
		s.metrics.AuthAttempts.WithLabelValues("otp_request", "invalid_phone").Inc()
		return nil, httpx.Validation("Phone number is not valid").
			WithField("phone", "must be a valid phone number")
	}

	if allowed, err := s.checkLimit(ctx, s.rules.OTPPerPhone, phone); err != nil || !allowed.Allowed {
		s.metrics.AuthAttempts.WithLabelValues("otp_request", "rate_limited").Inc()
		return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
	}
	if in.IP != "" {
		if allowed, err := s.checkLimit(ctx, s.rules.OTPPerIP, in.IP); err != nil || !allowed.Allowed {
			s.metrics.AuthAttempts.WithLabelValues("otp_request", "rate_limited").Inc()
			return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
		}
	}

	// Enforce a resend cooldown on top of the hourly limit so a user cannot
	// burn their whole quota with a double tap.
	cooldownKey := "otp:cooldown:" + phone
	fresh, err := s.cache.SetNX(ctx, cooldownKey, []byte("1"), resendCooldown)
	if err == nil && !fresh {
		remaining, _ := s.cache.TTL(ctx, cooldownKey)
		if remaining < 0 {
			remaining = resendCooldown
		}
		return nil, httpx.RateLimited(int(remaining.Seconds()))
	}

	code, err := security.NumericCode(s.cfg.OTPLength)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	if _, err := s.repo.CreateOTPChallenge(ctx, phone, security.HashToken(code),
		"login", s.cfg.OTPMaxAttempts, s.cfg.OTPTTL, in.IP); err != nil {
		return nil, httpx.Internal(err)
	}

	if err := s.sms.Send(ctx, phone, otpMessage(code, s.cfg.OTPTTL)); err != nil {
		// The challenge stays valid: a transient gateway failure should not
		// force the user to restart, and they can request a resend.
		s.logger.Error("failed to dispatch OTP",
			slog.String("phone", MaskPhone(phone)),
			slog.String("provider", s.sms.Name()),
			slog.Any("error", err))
		s.metrics.AuthAttempts.WithLabelValues("otp_request", "sms_failed").Inc()
		return nil, httpx.Unavailable("Could not send the verification code, please try again")
	}

	s.metrics.AuthAttempts.WithLabelValues("otp_request", "sent").Inc()

	result := &RequestOTPResult{
		ExpiresIn:   int(s.cfg.OTPTTL.Seconds()),
		ResendAfter: int(resendCooldown.Seconds()),
		Length:      s.cfg.OTPLength,
	}
	if s.smsCfg.EchoCodes {
		result.DebugCode = code
	}
	return result, nil
}

// VerifyOTPInput carries the code plus the device registration details.
type VerifyOTPInput struct {
	Phone      string
	Code       string
	Password   string // two-step password, when the account has one
	DeviceName string
	Platform   string
	AppVersion string
	UserAgent  string
	IP         string
}

// TokenPair is what a successful login returns.
type TokenPair struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	TokenType    string    `json:"token_type"`
	ExpiresAt    time.Time `json:"expires_at"`
	ExpiresIn    int       `json:"expires_in"`
	UserID       uuid.UUID `json:"user_id"`
	DeviceID     uuid.UUID `json:"device_id"`
	SessionID    uuid.UUID `json:"session_id"`
	IsNewAccount bool      `json:"is_new_account"`
}

// VerifyOTP checks the code, creates the account on first login, registers the
// device and issues the token pair.
func (s *Service) VerifyOTP(ctx context.Context, in VerifyOTPInput) (*TokenPair, error) {
	phone, err := NormalizePhone(in.Phone)
	if err != nil {
		return nil, httpx.Validation("Phone number is not valid").
			WithField("phone", "must be a valid phone number")
	}

	if in.IP != "" {
		if allowed, err := s.checkLimit(ctx, s.rules.LoginPerIP, in.IP); err != nil || !allowed.Allowed {
			return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
		}
	}

	challenge, err := s.repo.LatestOTPChallenge(ctx, phone, "login")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.metrics.AuthAttempts.WithLabelValues("otp_verify", "no_challenge").Inc()
			return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "Verification code is not valid")
		}
		return nil, httpx.Internal(err)
	}

	if time.Now().After(challenge.ExpiresAt) {
		s.metrics.AuthAttempts.WithLabelValues("otp_verify", "expired").Inc()
		return nil, httpx.Unauthorized(httpx.CodeOTPExpired, "Verification code has expired")
	}
	if challenge.Attempts >= challenge.MaxAttempts {
		s.metrics.AuthAttempts.WithLabelValues("otp_verify", "attempts_burned").Inc()
		return nil, httpx.Unauthorized(httpx.CodeOTPAttemptsBurned,
			"Too many incorrect attempts, request a new code")
	}

	if !security.CompareTokenHash(strings.TrimSpace(in.Code), challenge.CodeHash) {
		attempts, burnErr := s.repo.BurnOTPAttempt(ctx, challenge.ID)
		if burnErr != nil {
			s.logger.Error("failed to record OTP attempt", slog.Any("error", burnErr))
		}
		s.metrics.AuthAttempts.WithLabelValues("otp_verify", "wrong_code").Inc()
		if attempts >= challenge.MaxAttempts {
			return nil, httpx.Unauthorized(httpx.CodeOTPAttemptsBurned,
				"Too many incorrect attempts, request a new code")
		}
		return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "Verification code is not valid")
	}

	user, err := s.repo.UserByPhone(ctx, phone)
	isNew := false
	switch {
	case errors.Is(err, ErrNotFound):
		isNew = true
	case err != nil:
		return nil, httpx.Internal(err)
	}

	// Two-step verification is checked *before* the challenge is consumed, so
	// a user who mistypes their password can retry with the same code (§57).
	if user != nil && user.TwoStepEnabled && user.PasswordHash != nil {
		if in.Password == "" {
			hint := ""
			if user.TwoStepHint != nil {
				hint = *user.TwoStepHint
			}
			return nil, (&httpx.Error{
				Status:  401,
				Code:    httpx.CodeTwoStepRequired,
				Message: "This account is protected by a password",
			}).WithField("hint", hint)
		}
		ok, verifyErr := security.VerifyPassword(in.Password, *user.PasswordHash)
		if verifyErr != nil {
			return nil, httpx.Internal(verifyErr)
		}
		if !ok {
			s.metrics.AuthAttempts.WithLabelValues("two_step", "wrong_password").Inc()
			_ = s.repo.RecordLogin(ctx, user.ID, "two_step_failed", in.IP, in.UserAgent, in.Platform, false)
			return nil, httpx.Unauthorized(httpx.CodeInvalidPassword, "Password is not correct")
		}
	}

	// Consume the challenge exactly once. A concurrent verification loses here.
	consumed, err := s.repo.ConsumeOTPChallenge(ctx, challenge.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !consumed {
		return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "Verification code is not valid")
	}

	if isNew {
		phoneHash := security.HashPhone(phone, s.cfg.PhoneHashPepper)
		user, err = s.repo.CreateUser(ctx, phone, phoneHash, "")
		if err != nil {
			return nil, httpx.Internal(err)
		}
	}

	if !user.IsActive() {
		if user.Status == "banned" {
			return nil, httpx.Forbidden(httpx.CodeAccountBanned, "This account has been suspended")
		}
		return nil, httpx.Forbidden(httpx.CodeAccountDeleted, "This account is no longer available")
	}

	pair, err := s.issueTokens(ctx, user, in.DeviceName, in.Platform, in.AppVersion, in.UserAgent, in.IP)
	if err != nil {
		return nil, err
	}
	pair.IsNewAccount = isNew

	_ = s.repo.RecordLogin(ctx, user.ID, "login", in.IP, in.UserAgent, in.Platform, true)
	_ = s.limiter.Reset(ctx, s.rules.OTPPerPhone, phone)
	s.metrics.AuthAttempts.WithLabelValues("otp_verify", "success").Inc()

	return pair, nil
}

func (s *Service) issueTokens(ctx context.Context, user *User, deviceName, platform, appVersion, userAgent, ip string) (*TokenPair, error) {
	device, err := s.repo.UpsertDevice(ctx, user.ID, deviceName, normalizePlatform(platform), appVersion, ip, s.cfg.MaxDevicesPerUser)
	if err != nil {
		if errors.Is(err, ErrTooManyDevices) {
			return nil, httpx.Forbidden(httpx.CodeTooManyDevices,
				"You have reached the maximum number of active devices, remove one to continue")
		}
		return nil, httpx.Internal(err)
	}

	refreshToken, refreshHash, err := s.tokens.GenerateRefreshToken()
	if err != nil {
		return nil, httpx.Internal(err)
	}

	session, err := s.repo.CreateSession(ctx, user.ID, device.ID, refreshHash,
		time.Now().Add(s.tokens.RefreshTTL()), userAgent, ip)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	accessToken, _, expiresAt, err := s.tokens.IssueAccessToken(user.ID, device.ID, session.ID, user.TokenVersion)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    "Bearer",
		ExpiresAt:    expiresAt,
		ExpiresIn:    int(time.Until(expiresAt).Seconds()),
		UserID:       user.ID,
		DeviceID:     device.ID,
		SessionID:    session.ID,
	}, nil
}

// Refresh rotates the refresh token and mints a new access token (§33).
//
// Rotation is mandatory: the presented token is invalidated whether or not the
// caller receives the response, so a stolen token has a single-use lifetime.
func (s *Service) Refresh(ctx context.Context, refreshToken, ip string) (*TokenPair, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, httpx.Unauthorized(httpx.CodeTokenInvalid, "Refresh token is required")
	}

	newToken, newHash, err := s.tokens.GenerateRefreshToken()
	if err != nil {
		return nil, httpx.Internal(err)
	}

	session, err := s.repo.RotateRefreshToken(ctx,
		security.HashToken(refreshToken), newHash, time.Now().Add(s.tokens.RefreshTTL()), ip)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.metrics.AuthAttempts.WithLabelValues("refresh", "rejected").Inc()
			return nil, httpx.Unauthorized(httpx.CodeSessionRevoked,
				"This session is no longer valid, please sign in again")
		}
		return nil, httpx.Internal(err)
	}

	user, err := s.repo.UserByID(ctx, session.UserID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !user.IsActive() {
		return nil, httpx.Forbidden(httpx.CodeAccountBanned, "This account is not available")
	}

	accessToken, _, expiresAt, err := s.tokens.IssueAccessToken(
		user.ID, session.DeviceID, session.ID, user.TokenVersion)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	_ = s.repo.TouchDevice(ctx, session.DeviceID, ip)
	s.metrics.AuthAttempts.WithLabelValues("refresh", "success").Inc()

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: newToken,
		TokenType:    "Bearer",
		ExpiresAt:    expiresAt,
		ExpiresIn:    int(time.Until(expiresAt).Seconds()),
		UserID:       user.ID,
		DeviceID:     session.DeviceID,
		SessionID:    session.ID,
	}, nil
}

// Logout revokes the calling session.
func (s *Service) Logout(ctx context.Context, userID, sessionID uuid.UUID) error {
	if err := s.repo.RevokeSession(ctx, userID, sessionID, "user_logout"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // Already gone; logout is idempotent.
		}
		return httpx.Internal(err)
	}
	s.denySession(ctx, sessionID)
	return nil
}

// RevokeSession terminates another device's session (§56).
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	if err := s.repo.RevokeSession(ctx, userID, sessionID, "user_revoked"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Session not found")
		}
		return httpx.Internal(err)
	}
	s.denySession(ctx, sessionID)
	return nil
}

// RevokeOtherSessions signs every other device out.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keepSessionID uuid.UUID) (int, error) {
	revoked, err := s.repo.RevokeAllSessions(ctx, userID, keepSessionID, "user_revoked_all")
	if err != nil {
		return 0, httpx.Internal(err)
	}
	for _, id := range revoked {
		s.denySession(ctx, id)
	}
	return len(revoked), nil
}

// SetTwoStepPassword enables, changes or removes the second factor.
func (s *Service) SetTwoStepPassword(ctx context.Context, userID uuid.UUID, currentPassword, newPassword, hint string) error {
	user, err := s.repo.UserByID(ctx, userID)
	if err != nil {
		return httpx.Internal(err)
	}

	if user.TwoStepEnabled && user.PasswordHash != nil {
		ok, err := security.VerifyPassword(currentPassword, *user.PasswordHash)
		if err != nil {
			return httpx.Internal(err)
		}
		if !ok {
			return httpx.Unauthorized(httpx.CodeInvalidPassword, "Current password is not correct")
		}
	}

	if newPassword == "" {
		// Disabling the second factor.
		if err := s.repo.SetTwoStepPassword(ctx, userID, nil, nil); err != nil {
			return httpx.Internal(err)
		}
		return nil
	}

	hash, err := security.HashPassword(newPassword)
	if err != nil {
		if errors.Is(err, security.ErrPasswordTooShort) {
			return httpx.Validation("Password is too short").
				WithField("password", "must be at least 8 characters")
		}
		return httpx.Internal(err)
	}

	var hintPtr *string
	if hint != "" {
		hintPtr = &hint
	}
	if err := s.repo.SetTwoStepPassword(ctx, userID, &hash, hintPtr); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	sessions, err := s.repo.ListSessions(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return sessions, nil
}

func (s *Service) ListDevices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	devices, err := s.repo.ListDevices(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return devices, nil
}

func (s *Service) LoginHistory(ctx context.Context, userID uuid.UUID, limit int) ([]LoginRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	records, err := s.repo.LoginHistory(ctx, userID, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return records, nil
}

// denySession adds a revoked session to the Redis denylist so already-issued
// access tokens stop working immediately rather than at their natural expiry.
// The entry only has to outlive the access-token TTL.
func (s *Service) denySession(ctx context.Context, sessionID uuid.UUID) {
	key := sessionDenyKey(sessionID)
	if err := s.cache.Set(ctx, key, []byte("1"), s.tokens.AccessTTL()+time.Minute); err != nil {
		s.logger.Error("failed to add session to denylist",
			slog.String("session_id", sessionID.String()),
			slog.Any("error", err))
	}
}

func sessionDenyKey(sessionID uuid.UUID) string {
	return "auth:denied_session:" + sessionID.String()
}

func (s *Service) checkLimit(ctx context.Context, rule ratelimit.Rule, subject string) (ratelimit.Result, error) {
	result, err := s.limiter.Allow(ctx, rule, subject)
	if err != nil {
		s.logger.Error("rate limiter failed",
			slog.String("rule", rule.Name), slog.Any("error", err))
	}
	return result, err
}

func normalizePlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "android":
		return "android"
	case "ios":
		return "ios"
	case "web":
		return "web"
	case "desktop", "macos", "windows", "linux":
		return "desktop"
	default:
		return "unknown"
	}
}
