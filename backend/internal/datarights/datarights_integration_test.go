package datarights_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/datarights"
)

// Data export and account deletion (§56).
//
// `data_requests` has been in the schema since migration 0008 and unwritten
// since. The properties worth pinning down are that a deletion can be taken
// back inside its window, that an export contains the person's own data and
// nobody else's, and that a deleted account keeps nothing that identifies it
// while leaving other people's conversations intact.

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

func createUser(t *testing.T, db *database.DB, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var id uuid.UUID
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		"+9891"+uuid.NewString()[:9], []byte(label)).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`, id, label); err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_event_counters (user_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("create event counter: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// memoryStore stands in for object storage. It is a real destination — the
// test reads back exactly what the export wrote — rather than a stub that
// records the call and discards the bytes.
type memoryStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newStore() *memoryStore {
	return &memoryStore{objects: map[string][]byte{}}
}

func (s *memoryStore) ExportBucket() string { return "sobh-exports" }

func (s *memoryStore) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ string) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[bucket+"/"+key] = body
	return nil
}

func (s *memoryStore) only(t *testing.T) map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.objects) != 1 {
		t.Fatalf("the export wrote %d objects, want 1", len(s.objects))
	}
	for _, body := range s.objects {
		var document map[string]any
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&document); err != nil {
			t.Fatalf("the export is not readable JSON: %v", err)
		}
		return document
	}
	return nil
}

func TestADeletionWaitsAndCanBeTakenBack(t *testing.T) {
	db := testDB(t)
	service := datarights.NewService(datarights.NewRepository(db))
	repo := datarights.NewRepository(db)
	ctx := context.Background()

	userID := createUser(t, db, "leaver")

	request, err := service.Request(ctx, userID, datarights.TypeDelete)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if !request.ExecuteAfter.After(time.Now().Add(24 * time.Hour)) {
		t.Errorf("the deletion runs at %v; the window is what makes it reversible",
			request.ExecuteAfter)
	}

	// Nothing is due yet, so the worker must not pick it up.
	due, err := repo.ClaimDue(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	for _, claimed := range due {
		if claimed.ID == request.ID {
			t.Fatal("a deletion was carried out during its cancellation window")
		}
	}

	if err := service.Cancel(ctx, userID, request.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	var status string
	if err := db.Pool.QueryRow(ctx,
		`SELECT status FROM data_requests WHERE id = $1`, request.ID).Scan(&status); err != nil {
		t.Fatalf("read request: %v", err)
	}
	if status != datarights.StatusCancelled {
		t.Errorf("status = %q after cancelling", status)
	}

	// And the account is untouched, which is the whole point of cancelling.
	var accountStatus string
	if err := db.Pool.QueryRow(ctx,
		`SELECT status FROM users WHERE id = $1`, userID).Scan(&accountStatus); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if accountStatus != "active" {
		t.Errorf("account status = %q after a cancelled deletion", accountStatus)
	}
}

func TestOnlyTheOwnerCanCancelTheirRequest(t *testing.T) {
	db := testDB(t)
	service := datarights.NewService(datarights.NewRepository(db))
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	stranger := createUser(t, db, "stranger")

	request, err := service.Request(ctx, owner, datarights.TypeDelete)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := service.Cancel(ctx, stranger, request.ID); err == nil {
		t.Error("somebody else cancelled the request")
	}
	if err := service.Cancel(ctx, owner, request.ID); err != nil {
		t.Errorf("the owner could not cancel their own request: %v", err)
	}
}

func TestASecondRequestOfTheSameKindIsRefused(t *testing.T) {
	db := testDB(t)
	service := datarights.NewService(datarights.NewRepository(db))
	ctx := context.Background()

	userID := createUser(t, db, "asker")

	if _, err := service.Request(ctx, userID, datarights.TypeExport); err != nil {
		t.Fatalf("Request: %v", err)
	}
	if _, err := service.Request(ctx, userID, datarights.TypeExport); err == nil {
		t.Error("two exports of the same data were queued at once")
	}
	// A deletion is a different kind, so it is not blocked by an open export.
	if _, err := service.Request(ctx, userID, datarights.TypeDelete); err != nil {
		t.Errorf("a deletion was refused because an export was open: %v", err)
	}
}

func TestAnExportIsDueImmediately(t *testing.T) {
	db := testDB(t)
	service := datarights.NewService(datarights.NewRepository(db))
	repo := datarights.NewRepository(db)
	ctx := context.Background()

	userID := createUser(t, db, "asker")
	request, err := service.Request(ctx, userID, datarights.TypeExport)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// There is nothing to reconsider about a copy of your own data, so it does
	// not wait.
	due, err := repo.ClaimDue(ctx, 50)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	found := false
	for _, claimed := range due {
		if claimed.ID == request.ID {
			found = true
		}
	}
	if !found {
		t.Error("an export was not claimed on the first pass")
	}
}

func TestTheExportContainsTheirDataAndNobodyElses(t *testing.T) {
	db := testDB(t)
	repo := datarights.NewRepository(db)
	store := newStore()
	ctx := context.Background()

	author := createUser(t, db, "author")
	other := createUser(t, db, "other")

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO chats (type, title, creator_id, member_count)
		VALUES ('group', 'shared', $1, 2) RETURNING id`, author).Scan(&chatID); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})
	for _, member := range []uuid.UUID{author, other} {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	for i, pair := range []struct {
		sender  uuid.UUID
		content string
	}{{author, "mine to export"}, {other, "not mine to take"}} {
		if _, err := db.Pool.Exec(ctx, `
			INSERT INTO messages (chat_id, seq, sender_id, client_message_id, type, content)
			VALUES ($1, $2, $3, gen_random_uuid(), 'text', $4)`,
			chatID, i+1, pair.sender, pair.content); err != nil {
			t.Fatalf("insert message: %v", err)
		}
	}

	if _, err := repo.Export(ctx, store, author); err != nil {
		t.Fatalf("Export: %v", err)
	}

	document := store.only(t)
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	text := string(body)
	if !bytes.Contains([]byte(text), []byte("mine to export")) {
		t.Error("the export is missing the person's own message")
	}
	// The other side of a conversation is not theirs to take with them.
	if bytes.Contains([]byte(text), []byte("not mine to take")) {
		t.Error("the export contains somebody else's message")
	}
	if document["profile"] == nil {
		t.Error("the export has no profile section")
	}
	if document["chats"] == nil {
		t.Error("the export has no chats section")
	}
}

