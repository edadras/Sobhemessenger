package antispam_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/antispam"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
)

// Anti-spam scoring (§34).
//
// The three properties worth pinning down are that evidence accumulates and
// is weighted, that a score decays with time rather than ratcheting, and that
// a restriction lapses by itself. Everything else is a consequence of those.

func testDB(t *testing.T) *database.DB {
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

// subject uses a fresh key per test so the rows never collide, and cleans up
// after itself: the table is keyed by subject rather than owned by a user row,
// so nothing else would remove it.
func subject(t *testing.T, db *database.DB) string {
	t.Helper()
	key := uuid.NewString()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM spam_scores WHERE subject_key = $1`, key)
	})
	return key
}

func TestSignalsAccumulateAndAreWeighted(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()

	reported := subject(t, db)
	blocked := subject(t, db)

	if _, err := repo.Record(ctx, antispam.SubjectUser, reported,
		antispam.WeightReport, "reported: spam"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := repo.Record(ctx, antispam.SubjectUser, blocked,
		antispam.WeightBlock, "blocked"); err != nil {
		t.Fatalf("Record: %v", err)
	}

	reportedScore, err := repo.Get(ctx, antispam.SubjectUser, reported)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	blockedScore, err := repo.Get(ctx, antispam.SubjectUser, blocked)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reportedScore.Score <= blockedScore.Score {
		t.Errorf("a report scored %d and a block %d; a deliberate report must weigh more",
			reportedScore.Score, blockedScore.Score)
	}

	// A second signal adds rather than replaces.
	if _, err := repo.Record(ctx, antispam.SubjectUser, blocked,
		antispam.WeightBlock, "blocked"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	again, err := repo.Get(ctx, antispam.SubjectUser, blocked)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if again.Score != 2*antispam.WeightBlock {
		t.Errorf("score after two blocks = %d, want %d", again.Score, 2*antispam.WeightBlock)
	}
}

func TestCrossingTheThresholdRestrictsTheSubject(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	// One short of the threshold changes nothing.
	just, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.RestrictThreshold-1, "almost")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if just.Restricted() {
		t.Errorf("score %d restricted below the threshold of %d",
			just.Score, antispam.RestrictThreshold)
	}

	crossed, err := repo.Record(ctx, antispam.SubjectUser, key, 1, "and over")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !crossed.Restricted() {
		t.Fatalf("score %d did not restrict at the threshold of %d",
			crossed.Score, antispam.RestrictThreshold)
	}
	if crossed.RestrictedUntil.Before(time.Now()) {
		t.Error("the restriction was set in the past")
	}
}

func TestMoreBadBehaviourNeverBuysAnEarlierRelease(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	first, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.RestrictThreshold, "restricted")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Push the existing restriction well into the future, then record another
	// signal. A naive "restrict for 24 hours from now" would shorten it.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE spam_scores SET restricted_until = now() + interval '7 days'
		 WHERE subject_type = 'user' AND subject_key = $1`, key); err != nil {
		t.Fatalf("extend restriction: %v", err)
	}

	later, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.WeightReport, "another report")
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if later.RestrictedUntil.Before(time.Now().Add(6 * 24 * time.Hour)) {
		t.Errorf("a new signal shortened the restriction to %v (it was set to a week out); "+
			"the later of the two must win", later.RestrictedUntil)
	}
	_ = first
}

func TestScoreDecaysWithTime(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	if _, err := repo.Record(ctx, antispam.SubjectUser, key, 60, "a bad day"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// Two days ago, so two days' worth should come off.
	if _, err := db.Pool.Exec(ctx,
		`UPDATE spam_scores SET updated_at = now() - interval '2 days'
		 WHERE subject_type = 'user' AND subject_key = $1`, key); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	if _, err := repo.Decay(ctx, antispam.DecayPerDay); err != nil {
		t.Fatalf("Decay: %v", err)
	}

	decayed, err := repo.Get(ctx, antispam.SubjectUser, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if decayed == nil {
		t.Fatal("the row was dropped; 60 minus two days' decay is not zero")
	}
	want := 60 - 2*antispam.DecayPerDay
	if decayed.Score != want {
		t.Errorf("score after two days = %d, want %d", decayed.Score, want)
	}
}

func TestAScoreThatReachesZeroIsForgotten(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	if _, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.WeightRateLimit, "one trip"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE spam_scores SET updated_at = now() - interval '30 days'
		 WHERE subject_type = 'user' AND subject_key = $1`, key); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	if _, err := repo.Decay(ctx, antispam.DecayPerDay); err != nil {
		t.Fatalf("Decay: %v", err)
	}

	score, err := repo.Get(ctx, antispam.SubjectUser, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if score != nil {
		t.Errorf("a month-old score of %d survived; an account that behaved should be forgotten",
			score.Score)
	}
}

func TestDecayLeavesALiveRestrictionInPlace(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	if _, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.RestrictThreshold, "restricted"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	// The score is old enough to decay to nothing, but the restriction is
	// still running. Dropping the row would release the subject early.
	if _, err := db.Pool.Exec(ctx, `
		UPDATE spam_scores
		SET updated_at = now() - interval '30 days', restricted_until = now() + interval '1 hour'
		WHERE subject_type = 'user' AND subject_key = $1`, key); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	if _, err := repo.Decay(ctx, antispam.DecayPerDay); err != nil {
		t.Fatalf("Decay: %v", err)
	}

	score, err := repo.Get(ctx, antispam.SubjectUser, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if score == nil {
		t.Fatal("decay dropped a row whose restriction is still in force")
	}
	if !score.Restricted() {
		t.Error("the restriction was lifted by the decay sweep")
	}
}

func TestALapsedRestrictionNoLongerBites(t *testing.T) {
	db := testDB(t)
	repo := antispam.NewRepository(db)
	ctx := context.Background()
	key := subject(t, db)

	if _, err := repo.Record(ctx, antispam.SubjectUser, key,
		antispam.RestrictThreshold, "restricted"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`UPDATE spam_scores SET restricted_until = now() - interval '1 minute'
		 WHERE subject_type = 'user' AND subject_key = $1`, key); err != nil {
		t.Fatalf("expire the restriction: %v", err)
	}

	score, err := repo.Get(ctx, antispam.SubjectUser, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if score.Restricted() {
		t.Error("a restriction that has run out is still being enforced")
	}
}

func TestLiftingClearsTheScoreAndTheRestriction(t *testing.T) {
	db := testDB(t)
	service := antispam.NewService(antispam.NewRepository(db))
	ctx := context.Background()

	userID := uuid.New()
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM spam_scores WHERE subject_key = $1`, userID.String())
	})

	for i := 0; i < 4; i++ {
		service.Reported(ctx, userID, "spam")
	}
	if restricted, _ := service.Restricted(ctx, userID); !restricted {
		t.Fatal("four reports did not restrict the account")
	}

	if err := service.Lift(ctx, userID); err != nil {
		t.Fatalf("Lift: %v", err)
	}
	if restricted, _ := service.Restricted(ctx, userID); restricted {
		t.Error("the account is still restricted after a moderator lifted it")
	}
	score, err := service.Score(ctx, userID)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	if score != nil {
		t.Errorf("the score survived being lifted: %+v", score)
	}
}

func TestANilServiceAllowsEverything(t *testing.T) {
	// A deployment that has not wired the module in must behave exactly as it
	// did before rather than refusing traffic.
	var service *antispam.Service
	if restricted, _ := service.Restricted(context.Background(), uuid.New()); restricted {
		t.Error("an unwired anti-spam service restricted an account")
	}
	service.Reported(context.Background(), uuid.New(), "spam")
}
