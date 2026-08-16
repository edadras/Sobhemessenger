package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/sobh/messenger/backend/internal/config"
)

// SMSSender delivers the OTP. Providers differ per market, so the transport is
// an interface and the service never depends on a specific vendor.
type SMSSender interface {
	Send(ctx context.Context, phone, message string) error
	Name() string
}

// NewSMSSender builds the sender named by SMS_PROVIDER.
func NewSMSSender(cfg config.SMS, logger *slog.Logger) (SMSSender, error) {
	switch cfg.Provider {
	case "log":
		return &logSender{logger: logger}, nil
	case "http":
		if cfg.BaseURL == "" || cfg.APIKey == "" {
			return nil, fmt.Errorf("auth: SMS_BASE_URL and SMS_API_KEY are required for the http provider")
		}
		return &httpSender{
			cfg:    cfg,
			client: &http.Client{Timeout: cfg.Timeout},
		}, nil
	default:
		return nil, fmt.Errorf("auth: unknown SMS provider %q", cfg.Provider)
	}
}

// logSender is the development transport. It records that a message would have
// been sent without ever writing the code itself to the log.
type logSender struct {
	logger *slog.Logger
}

func (s *logSender) Name() string { return "log" }

func (s *logSender) Send(_ context.Context, phone, _ string) error {
	s.logger.Info("OTP dispatch (log provider)",
		slog.String("phone", MaskPhone(phone)),
		slog.String("note", "code omitted; set SMS_ECHO_CODES=true in development to receive it in the API response"),
	)
	return nil
}

// httpSender posts to a generic JSON SMS gateway. The request shape below is
// the common {to, from, text} contract; adapt per operator contract if needed.
type httpSender struct {
	cfg    config.SMS
	client *http.Client
}

func (s *httpSender) Name() string { return "http" }

func (s *httpSender) Send(ctx context.Context, phone, message string) error {
	body, err := json.Marshal(map[string]string{
		"to":   phone,
		"from": s.cfg.Sender,
		"text": message,
	})
	if err != nil {
		return fmt.Errorf("auth: marshal SMS payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("auth: build SMS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: send SMS: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Read a bounded amount so a misbehaving gateway cannot flood the log.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("auth: SMS gateway returned %d: %s", resp.StatusCode, bytes.TrimSpace(detail))
	}
	return nil
}

// otpMessage renders the SMS body. Persian first: it is the primary market.
func otpMessage(code string, ttl time.Duration) string {
	minutes := int(ttl.Minutes())
	if minutes < 1 {
		minutes = 1
	}
	return fmt.Sprintf("کد ورود شما به صبح: %s\nاعتبار: %d دقیقه\n\nSOBH verification code: %s", code, minutes, code)
}
