package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/auth"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Email account recovery (§4).
//
// The property the whole flow turns on is that an unverified address is worth
// nothing: it is stored, but it cannot recover anything until a code proves
// it. Most of what follows is that claim from a different angle.

func recoveryDB(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}
	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 8, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// captureSender is a real transport for the test's purposes: it delivers, and
// the test reads the inbox. It reports a name other than "log" because that is
// what tells the service email recovery is available at all.
type captureSender struct {
	mu   sync.Mutex
	sent []sentEmail
	fail error
}

type sentEmail struct {
	to      string
	subject string
	body    string
}

func (s *captureSender) Name() string { return "capture" }

func (s *captureSender) Send(_ context.Context, to, subject, body string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.sent = append(s.sent, sentEmail{to: to, subject: subject, body: body})
	return nil
}

func (s *captureSender) last(t *testing.T) sentEmail {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sent) == 0 {
		t.Fatal("no email was sent")
	}
	return s.sent[len(s.sent)-1]
}

func (s *captureSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// recoveryService builds the auth service with a capturing mail transport and
// codes echoed back, so a test can present the code the user would have read.
func recoveryService(t *testing.T, db *database.DB) (*auth.Service, *captureSender) {
	t.Helper()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tokens, err := auth.NewTokenService(config.Auth{
		SigningKeys:     map[string]string{"test": strings.Repeat("k", 32)},
		ActiveKeyID:     "test",
		AccessTokenTTL:  time.Hour,
		RefreshTokenTTL: 24 * time.Hour,
		OTPLength:       6,
	})
	if err != nil {
		t.Fatalf("token service: %v", err)
	}

	service := auth.NewService(auth.NewRepository(db), tokens, nil,
		ratelimit.NewRules(config.RateLimits{}), nil, nil,
		config.Auth{OTPLength: 6}, config.SMS{}, observability.New("auth-test"), logger)

	sender := &captureSender{}
	service.SetEmailSender(sender, true)
	return service, sender
}

func recoveryUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		"+9891"+uuid.NewString()[:9], []byte("recovery")).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// address gives each test its own, so two runs never collide on the recovery
// lookup.
func address() string { return "person-" + uuid.NewString()[:8] + "@example.com" }

func TestAnEnrolledAddressDoesNothingUntilItIsProved(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	email := address()

	if _, err := service.SetRecoveryEmail(ctx, userID, email); err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}

	status, err := service.EmailStatus(ctx, userID)
	if err != nil {
		t.Fatalf("EmailStatus: %v", err)
	}
	if status.Verified {
		t.Error("an address is verified before anyone proved it")
	}

	// And it cannot start a recovery. This is the point of the whole design:
	// a stolen session that adds an attacker's address still cannot recover
	// anything without reaching that inbox.
	if _, err := service.StartEmailRecovery(ctx, email); err != nil {
		t.Fatalf("StartEmailRecovery: %v", err)
	}
	if err := service.CompleteEmailRecovery(ctx, email, "000000"); err == nil {
		t.Error("an unverified address completed a recovery")
	}
}

func TestTheCodeSentToTheAddressProvesIt(t *testing.T) {
	db := recoveryDB(t)
	service, sender := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	email := address()

	code, err := service.SetRecoveryEmail(ctx, userID, email)
	if err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}
	if code == "" {
		t.Fatal("no code was echoed; the test cannot present one the user would have read")
	}

	sent := sender.last(t)
	if sent.to != email {
		t.Errorf("the code went to %q, want %q", sent.to, email)
	}
	if !strings.Contains(sent.body, code) {
		t.Error("the email does not carry the code the service issued")
	}

	if err := service.VerifyRecoveryEmail(ctx, userID, code); err != nil {
		t.Fatalf("VerifyRecoveryEmail: %v", err)
	}
	status, err := service.EmailStatus(ctx, userID)
	if err != nil {
		t.Fatalf("EmailStatus: %v", err)
	}
	if !status.Verified {
		t.Error("the address is still unverified after the right code")
	}
	if strings.Contains(status.Email, "@example.com") && !strings.Contains(status.Email, "*") {
		t.Errorf("the address came back unmasked as %q", status.Email)
	}
}

func TestAWrongCodeCostsAnAttempt(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	code, err := service.SetRecoveryEmail(ctx, userID, address())
	if err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}

	wrong := "000000"
	if wrong == code {
		wrong = "111111"
	}
	for i := 0; i < 6; i++ {
		if err := service.VerifyRecoveryEmail(ctx, userID, wrong); err == nil {
			t.Fatal("a wrong code verified an address")
		}
	}

	// The attempts are spent, so even the right code is now refused: guessing
	// has to cost something or a six-digit code is no protection at all.
	if err := service.VerifyRecoveryEmail(ctx, userID, code); err == nil {
		t.Error("the right code still worked after the attempts ran out")
	}
}

func TestACodeWorksOnlyOnce(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	code, err := service.SetRecoveryEmail(ctx, userID, address())
	if err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}
	if err := service.VerifyRecoveryEmail(ctx, userID, code); err != nil {
		t.Fatalf("VerifyRecoveryEmail: %v", err)
	}
	if err := service.VerifyRecoveryEmail(ctx, userID, code); err == nil {
		t.Error("the same code verified twice")
	}
}

