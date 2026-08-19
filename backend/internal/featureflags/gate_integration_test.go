package featureflags_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/featureflags"
)

// Feature flags (§32).
//
// The panel could toggle a flag and nothing on the server read one, so turning
// a feature off left it running. What matters is the one claim the whole
// mechanism rests on: a route behind a flag that is off must refuse.

func flagsFixture(t *testing.T) (*featureflags.Service, *database.DB) {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}
	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute,
	})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	redis := miniredis.RunT(t)
	cacheClient, err := cache.Connect(context.Background(),
		config.Redis{Addr: redis.Addr(), PoolSize: 4})
	if err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })

	return featureflags.NewService(db, cacheClient,
		slog.New(slog.NewTextHandler(io.Discard, nil))), db
}

func setFlag(t *testing.T, db *database.DB, key string, enabled bool) {
	t.Helper()
	_, err := db.Pool.Exec(context.Background(), `
		INSERT INTO feature_flags (key, enabled, rollout_percent, description)
		VALUES ($1, $2, 100, 'gate test')
		ON CONFLICT (key) DO UPDATE SET enabled = EXCLUDED.enabled,
		                                rollout_percent = 100`, key, enabled)
	if err != nil {
		t.Fatalf("set flag: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM feature_flags WHERE key = $1`, key)
	})
}

func throughGate(service *featureflags.Service, key string) *httptest.ResponseRecorder {
	reached := false
	handler := service.Gate(key)(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(http.StatusOK)
		}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/x", nil))
	if reached && recorder.Code != http.StatusOK {
		panic("the handler ran but the recorder disagrees")
	}
	return recorder
}

func TestAGateLetsThroughWhatIsOn(t *testing.T) {
	service, db := flagsFixture(t)
	key := "gate_test_" + uuid.NewString()[:8]
	setFlag(t, db, key, true)

	if got := throughGate(service, key).Code; got != http.StatusOK {
		t.Fatalf("an enabled feature was refused: %d", got)
	}
}

func TestAGateRefusesWhatIsOff(t *testing.T) {
	service, db := flagsFixture(t)
	key := "gate_test_" + uuid.NewString()[:8]
	setFlag(t, db, key, false)

	// This is the claim that was untrue for the whole of the project's life:
	// switching a feature off in the panel left it running.
	recorder := throughGate(service, key)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("a disabled feature was served: %d", recorder.Code)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "FEATURE_DISABLED") {
		t.Errorf("the refusal does not say why: %s", body)
	}
}

func TestAnUnknownFlagIsRefusedRatherThanAssumedOn(t *testing.T) {
	service, _ := flagsFixture(t)

	// A typo in a gate must not open the feature. Defaulting to "on" would
	// mean a misspelt key silently ungates whatever it guards.
	if got := throughGate(service, "no_such_flag_"+uuid.NewString()[:8]).Code; got != http.StatusForbidden {
		t.Fatalf("an unknown flag defaulted to enabled: %d", got)
	}
}
