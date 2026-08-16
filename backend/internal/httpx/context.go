package httpx

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

type contextKey int

const (
	ctxKeyRequestID contextKey = iota
	ctxKeyLogger
	ctxKeyPrincipal
	ctxKeyClientIP
)

// Principal is the authenticated caller behind a request. It is set by the
// authentication middleware and is the only source of identity handlers trust —
// no handler ever reads a user id out of the request body or path.
type Principal struct {
	UserID    uuid.UUID
	DeviceID  uuid.UUID
	SessionID uuid.UUID
	// AdminRoles is empty for ordinary users and drives RBAC checks (§32).
	AdminRoles  []string
	Permissions []string
	TokenID     string
}

// HasPermission reports whether the principal holds an admin permission.
// The "*" wildcard belongs to super admins only.
func (p *Principal) HasPermission(permission string) bool {
	if p == nil {
		return false
	}
	for _, held := range p.Permissions {
		if held == "*" || held == permission {
			return true
		}
	}
	return false
}

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, logger)
}

// LoggerFrom returns the request-scoped logger, falling back to the default so
// a caller never has to nil-check.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if v, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && v != nil {
		return v
	}
	return slog.Default()
}

func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// PrincipalFrom returns the authenticated caller, or nil on anonymous routes.
func PrincipalFrom(ctx context.Context) *Principal {
	if v, ok := ctx.Value(ctxKeyPrincipal).(*Principal); ok {
		return v
	}
	return nil
}

// MustPrincipal returns the caller for routes mounted behind RequireAuth.
// It returns an error rather than panicking so a routing mistake degrades to a
// 401 instead of taking down the process.
func MustPrincipal(ctx context.Context) (*Principal, error) {
	p := PrincipalFrom(ctx)
	if p == nil {
		return nil, Unauthorized(CodeUnauthorized, "Authentication is required")
	}
	return p, nil
}

func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxKeyClientIP, ip)
}

func ClientIPFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyClientIP).(string); ok {
		return v
	}
	return ""
}