func TestTheExportIsRecordedAsMediaTheOwnerCanFetch(t *testing.T) {
	db := testDB(t)
	repo := datarights.NewRepository(db)
	store := newStore()
	ctx := context.Background()

	userID := createUser(t, db, "asker")
	mediaID, err := repo.Export(ctx, store, userID)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	var owner uuid.UUID
	var bucket, kind, scan string
	if err := db.Pool.QueryRow(ctx,
		`SELECT owner_id, bucket, kind, scan_status FROM media WHERE id = $1`,
		mediaID).Scan(&owner, &bucket, &kind, &scan); err != nil {
		t.Fatalf("read media: %v", err)
	}
	if owner != userID {
		t.Errorf("the archive belongs to %s, want the person who asked for it", owner)
	}
	if bucket != store.ExportBucket() {
		t.Errorf("the archive is in %q, want the export bucket", bucket)
	}
	// The server wrote it, so there is nothing to scan for a virus.
	if scan != "skipped" {
		t.Errorf("scan_status = %q for a file the server produced", scan)
	}
}

func TestDeletionEmptiesTheAccountAndFreesTheNumber(t *testing.T) {
	db := testDB(t)
	repo := datarights.NewRepository(db)
	ctx := context.Background()

	userID := createUser(t, db, "leaver")
	friend := createUser(t, db, "friend")

	var phone string
	if err := db.Pool.QueryRow(ctx,
		`SELECT phone_number FROM users WHERE id = $1`, userID).Scan(&phone); err != nil {
		t.Fatalf("read phone: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO contacts (owner_id, contact_id) VALUES ($1, $2), ($2, $1)`,
		userID, friend); err != nil {
		t.Fatalf("add contacts: %v", err)
	}

	if err := repo.Delete(ctx, userID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var status, storedPhone, displayName string
	var username, email *string
	var deletedAt *time.Time
	if err := db.Pool.QueryRow(ctx, `
		SELECT u.status, u.phone_number, u.username, u.email, u.deleted_at,
		       COALESCE(p.display_name, '')
		FROM users u LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.id = $1`, userID,
	).Scan(&status, &storedPhone, &username, &email, &deletedAt, &displayName); err != nil {
		t.Fatalf("read user: %v", err)
	}

	if status != "deleted" || deletedAt == nil {
		t.Errorf("status = %q, deleted_at = %v", status, deletedAt)
	}
	if displayName != "" || username != nil || email != nil {
		t.Error("something identifying survived the deletion")
	}
	// The number is UNIQUE, so keeping it would stop anyone — including its
	// owner — ever registering it again.
	if storedPhone == phone {
		t.Error("the phone number is still held by the deleted account")
	}

	var contactsLeft int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM contacts WHERE owner_id = $1 OR contact_id = $1`,
		userID).Scan(&contactsLeft); err != nil {
		t.Fatalf("count contacts: %v", err)
	}
	if contactsLeft != 0 {
		t.Errorf("%d address-book entries survived", contactsLeft)
	}
}

func TestDeletionLeavesOtherPeoplesConversationsIntact(t *testing.T) {
	db := testDB(t)
	repo := datarights.NewRepository(db)
	ctx := context.Background()

	leaver := createUser(t, db, "leaver")
	stayer := createUser(t, db, "stayer")

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO chats (type, title, creator_id, member_count)
		VALUES ('group', 'shared', $1, 2) RETURNING id`, stayer).Scan(&chatID); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})
	for _, member := range []uuid.UUID{leaver, stayer} {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO messages (chat_id, seq, sender_id, client_message_id, type, content)
		VALUES ($1, 1, $2, gen_random_uuid(), 'text', 'still here')`,
		chatID, leaver); err != nil {
		t.Fatalf("insert message: %v", err)
	}

	if err := repo.Delete(ctx, leaver); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Removing the account row would set sender_id to NULL on every message
	// and turn their side of the conversation into anonymous text in somebody
	// else's chat. Emptying it instead keeps the thread readable.
	var content string
	var sender *uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`SELECT content, sender_id FROM messages WHERE chat_id = $1 AND seq = 1`,
		chatID).Scan(&content, &sender); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if content != "still here" {
		t.Errorf("the message reads %q after the sender deleted their account", content)
	}
	if sender == nil || *sender != leaver {
		t.Error("the message lost its author")
	}
}

func TestAnUnknownRequestTypeIsRefused(t *testing.T) {
	db := testDB(t)
	service := datarights.NewService(datarights.NewRepository(db))

	userID := createUser(t, db, "asker")
	if _, err := service.Request(context.Background(), userID, "everything"); err == nil {
		t.Error("an invented request type was accepted")
	}
}
