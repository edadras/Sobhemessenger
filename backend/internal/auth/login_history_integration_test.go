package auth_test

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/auth"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// The sign-in log (§56).
//
// It is read on the security screen, where the entry that matters most is the
// one that did *not* work: a code someone tried and got wrong is the first
// visible sign of an attempt on the account. A log of successes only would
// show a clean history while it was happening — which is what it did until the
// screen existed to read it.

// echoingService issues codes the test can read back, which is how it presents
// the right one and, for what is being checked here, the wrong one.
func echoingService(t *testing.T, db *database.DB) *auth.Service {
	t.Helper()

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

	// The resend cooldown lives in Redis, so a service without one panics on
	// the first request rather than issuing a code.
	redis := miniredis.RunT(t)
	cacheClient, err := cache.Connect(context.Background(),
		config.Redis{Addr: redis.Addr(), PoolSize: 4})
	if err != nil {
		t.Fatalf("connect to test redis: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })

	rules := ratelimit.NewRules(config.RateLimits{
		OTPPerPhonePerHour: 10000, OTPPerIPPerHour: 10000, LoginPerIPPerHour: 10000,
	})

	// SMS_PROVIDER=log is the transport a development deployment uses, and it
	// is what makes an echoed code meaningful: the code exists and was
	// "delivered", it simply came back in the response instead of a message.
	sms, err := auth.NewSMSSender(config.SMS{Provider: "log", EchoCodes: true},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("sms sender: %v", err)
	}

	metrics := observability.New("login-history-test")
	return auth.NewService(auth.NewRepository(db), tokens,
		ratelimit.New(cacheClient, metrics), rules, sms, cacheClient,
		config.Auth{
			OTPLength: 6, OTPTTL: 5 * time.Minute, OTPMaxAttempts: 5,
			MaxDevicesPerUser: 20,
		},
		config.SMS{EchoCodes: true}, metrics,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// digits gives each test its own number. A UUID's hex is not a phone number —
// the service normalises before anything else and refuses the letters.
func digits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteByte(byte('0' + rand.IntN(10)))
	}
	return b.String()
}

// existingAccount plants an account with a known number, so a wrong code has
// somebody to be recorded against without a successful sign-in first — which
// would consume the challenge and run into the resend cooldown.
func existingAccount(t *testing.T, db *database.DB) (uuid.UUID, string) {
	t.Helper()

	phone := "+98912" + digits(7)
	var id uuid.UUID
	err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte("login-history")).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, phone
}

func TestARefusedCodeIsRecordedAgainstTheAccount(t *testing.T) {
	db := recoveryDB(t)
	service := echoingService(t, db)
	ctx := context.Background()

	userID, phone := existingAccount(t, db)

	if _, err := service.RequestOTP(ctx, auth.RequestOTPInput{Phone: phone}); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := service.VerifyOTP(ctx, auth.VerifyOTPInput{
		Phone: phone, Code: "000000",
		DeviceName: "test", Platform: "android", AppVersion: "1.0.0",
	}); err == nil {
		t.Fatal("a wrong code was accepted")
	}

	history, err := service.LoginHistory(ctx, userID, 20)
	if err != nil {
		t.Fatalf("LoginHistory: %v", err)
	}

	var failure *auth.LoginRecord
	for i := range history {
		if !history[i].Succeeded {
			failure = &history[i]
			break
		}
	}
	if failure == nil {
		t.Fatalf("the refused code was not recorded; history was %+v", history)
	}
	if failure.Event == "" {
		t.Error("the entry has no event name to show")
	}
}

func TestASuccessfulSignInIsRecordedToo(t *testing.T) {
	db := recoveryDB(t)
	service := echoingService(t, db)
	ctx := context.Background()

	_, phone := existingAccount(t, db)

	issued, err := service.RequestOTP(ctx, auth.RequestOTPInput{Phone: phone})
	if err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if issued.DebugCode == "" {
		t.Fatal("the test needs echoed codes")
	}

	tokens, err := service.VerifyOTP(ctx, auth.VerifyOTPInput{
		Phone: phone, Code: issued.DebugCode,
		DeviceName: "test", Platform: "android", AppVersion: "1.0.0",
	})
	if err != nil {
		t.Fatalf("VerifyOTP: %v", err)
	}

	history, err := service.LoginHistory(ctx, tokens.UserID, 20)
	if err != nil {
		t.Fatalf("LoginHistory: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("signing in recorded nothing")
	}

	var succeeded bool
	for _, entry := range history {
		if entry.Succeeded {
			succeeded = true
		}
	}
	if !succeeded {
		t.Errorf("no successful entry in %+v", history)
	}
}

func TestARefusedCodeForAnUnknownNumberRecordsNothing(t *testing.T) {
	db := recoveryDB(t)
	service := echoingService(t, db)
	ctx := context.Background()

	// No account has ever existed for this number, so there is nobody to show
	// an entry to, and the row would need an owner it does not have. What is
	// being checked is that the attempt is refused normally rather than
	// failing on a foreign key.
	phone := "+98919" + digits(7)
	if _, err := service.RequestOTP(ctx, auth.RequestOTPInput{Phone: phone}); err != nil {
		t.Fatalf("RequestOTP: %v", err)
	}
	if _, err := service.VerifyOTP(ctx, auth.VerifyOTPInput{
		Phone: phone, Code: "000000",
		DeviceName: "test", Platform: "android", AppVersion: "1.0.0",
	}); err == nil {
		t.Fatal("a wrong code was accepted for an unknown number")
	}
}
