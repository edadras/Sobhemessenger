package users_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/users"
)

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

func createUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte(uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// uniqueName produces a handle that satisfies the pattern and cannot collide
// with a parallel run: usernames are globally unique.
func uniqueName(t *testing.T, db *database.DB) string {
	t.Helper()
	name := "u" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM reserved_usernames WHERE username = $1`, name)
	})
	return name
}

func TestClaimingAUsernameMakesItResolvable(t *testing.T) {
	db := testDB(t)
	repo := users.NewRepository(db)
	svc := users.NewService(repo)
	ctx := context.Background()

	owner := createUser(t, db)
	name := uniqueName(t, db)

	if err := svc.ClaimUsername(ctx, owner, name); err != nil {
		t.Fatalf("ClaimUsername: %v", err)
	}

	profile, err := svc.ByUsername(ctx, "@"+strings.ToUpper(name), owner)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	if profile.UserID != owner {
		t.Fatalf("resolved %s, want %s", profile.UserID, owner)
	}
	if profile.Username == nil || !strings.EqualFold(*profile.Username, name) {
		t.Fatalf("profile carries username %v, want %s", profile.Username, name)
	}
}

func TestASecondUserCannotTakeALiveUsername(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()

	first := createUser(t, db)
	second := createUser(t, db)
	name := uniqueName(t, db)

	if err := svc.ClaimUsername(ctx, first, name); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	err := svc.ClaimUsername(ctx, second, name)
	if err == nil {
		t.Fatal("second claim succeeded; the username should have been taken")
	}
	var apiErr *httpx.Error
	if !errors.As(err, &apiErr) || apiErr.Code != httpx.CodeUsernameTaken {
		t.Fatalf("second claim failed with %v, want a %s error", err, httpx.CodeUsernameTaken)
	}
}

// A released username must not be free the instant it is released: someone who
// still has old links pointing at it would otherwise be impersonated.
func TestAReleasedUsernameStaysReservedAgainstOthers(t *testing.T) {
	db := testDB(t)
	repo := users.NewRepository(db)
	svc := users.NewService(repo)
	ctx := context.Background()

	owner := createUser(t, db)
	squatter := createUser(t, db)
	original := uniqueName(t, db)
	replacement := uniqueName(t, db)

	if err := svc.ClaimUsername(ctx, owner, original); err != nil {
		t.Fatalf("claim original: %v", err)
	}
	if err := svc.ClaimUsername(ctx, owner, replacement); err != nil {
		t.Fatalf("claim replacement: %v", err)
	}

	available, err := repo.UsernameAvailable(ctx, original, squatter)
	if err != nil {
		t.Fatalf("UsernameAvailable: %v", err)
	}
	if available {
		t.Fatal("the released username is available to a stranger straight away")
	}
	if err := svc.ClaimUsername(ctx, squatter, original); err == nil {
		t.Fatal("a stranger claimed the released username during the hold")
	}
}

// The hold protects the previous holder, so it must not lock them out of
// their own old name.
func TestThePreviousHolderCanTakeTheirOldUsernameBack(t *testing.T) {
	db := testDB(t)
	repo := users.NewRepository(db)
	svc := users.NewService(repo)
	ctx := context.Background()

	owner := createUser(t, db)
	original := uniqueName(t, db)
	replacement := uniqueName(t, db)

	if err := svc.ClaimUsername(ctx, owner, original); err != nil {
		t.Fatalf("claim original: %v", err)
	}
	if err := svc.ClaimUsername(ctx, owner, replacement); err != nil {
		t.Fatalf("claim replacement: %v", err)
	}

	available, err := repo.UsernameAvailable(ctx, original, owner)
	if err != nil {
		t.Fatalf("UsernameAvailable: %v", err)
	}
	if !available {
		t.Fatal("the previous holder is locked out of their own released username")
	}
	if err := svc.ClaimUsername(ctx, owner, original); err != nil {
		t.Fatalf("reclaim original: %v", err)
	}

	// Taking the name back must consume the reservation, otherwise the row
	// would keep blocking everyone once the user renames again.
	var held int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM reserved_usernames WHERE username = $1`, original).Scan(&held); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	if held != 0 {
		t.Fatalf("%d reservation rows survive the reclaim, want 0", held)
	}
}

func TestUsernameShapeAndReservedNamesAreRejected(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)

	for _, candidate := range []string{
		"ab",             // too short
		"1leading",       // must start with a letter
		"has-a-hyphen",   // hyphen is not in the alphabet
		"has space",      // nor is whitespace
		"Ünicode_lookal", // non-ASCII homoglyphs are excluded, not folded
		"admin",          // reserved: names the platform
		"support",        // reserved
	} {
		if err := svc.ClaimUsername(ctx, owner, candidate); err == nil {
			t.Errorf("ClaimUsername(%q) succeeded; it should have been rejected", candidate)
		}

		result, err := svc.CheckUsername(ctx, candidate, owner)
		if err != nil {
			t.Fatalf("CheckUsername(%q): %v", candidate, err)
		}
		if result.Available {
			t.Errorf("CheckUsername(%q) reports it available", candidate)
		}
		if result.Reason == "" {
			t.Errorf("CheckUsername(%q) gives no reason", candidate)
		}
	}
}

