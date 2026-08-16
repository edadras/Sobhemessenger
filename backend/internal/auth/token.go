// Package auth owns identity: OTP login, JWT access tokens, per-device refresh
// token rotation, two-step verification and session management (§11, §56, §57).
package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/security"
)

// Claims is the SOBH access-token body. Everything the authorization layer
// needs is here, so the common path never touches the database.
type Claims struct {
	jwt.RegisteredClaims
	DeviceID  string `json:"did"`
	SessionID string `json:"sid"`
	// TokenVersion is bumped when a user changes their two-step password or
	// revokes everything, invalidating tokens issued before the change.
	TokenVersion int `json:"tv"`
}

// TokenService issues and verifies access tokens. Refresh tokens are opaque
// random strings stored as hashes — they carry no claims and cannot be
// replayed against another device.
type TokenService struct {
	keys        map[string][]byte
	activeKeyID string
	accessTTL   time.Duration
	refreshTTL  time.Duration
	issuer      string
	audience    string
}

var (
	ErrTokenInvalid = errors.New("auth: token is invalid")
	ErrTokenExpired = errors.New("auth: token has expired")
)

func NewTokenService(cfg config.Auth) (*TokenService, error) {
	if len(cfg.SigningKeys) == 0 {
		return nil, errors.New("auth: no signing keys configured")
	}
	keys := make(map[string][]byte, len(cfg.SigningKeys))
	for kid, secret := range cfg.SigningKeys {
		keys[kid] = []byte(secret)
	}
	return &TokenService{
		keys:        keys,
		activeKeyID: cfg.ActiveKeyID,
		accessTTL:   cfg.AccessTokenTTL,
		refreshTTL:  cfg.RefreshTokenTTL,
		issuer:      cfg.Issuer,
		audience:    cfg.Audience,
	}, nil
}

func (s *TokenService) AccessTTL() time.Duration  { return s.accessTTL }
func (s *TokenService) RefreshTTL() time.Duration { return s.refreshTTL }

// IssueAccessToken mints a short-lived HS256 token bound to one session.
func (s *TokenService) IssueAccessToken(userID, deviceID, sessionID uuid.UUID, tokenVersion int) (string, string, time.Time, error) {
	now := time.Now().UTC()
	expiresAt := now.Add(s.accessTTL)
	tokenID := uuid.NewString()

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID.String(),
			Issuer:    s.issuer,
			Audience:  jwt.ClaimStrings{s.audience},
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
		DeviceID:     deviceID.String(),
		SessionID:    sessionID.String(),
		TokenVersion: tokenVersion,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	token.Header["kid"] = s.activeKeyID

	signed, err := token.SignedString(s.keys[s.activeKeyID])
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("auth: sign access token: %w", err)
	}
	return signed, tokenID, expiresAt, nil
}

// ParseAccessToken verifies signature, expiry, issuer and audience. Key
// rotation is handled by looking the `kid` header up in the configured set, so
// tokens signed with a retired key keep working until they expire (§33).
func (s *TokenService) ParseAccessToken(raw string) (*Claims, error) {
	claims := &Claims{}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(s.issuer),
		jwt.WithAudience(s.audience),
		jwt.WithExpirationRequired(),
	)

	_, err := parser.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		key, ok := s.keys[kid]
		if !ok {
			return nil, fmt.Errorf("auth: unknown signing key %q", kid)
		}
		return key, nil
	})
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %s", ErrTokenInvalid, err)
	}

	if claims.Subject == "" || claims.SessionID == "" || claims.DeviceID == "" {
		return nil, ErrTokenInvalid
	}
	return claims, nil
}

// GenerateRefreshToken returns the opaque token handed to the client and the
// hash persisted server-side. The plaintext is never stored.
func (s *TokenService) GenerateRefreshToken() (token string, hash []byte, err error) {
	token, err = security.RandomToken(48)
	if err != nil {
		return "", nil, err
	}
	return token, security.HashToken(token), nil
}

// ParsedIdentity converts string claims into typed ids.
func (c *Claims) ParsedIdentity() (userID, deviceID, sessionID uuid.UUID, err error) {
	if userID, err = uuid.Parse(c.Subject); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, ErrTokenInvalid
	}
	if deviceID, err = uuid.Parse(c.DeviceID); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, ErrTokenInvalid
	}
	if sessionID, err = uuid.Parse(c.SessionID); err != nil {
		return uuid.Nil, uuid.Nil, uuid.Nil, ErrTokenInvalid
	}
	return userID, deviceID, sessionID, nil
}
