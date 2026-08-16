// Package logging builds the structured logger. Output is JSON in every
// deployed environment so Loki/ELK can parse it without a regex (§35).
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/sobh/messenger/backend/internal/config"
)

// redactedKeys never reach the log, whatever an attribute is called upstream.
var redactedKeys = map[string]struct{}{
	"password":      {},
	"otp":           {},
	"code":          {},
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"secret":        {},
	"authorization": {},
	"phone_number":  {},
	"push_token":    {},
	"ciphertext":    {},
}

// New builds a logger for the given configuration and installs it as the
// process default.
func New(cfg config.Log, serviceName, nodeID string) *slog.Logger {
	return newWithWriter(cfg, serviceName, nodeID, os.Stdout)
}

func newWithWriter(cfg config.Log, serviceName, nodeID string, w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{
		Level:       parseLevel(cfg.Level),
		ReplaceAttr: redact,
	}

	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		handler = slog.NewTextHandler(w, opts)
	} else {
		handler = slog.NewJSONHandler(w, opts)
	}

	logger := slog.New(handler).With(
		slog.String("service", serviceName),
		slog.String("node", nodeID),
	)
	slog.SetDefault(logger)
	return logger
}

// redact replaces sensitive values rather than dropping the key, so the shape
// of a log line stays stable and the omission is visible during debugging.
func redact(_ []string, attr slog.Attr) slog.Attr {
	if _, sensitive := redactedKeys[strings.ToLower(attr.Key)]; sensitive {
		return slog.String(attr.Key, "[redacted]")
	}
	return attr
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