// CheckUsername is what a rename form calls as the user types; it must agree
// with what ClaimUsername will actually do.
func TestAvailabilityCheckAgreesWithTheClaim(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()

	owner := createUser(t, db)
	other := createUser(t, db)
	name := uniqueName(t, db)

	before, err := svc.CheckUsername(ctx, name, owner)
	if err != nil {
		t.Fatalf("CheckUsername before: %v", err)
	}
	if !before.Available {
		t.Fatal("a fresh username is reported unavailable")
	}

	if err := svc.ClaimUsername(ctx, owner, name); err != nil {
		t.Fatalf("ClaimUsername: %v", err)
	}

	// The holder still sees their own name as available, because re-submitting
	// an unchanged rename form must not be an error.
	mine, err := svc.CheckUsername(ctx, name, owner)
	if err != nil {
		t.Fatalf("CheckUsername as holder: %v", err)
	}
	if !mine.Available {
		t.Fatal("the holder is told their own username is taken")
	}

	theirs, err := svc.CheckUsername(ctx, name, other)
	if err != nil {
		t.Fatalf("CheckUsername as stranger: %v", err)
	}
	if theirs.Available {
		t.Fatal("a taken username is reported available to a stranger")
	}
	if theirs.Reason != "taken" {
		t.Fatalf("reason is %q, want \"taken\"", theirs.Reason)
	}
}

func TestProfileUpdateIsPartialAndValidated(t *testing.T) {
	db := testDB(t)
	repo := users.NewRepository(db)
	svc := users.NewService(repo)
	ctx := context.Background()
	owner := createUser(t, db)

	name := "نام آزمایشی"
	about := "یک بیوگرافی کوتاه"
	language := "fa"
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{
		DisplayName: &name, About: &about, Language: &language,
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	// A second update naming only one field must leave the others standing.
	newAbout := "بیوگرافی تازه"
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{About: &newAbout}); err != nil {
		t.Fatalf("UpdateProfile second: %v", err)
	}

	self, err := svc.Self(ctx, owner)
	if err != nil {
		t.Fatalf("Self: %v", err)
	}
	if self.DisplayName != name {
		t.Fatalf("display name is %q, want %q — the partial update overwrote it", self.DisplayName, name)
	}
	if self.About != newAbout {
		t.Fatalf("about is %q, want %q", self.About, newAbout)
	}

	blank := "   "
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{DisplayName: &blank}); err == nil {
		t.Error("a whitespace-only display name was accepted")
	}
	tooLong := strings.Repeat("ا", 300)
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{About: &tooLong}); err == nil {
		t.Error("a 300-character bio was accepted; the limit is 280")
	}
	klingon := "tlh"
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{Language: &klingon}); err == nil {
		t.Error("an unsupported language was accepted")
	}
}

// Lengths are counted in runes, not bytes: a Persian bio is two bytes per
// character, so a byte limit would cut it in half.
func TestBioLimitCountsCharactersNotBytes(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()
	owner := createUser(t, db)

	persian := strings.Repeat("ا", 280)
	if len(persian) <= 280 {
		t.Fatalf("test premise is wrong: %d bytes for 280 characters", len(persian))
	}
	if err := svc.UpdateProfile(ctx, owner, users.ProfileUpdate{About: &persian}); err != nil {
		t.Fatalf("a 280-character Persian bio was rejected: %v", err)
	}
}

func TestUnknownUserAndUnknownUsernameAreNotFound(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()
	viewer := createUser(t, db)

	if _, err := svc.Profile(ctx, uuid.New(), viewer); err == nil {
		t.Error("a random user id resolved to a profile")
	}
	if _, err := svc.ByUsername(ctx, "no_such_handle_at_all", viewer); err == nil {
		t.Error("an unclaimed username resolved to a profile")
	}
}

// The bio is public by design (§55 lists no privacy key for it), and the
// contact and block flags describe the viewer's relationship, not the subject's
// settings — so they must reflect the viewer.
func TestProfileReflectsTheViewersRelationship(t *testing.T) {
	db := testDB(t)
	svc := users.NewService(users.NewRepository(db))
	ctx := context.Background()

	subject := createUser(t, db)
	viewer := createUser(t, db)

	about := "قابل مشاهده برای همه"
	display := "کاربر"
	if err := svc.UpdateProfile(ctx, subject, users.ProfileUpdate{
		DisplayName: &display, About: &about,
	}); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	stranger, err := svc.Profile(ctx, subject, viewer)
	if err != nil {
		t.Fatalf("Profile as stranger: %v", err)
	}
	if stranger.About != about {
		t.Fatalf("about is %q for a non-contact, want %q", stranger.About, about)
	}
	if stranger.IsContact || stranger.IsBlocked {
		t.Fatal("a stranger is reported as a contact or blocked")
	}

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO contacts (owner_id, contact_id, first_name) VALUES ($1, $2, $3)`,
		viewer, subject, "دوست"); err != nil {
		t.Fatalf("insert contact: %v", err)
	}

	known, err := svc.Profile(ctx, subject, viewer)
	if err != nil {
		t.Fatalf("Profile as contact: %v", err)
	}
	if !known.IsContact {
		t.Fatal("a saved contact is not reported as one")
	}
}
