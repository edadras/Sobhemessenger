package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// Middleware authenticates requests and populates the request principal.
type Middleware struct {
	tokens *TokenService
	repo   *Repository
	cache  *cache.Client
	logger *slog.Logger
}

func NewMiddleware(tokens *TokenService, repo *Repository, cacheClient *cache.Client, logger *slog.Logger) *Middleware {
	return &Middleware{tokens: tokens, repo: repo, cache: cacheClient, logger: logger}
}

// RequireAuth rejects anything without a valid, non-revoked access token.
func (m *Middleware) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := m.authenticate(r)
		if err != nil {
			httpx.Fail(w, r, err)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithPrincipal(r.Context(), principal)))
	})
}

// OptionalAuth attaches a principal when a valid token is present but lets
// anonymous callers through. Used by public news endpoints, which personalise
// their response for signed-in readers.
func (m *Middleware) OptionalAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) == "" {
			next.ServeHTTP(w, r)
			return
		}
		principal, err := m.authenticate(r)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(httpx.WithPrincipal(r.Context(), principal)))
	})
}

// RequirePermission gates an admin route on an RBAC permission (§32).
func (m *Middleware) RequirePermission(permission string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal := httpx.PrincipalFrom(r.Context())
			if principal == nil {
				httpx.Fail(w, r, httpx.Unauthorized(httpx.CodeUnauthorized, "Authentication is required"))
				return
			}
			if !principal.HasPermission(permission) {
				m.logger.Warn("admin permission denied",
					slog.String("user_id", principal.UserID.String()),
					slog.String("permission", permission),
					slog.String("path", r.URL.Path))
				httpx.Fail(w, r, httpx.Forbidden(httpx.CodeForbidden,
					"You do not have permission to perform this action"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AuthenticateToken validates a raw token. The WebSocket handshake uses this
// directly, since it cannot rely on the HTTP middleware chain.
func (m *Middleware) AuthenticateToken(ctx context.Context, raw string) (*httpx.Principal, error) {
	claims, err := m.tokens.ParseAccessToken(raw)
	if err != nil {
		switch {
		case errors.Is(err, ErrTokenExpired):
			return nil, httpx.Unauthorized(httpx.CodeTokenExpired, "Access token has expired")
		default:
			return nil, httpx.Unauthorized(httpx.CodeTokenInvalid, "Access token is not valid")
		}
	}

	userID, deviceID, sessionID, err := claims.ParsedIdentity()
	if err != nil {
		return nil, httpx.Unauthorized(httpx.CodeTokenInvalid, "Access token is not valid")
	}

	// Revocation check. The denylist is the fast path; a Redis outage falls
	// through to the database rather than trusting a revoked token.
	if _, err := m.cache.Get(ctx, sessionDenyKey(sessionID)); err == nil {
		return nil, httpx.Unauthorized(httpx.CodeSessionRevoked, "This session has been signed out")
	} else if !errors.Is(err, cache.ErrNotFound) {
		session, dbErr := m.repo.SessionByID(ctx, sessionID)
		if dbErr != nil || session.RevokedAt != nil {
			return nil, httpx.Unauthorized(httpx.CodeSessionRevoked, "This session has been signed out")
		}
	}

	// token_version invalidates everything issued before a password change or
	// a global sign-out, without a per-request session lookup.
	user, err := m.userWithCache(ctx, userID)
	if err != nil {
		return nil, err
	}
	if user.TokenVersion != claims.TokenVersion {
		return nil, httpx.Unauthorized(httpx.CodeSessionRevoked, "Please sign in again")
	}
	if !user.IsActive() {
		if user.Status == "banned" {
			return nil, httpx.Forbidden(httpx.CodeAccountBanned, "This account has been suspended")
		}
		return nil, httpx.Forbidden(httpx.CodeAccountDeleted, "This account is no longer available")
	}

	principal := &httpx.Principal{
		UserID:    userID,
		DeviceID:  deviceID,
		SessionID: sessionID,
		TokenID:   claims.ID,
	}

	// Admin roles are only looked up for accounts that have them. The lookup
	// is cached to keep it off the hot path for ordinary users.
	roles, permissions, err := m.adminRolesWithCache(ctx, userID)
	if err == nil {
		principal.AdminRoles = roles
		principal.Permissions = permissions
	}

	return principal, nil
}

func (m *Middleware) authenticate(r *http.Request) (*httpx.Principal, error) {
	raw := bearerToken(r)
	if raw == "" {
		return nil, httpx.Unauthorized(httpx.CodeUnauthorized, "Authentication is required")
	}
	return m.AuthenticateToken(r.Context(), raw)
}

// userCacheTTL is short: it only has to absorb bursts, and a stale entry can
// delay a ban by at most this long.
const userCacheTTL = 30 * time.Second

func (m *Middleware) userWithCache(ctx context.Context, userID uuid.UUID) (*User, error) {
	key := "auth:user_state:" + userID.String()
	if raw, err := m.cache.Get(ctx, key); err == nil {
		if user, ok := decodeUserState(raw); ok {
			return user, nil
		}
	}

	user, err := m.repo.UserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.Unauthorized(httpx.CodeTokenInvalid, "Account not found")
		}
		return nil, httpx.Internal(err)
	}
	if encoded, ok := encodeUserState(user); ok {
		_ = m.cache.Set(ctx, key, encoded, userCacheTTL)
	}
	return user, nil
}

func (m *Middleware) adminRolesWithCache(ctx context.Context, userID uuid.UUID) ([]string, []string, error) {
	key := "auth:admin_roles:" + userID.String()
	if raw, err := m.cache.Get(ctx, key); err == nil {
		roles, permissions := decodeRoles(raw)
		return roles, permissions, nil
	}

	roles, permissions, err := m.repo.AdminRolesFor(ctx, userID)
	if err != nil {
		return nil, nil, err
	}
	_ = m.cache.Set(ctx, key, encodeRoles(roles, permissions), userCacheTTL)
	return roles, permissions, nil
}

// InvalidateUserCache drops the cached account state, used after a ban or a
// role change so the effect is immediate.
func (m *Middleware) InvalidateUserCache(ctx context.Context, userID uuid.UUID) {
	_ = m.cache.Delete(ctx,
		"auth:user_state:"+userID.String(),
		"auth:admin_roles:"+userID.String())
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// The cached account state is a compact pipe-delimited record rather than
// JSON: it is read on every authenticated request, and the fields are fixed.
func encodeUserState(u *User) ([]byte, bool) {
	return []byte(u.Status + "|" + strconv.Itoa(u.TokenVersion) + "|" + boolChar(u.DeletedAt != nil)), true
}

func decodeUserState(raw []byte) (*User, bool) {
	parts := strings.Split(string(raw), "|")
	if len(parts) != 3 {
		return nil, false
	}
	version, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, false
	}
	user := &User{Status: parts[0], TokenVersion: version}
	if parts[2] == "1" {
		now := time.Now()
		user.DeletedAt = &now
	}
	return user, true
}

func encodeRoles(roles, permissions []string) []byte {
	return []byte(strings.Join(roles, ",") + "|" + strings.Join(permissions, ","))
}

func decodeRoles(raw []byte) ([]string, []string) {
	rolePart, permPart, _ := strings.Cut(string(raw), "|")
	return splitNonEmpty(rolePart), splitNonEmpty(permPart)
}

func splitNonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func boolChar(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
