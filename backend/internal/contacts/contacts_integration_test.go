package contacts_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/contacts"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/security"
)

// Like the other integration suites, these tests need a real PostgreSQL with
// the migrations applied and skip themselves when it is not configured.
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

var testPepper = []byte("integration-test-pepper")

// createUser registers an account whose phone_hash is derived exactly as
// registration derives it, so discovery can find it by digest.
func createUser(t *testing.T, db *database.DB, label string) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	// last_seen_at is set so the privacy tests distinguish "hidden by the rule"
	// from "never recorded" — both surface as a NULL column otherwise.
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash, last_seen_at)
		 VALUES ($1, $2, now()) RETURNING id`,
		phone, security.HashPhone(phone, testPepper)).Scan(&id)
	if err != nil {
		t.Fatalf("create user %s: %v", label, err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`, id, label); err != nil {
		t.Fatalf("create profile for %s: %v", label, err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id, phone
}

// newService builds the service without a rate limiter, which needs Redis. The
// limiter is exercised in its own package; here the interest is the matching
// and privacy behaviour.
func newService(db *database.DB) *contacts.Service {
	return contacts.NewService(contacts.NewRepository(db), nil,
		ratelimit.NewRules(config.RateLimits{}), config.Auth{PhoneHashPepper: testPepper})
}

func TestSyncMatchesOnlyHashedNumbers(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	_, bobPhone := createUser(t, db, "bob")

	digest := contacts.DigestFor(bobPhone, testPepper)
	matches, err := repo.MatchByHash(ctx, [][]byte{security.HashPhone(bobPhone, testPepper)}, alice)
	if err != nil {
		t.Fatalf("MatchByHash: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("got %d matches, want 1", len(matches))
	}
	if matches[0].Digest != digest {
		t.Errorf("match digest = %q, want %q", matches[0].Digest, digest)
	}
	if matches[0].DisplayName != "bob" {
		t.Errorf("match display name = %q, want %q", matches[0].DisplayName, "bob")
	}
}

// An unregistered number must produce nothing at all — not an empty
// placeholder the caller could use to confirm the number was tried.
func TestSyncIgnoresUnknownDigests(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")

	unknown := security.HashPhone("+989000000000", testPepper)
	matches, err := repo.MatchByHash(ctx, [][]byte{unknown}, alice)
	if err != nil {
		t.Fatalf("MatchByHash: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("got %d matches for an unregistered number, want 0", len(matches))
	}
}

// The caller must never discover themselves, or every sync would add a
// self-contact.
func TestSyncExcludesTheCaller(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, alicePhone := createUser(t, db, "alice")

	matches, err := repo.MatchByHash(ctx,
		[][]byte{security.HashPhone(alicePhone, testPepper)}, alice)
	if err != nil {
		t.Fatalf("MatchByHash: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("caller discovered themselves: %v", matches)
	}
}

func TestBlockedUsersAreNotDiscoverable(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, bobPhone := createUser(t, db, "bob")

	if err := repo.Block(ctx, bob, alice, "no thanks"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	matches, err := repo.MatchByHash(ctx,
		[][]byte{security.HashPhone(bobPhone, testPepper)}, alice)
	if err != nil {
		t.Fatalf("MatchByHash: %v", err)
	}
	if len(matches) != 0 {
		t.Errorf("a user who blocked the caller was still discoverable: %v", matches)
	}
}

func TestServiceSyncStoresMatchedContacts(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, bobPhone := createUser(t, db, "bob")

	result, err := service.Sync(ctx, contacts.SyncInput{
		UserID: alice,
		Entries: []contacts.SyncEntry{
			{Digest: contacts.DigestFor(bobPhone, testPepper), FirstName: "Bob", LastName: "B"},
			{Digest: contacts.DigestFor("+989000000001", testPepper), FirstName: "Nobody"},
		},
		Replace: true,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(result.Matched) != 1 || result.Matched[0].UserID != bob {
		t.Fatalf("matched = %v, want just bob (%s)", result.Matched, bob)
	}
	if result.Uploaded != 2 {
		t.Errorf("uploaded = %d, want 2", result.Uploaded)
	}

	list, err := service.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("alice has %d contacts, want 1", len(list))
	}
	if list[0].FirstName != "Bob" {
		t.Errorf("stored first name = %q, want %q", list[0].FirstName, "Bob")
	}
	if list[0].IsMutual {
		t.Error("contact is reported as mutual before bob added alice back")
	}
}

// A full sync is a replacement: a number deleted on the device disappears here.
func TestReplaceSyncPrunesRemovedContacts(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	_, bobPhone := createUser(t, db, "bob")
	_, carolPhone := createUser(t, db, "carol")

	if _, err := service.Sync(ctx, contacts.SyncInput{
		UserID: alice,
		Entries: []contacts.SyncEntry{
			{Digest: contacts.DigestFor(bobPhone, testPepper), FirstName: "Bob"},
			{Digest: contacts.DigestFor(carolPhone, testPepper), FirstName: "Carol"},
		},
		Replace: true,
	}); err != nil {
		t.Fatalf("first Sync: %v", err)
	}

	if _, err := service.Sync(ctx, contacts.SyncInput{
		UserID:  alice,
		Entries: []contacts.SyncEntry{{Digest: contacts.DigestFor(bobPhone, testPepper), FirstName: "Bob"}},
		Replace: true,
	}); err != nil {
		t.Fatalf("second Sync: %v", err)
	}

	list, err := service.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].FirstName != "Bob" {
		t.Errorf("after replacement alice has %d contacts %v, want only Bob", len(list), list)
	}
}

// A partial sync adds without pruning, so a client uploading its address book
// in batches does not erase the batch it uploaded a moment ago.
func TestPartialSyncDoesNotPrune(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	_, bobPhone := createUser(t, db, "bob")
	_, carolPhone := createUser(t, db, "carol")

	for _, phone := range []string{bobPhone, carolPhone} {
		if _, err := service.Sync(ctx, contacts.SyncInput{
			UserID:  alice,
			Entries: []contacts.SyncEntry{{Digest: contacts.DigestFor(phone, testPepper)}},
			Replace: false,
		}); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}

	list, err := service.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("alice has %d contacts after two partial batches, want 2", len(list))
	}
}

func TestDuplicateDigestsAreCountedOnce(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	_, bobPhone := createUser(t, db, "bob")
	digest := contacts.DigestFor(bobPhone, testPepper)

	result, err := service.Sync(ctx, contacts.SyncInput{
		UserID: alice,
		Entries: []contacts.SyncEntry{
			{Digest: digest, FirstName: "Bob"},
			{Digest: digest, FirstName: "Bob (work)"},
		},
		Replace: true,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if result.Uploaded != 1 {
		t.Errorf("uploaded = %d for a repeated digest, want 1", result.Uploaded)
	}
	if len(result.Matched) != 1 {
		t.Errorf("matched %d times for one number, want 1", len(result.Matched))
	}
}

func TestMalformedDigestFailsTheBatch(t *testing.T) {
	db := testDB(t)
	service := newService(db)

	alice, _ := createUser(t, db, "alice")

	_, err := service.Sync(context.Background(), contacts.SyncInput{
		UserID:  alice,
		Entries: []contacts.SyncEntry{{Digest: "not-hex"}},
	})
	if err == nil {
		t.Fatal("a malformed digest was accepted")
	}
}

// Blocking is symmetric housekeeping: it must clear the contact entry on both
// sides, not just the blocker's.
func TestBlockRemovesContactsBothWays(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")

	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add(alice→bob): %v", err)
	}
	if err := repo.Add(ctx, bob, alice, "Alice", ""); err != nil {
		t.Fatalf("Add(bob→alice): %v", err)
	}

	if err := repo.Block(ctx, alice, bob, "spam"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	for _, owner := range []struct {
		id    uuid.UUID
		label string
	}{{alice, "alice"}, {bob, "bob"}} {
		list, err := repo.List(ctx, owner.id)
		if err != nil {
			t.Fatalf("List(%s): %v", owner.label, err)
		}
		if len(list) != 0 {
			t.Errorf("%s still has %d contacts after the block", owner.label, len(list))
		}
	}

	blocked, err := repo.IsBlocked(ctx, bob, alice)
	if err != nil {
		t.Fatalf("IsBlocked: %v", err)
	}
	if !blocked {
		t.Error("IsBlocked is false in the reverse direction; blocking must be symmetric for delivery")
	}
}

func TestMutualContactsAreFlagged(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")

	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	list, err := repo.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].IsMutual {
		t.Fatalf("one-sided contact reported as mutual: %v", list)
	}

	if err := repo.Add(ctx, bob, alice, "Alice", ""); err != nil {
		t.Fatalf("Add (back): %v", err)
	}
	list, err = repo.List(ctx, alice)
	if err != nil {
		t.Fatalf("List (after mutual): %v", err)
	}
	if len(list) != 1 || !list[0].IsMutual {
		t.Errorf("mutual contact not flagged: %v", list)
	}
}

// §55: last_seen defaults to "contacts", so a stranger sees nothing and a
// contact does.
func TestLastSeenFollowsPrivacyRules(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")

	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Bob has not added Alice, so under the default rule she is not his contact.
	list, err := repo.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("alice has %d contacts, want 1", len(list))
	}
	if list[0].LastSeen != nil {
		t.Error("last seen was exposed to a non-contact under the default rule")
	}

	if err := repo.Add(ctx, bob, alice, "Alice", ""); err != nil {
		t.Fatalf("Add (back): %v", err)
	}
	list, err = repo.List(ctx, alice)
	if err != nil {
		t.Fatalf("List (mutual): %v", err)
	}
	if list[0].LastSeen == nil {
		t.Error("last seen was hidden from a contact under the default rule")
	}
}

func TestExplicitDenyBeatsAnAllowRule(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")

	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Bob opens last_seen to everyone but denies Alice specifically.
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO user_privacy_settings (user_id, key, rule, deny_list)
		VALUES ($1, 'last_seen', 'everyone', ARRAY[$2::uuid])
		ON CONFLICT (user_id, key) DO UPDATE
		SET rule = EXCLUDED.rule, deny_list = EXCLUDED.deny_list`, bob, alice); err != nil {
		t.Fatalf("set privacy: %v", err)
	}

	list, err := repo.List(ctx, alice)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if list[0].LastSeen != nil {
		t.Error("an explicit deny did not override the 'everyone' rule")
	}
}
