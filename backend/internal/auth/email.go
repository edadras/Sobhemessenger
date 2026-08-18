package auth

// Email delivery for account recovery (§4).
//
// The transport is an interface for the same reason the SMS one is: providers
// differ per deployment, and the service must never depend on a specific
// vendor. The difference is that email is optional. A server with no mail
// transport refuses to set a recovery address rather than accepting one it
// could never send to — an unverified recovery address is worse than none,
// because it is a second way into the account that the owner never confirmed.

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/smtp"
	"strings"
	"time"

	"github.com/sobh/messenger/backend/internal/config"
)

// EmailSender delivers one message.
type EmailSender interface {
	Send(ctx context.Context, to, subject, body string) error
	Name() string
}

// NewEmailSender builds the sender named by EMAIL_PROVIDER.
//
// "log" is the development transport and also the way to run without email
// recovery at all: it reports that it is not a real transport, and the service
// refuses to enrol an address when that is the case.
func NewEmailSender(cfg config.Email, logger *slog.Logger) (EmailSender, error) {
	switch cfg.Provider {
	case "log":
		return &logEmailSender{logger: logger}, nil
	case "smtp":
		if cfg.SMTPHost == "" || cfg.From == "" {
			return nil, fmt.Errorf("auth: SMTP_HOST and EMAIL_FROM are required for the smtp provider")
		}
		return &smtpSender{cfg: cfg}, nil
	default:
		return nil, fmt.Errorf("auth: unknown email provider %q", cfg.Provider)
	}
}

// Deliverable reports whether a sender can actually reach an inbox. The log
// transport cannot, and account recovery must not be offered on top of it.
func Deliverable(sender EmailSender) bool {
	return sender != nil && sender.Name() != "log"
}

type logEmailSender struct {
	logger *slog.Logger
}

func (s *logEmailSender) Name() string { return "log" }

func (s *logEmailSender) Send(_ context.Context, to, subject, _ string) error {
	// The address is masked and the body omitted for the same reason the SMS
	// transport omits the code: a development log is still a log.
	s.logger.Info("email dispatch (log provider)",
		slog.String("to", MaskEmail(to)),
		slog.String("subject", subject),
		slog.String("note", "body omitted; set EMAIL_ECHO_CODES=true in development to receive it in the API response"))
	return nil
}

// smtpSender speaks SMTP directly rather than through a vendor SDK, so a
// deployment can point it at whatever relay it already runs.
type smtpSender struct {
	cfg config.Email
}

func (s *smtpSender) Name() string { return "smtp" }

func (s *smtpSender) Send(ctx context.Context, to, subject, body string) error {
	address := net.JoinHostPort(s.cfg.SMTPHost, fmt.Sprint(s.cfg.SMTPPort))

	dialer := &net.Dialer{Timeout: s.cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return fmt.Errorf("auth: dial smtp: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(s.cfg.Timeout))
	}

	client, err := smtp.NewClient(conn, s.cfg.SMTPHost)
	if err != nil {
		return fmt.Errorf("auth: smtp handshake: %w", err)
	}
	defer func() { _ = client.Quit() }()

	if s.cfg.StartTLS {
		if ok, _ := client.Extension("STARTTLS"); ok {
			// ServerName is set explicitly so the certificate is checked
			// against the host we meant to reach, not whatever it claims.
			if err := client.StartTLS(&tls.Config{ServerName: s.cfg.SMTPHost}); err != nil {
				return fmt.Errorf("auth: starttls: %w", err)
			}
		}
	}

	if s.cfg.SMTPUsername != "" {
		auth := smtp.PlainAuth("", s.cfg.SMTPUsername, s.cfg.SMTPPassword, s.cfg.SMTPHost)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("auth: smtp auth: %w", err)
		}
	}

	if err := client.Mail(s.cfg.From); err != nil {
		return fmt.Errorf("auth: smtp from: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("auth: smtp rcpt: %w", err)
	}

	writer, err := client.Data()
	if err != nil {
		return fmt.Errorf("auth: smtp data: %w", err)
	}
	message := buildMessage(s.cfg, to, subject, body)
	if _, err := writer.Write([]byte(message)); err != nil {
		return fmt.Errorf("auth: write message: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("auth: close message: %w", err)
	}
	return nil
}

// buildMessage assembles the RFC 5322 headers and body.
//
// The header values are stripped of CR and LF before they go in. Without that,
// an address or subject carrying a newline could inject headers of its own —
// a second Bcc, say — and the subject here is not fully under our control.
func buildMessage(cfg config.Email, to, subject, body string) string {
	var builder strings.Builder
	builder.WriteString("From: " + headerSafe(cfg.FromName) + " <" + headerSafe(cfg.From) + ">\r\n")
	builder.WriteString("To: " + headerSafe(to) + "\r\n")
	builder.WriteString("Subject: " + headerSafe(subject) + "\r\n")
	builder.WriteString("MIME-Version: 1.0\r\n")
	builder.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	builder.WriteString("\r\n")
	builder.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return builder.String()
}

func headerSafe(value string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(value)
}

// MaskEmail keeps an address out of the logs while leaving enough to tell two
// apart when reading them.
func MaskEmail(address string) string {
	at := strings.LastIndex(address, "@")
	if at <= 0 {
		return "***"
	}
	local, domain := address[:at], address[at+1:]
	if len(local) <= 2 {
		return "**@" + domain
	}
	return local[:1] + strings.Repeat("*", len(local)-2) + local[len(local)-1:] + "@" + domain
}