func TestRecoveryClearsTheTwoStepPasswordAndEverySession(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	email := address()

	code, err := service.SetRecoveryEmail(ctx, userID, email)
	if err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}
	if err := service.VerifyRecoveryEmail(ctx, userID, code); err != nil {
		t.Fatalf("VerifyRecoveryEmail: %v", err)
	}

	if err := service.SetTwoStepPassword(ctx, userID, "", "a strong password", "hint"); err != nil {
		t.Fatalf("SetTwoStepPassword: %v", err)
	}
	var versionBefore int
	if err := db.Pool.QueryRow(ctx,
		`SELECT token_version FROM users WHERE id = $1`, userID).Scan(&versionBefore); err != nil {
		t.Fatalf("read token version: %v", err)
	}

	recoveryCode, err := service.StartEmailRecovery(ctx, email)
	if err != nil {
		t.Fatalf("StartEmailRecovery: %v", err)
	}
	if recoveryCode == "" {
		t.Fatal("no recovery code was issued for a verified address")
	}
	if err := service.CompleteEmailRecovery(ctx, email, recoveryCode); err != nil {
		t.Fatalf("CompleteEmailRecovery: %v", err)
	}

	var enabled bool
	var versionAfter int
	if err := db.Pool.QueryRow(ctx,
		`SELECT two_step_enabled, token_version FROM users WHERE id = $1`,
		userID).Scan(&enabled, &versionAfter); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if enabled {
		t.Error("the two-step password survived the recovery it was meant to clear")
	}
	if versionAfter <= versionBefore {
		t.Error("the token version did not move; a reset prompted by an intruder would leave them signed in")
	}
}

func TestRecoveryDoesNotRevealWhetherAnAddressIsKnown(t *testing.T) {
	db := recoveryDB(t)
	service, sender := recoveryService(t, db)
	ctx := context.Background()

	before := sender.count()
	// Nobody has ever registered this address.
	code, err := service.StartEmailRecovery(ctx, address())
	if err != nil {
		t.Fatalf("StartEmailRecovery reported an error for an unknown address: %v", err)
	}
	if code != "" {
		t.Error("a code was issued for an address on no account")
	}
	if sender.count() != before {
		t.Error("mail was sent to an address on no account")
	}
}

func TestChangingTheAddressRevokesTheOldOne(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	first := address()

	code, err := service.SetRecoveryEmail(ctx, userID, first)
	if err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}
	if err := service.VerifyRecoveryEmail(ctx, userID, code); err != nil {
		t.Fatalf("VerifyRecoveryEmail: %v", err)
	}

	// Wait out the resend window rather than fighting it: the window is the
	// subject of its own test.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE email_challenges SET created_at = created_at - interval '1 hour' WHERE user_id = $1`,
		userID); err != nil {
		t.Fatalf("age the challenge: %v", err)
	}

	if _, err := service.SetRecoveryEmail(ctx, userID, address()); err != nil {
		t.Fatalf("SetRecoveryEmail (second): %v", err)
	}

	// The old address must no longer be able to recover anything: two live
	// ways in is exactly what changing the address is meant to prevent.
	if _, err := service.StartEmailRecovery(ctx, first); err != nil {
		t.Fatalf("StartEmailRecovery: %v", err)
	}
	if err := service.CompleteEmailRecovery(ctx, first, code); err == nil {
		t.Error("the replaced address could still recover the account")
	}
}

func TestResendingIsRefusedInsideTheWindow(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	if _, err := service.SetRecoveryEmail(ctx, userID, address()); err != nil {
		t.Fatalf("SetRecoveryEmail: %v", err)
	}
	if _, err := service.SetRecoveryEmail(ctx, userID, address()); err == nil {
		t.Error("a second code was issued immediately; that is a way to mail someone repeatedly")
	}
}

func TestARejectedAddressIsNotEnrolled(t *testing.T) {
	db := recoveryDB(t)
	service, sender := recoveryService(t, db)
	ctx := context.Background()

	userID := recoveryUser(t, db)
	before := sender.count()

	if _, err := service.SetRecoveryEmail(ctx, userID, "not an address"); err == nil {
		t.Error("a malformed address was accepted")
	}
	if sender.count() != before {
		t.Error("mail was sent to a malformed address")
	}
}

func TestWithoutAMailTransportTheFeatureRefusesRatherThanPretends(t *testing.T) {
	db := recoveryDB(t)
	service, _ := recoveryService(t, db)
	ctx := context.Background()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	logSender, err := auth.NewEmailSender(config.Email{Provider: "log"}, logger)
	if err != nil {
		t.Fatalf("NewEmailSender: %v", err)
	}
	if auth.Deliverable(logSender) {
		t.Fatal("the log transport claims it can reach an inbox")
	}
	service.SetEmailSender(logSender, false)

	userID := recoveryUser(t, db)
	if _, err := service.SetRecoveryEmail(ctx, userID, address()); err == nil {
		t.Error("an address was enrolled on a server that cannot send mail")
	}
}

func TestAFailedDeliveryDoesNotLeaveAnAddressLookingEnrolled(t *testing.T) {
	db := recoveryDB(t)
	service, sender := recoveryService(t, db)
	ctx := context.Background()

	sender.fail = errors.New("the relay is down")
	userID := recoveryUser(t, db)

	if _, err := service.SetRecoveryEmail(ctx, userID, address()); err == nil {
		t.Fatal("enrolment reported success while the mail bounced")
	}

	status, err := service.EmailStatus(ctx, userID)
	if err != nil {
		t.Fatalf("EmailStatus: %v", err)
	}
	if status.Verified {
		t.Error("an address whose code was never delivered is marked verified")
	}
}

func TestMaskingKeepsAnAddressOutOfTheLogs(t *testing.T) {
	cases := map[string]string{
		"someone@example.com": "s*****e@example.com",
		"ab@example.com":      "**@example.com",
		"not-an-address":      "***",
	}
	for input, want := range cases {
		if got := auth.MaskEmail(input); got != want {
			t.Errorf("MaskEmail(%q) = %q, want %q", input, got, want)
		}
	}
}
