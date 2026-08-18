package worker

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
)

// A chat's self-destruct timer (§14).
//
// `auto_delete_seconds` is a promise about privacy: someone turns it on
// believing that what they say stops existing after that long. It was settable
// through the API and read into every chat context, and nothing ever acted on
// it — which is worse than not offering the setting at all.

func testDB(t *testing.T) *database.DB {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping integration test")
	}

	db, err := database.Connect(context.Background(), config.Postgres{
		DSN: dsn, MaxConns: 4, MinConns: 1,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db
}

// chatWithTimer builds a two-person chat whose messages expire after ttl
// seconds; ttl of zero leaves the timer off.
func chatWithTimer(t *testing.T, db *database.DB, ttl int) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	newUser := func() uuid.UUID {
		var id uuid.UUID
		if err := db.Pool.QueryRow(ctx,
			`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
			"+9891"+uuid.NewString()[:9], []byte(uuid.NewString())).Scan(&id); err != nil {
			t.Fatalf("create user: %v", err)
		}
		t.Cleanup(func() {
			_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
		})
		return id
	}

	alice, bob := newUser(), newUser()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id, member_count) VALUES ('group', $1, 2) RETURNING id`,
		alice).Scan(&chatID); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, role)
		VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, chatID, alice, bob); err != nil {
		t.Fatalf("add members: %v", err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO chat_settings (chat_id, auto_delete_seconds) VALUES ($1, $2)`,
		chatID, ttl); err != nil {
		t.Fatalf("create settings: %v", err)
	}
	return chatID, alice
}

// insertMessage writes a message aged by the given duration.
func insertMessage(t *testing.T, db *database.DB, chatID, sender uuid.UUID, seq int64, age time.Duration) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Pool.QueryRow(context.Background(), `
		INSERT INTO messages (chat_id, sender_id, client_message_id, seq, type, content, created_at)
		VALUES ($1, $2, $3, $4, 'text', 'something private', now() - $5::interval)
		RETURNING id`,
		chatID, sender, uuid.New(), seq, age.String()).Scan(&id); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	return id
}

func state(t *testing.T, db *database.DB, id uuid.UUID) (deleted bool, content string) {
	t.Helper()
	var deletedAt *time.Time
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT deleted_at, content FROM messages WHERE id = $1`, id).Scan(&deletedAt, &content); err != nil {
		t.Fatalf("read message: %v", err)
	}
	return deletedAt != nil, content
}

func TestAutoDeleteRemovesTheContentOfExpiredMessages(t *testing.T) {
	db := testDB(t)
	runner := &Runner{db: db}

	chatID, alice := chatWithTimer(t, db, 3600) // one hour
	expired := insertMessage(t, db, chatID, alice, 1, 2*time.Hour)
	fresh := insertMessage(t, db, chatID, alice, 2, 10*time.Minute)

	if _, err := runner.autoDeleteMessages(context.Background()); err != nil {
		t.Fatalf("autoDeleteMessages: %v", err)
	}

	deleted, content := state(t, db, expired)
	if !deleted {
		t.Error("a message past the chat's timer was not deleted")
	}
	// The promise was about the content, not the row.
	if content != "" {
		t.Errorf("the expired message still holds its text: %q", content)
	}

	deleted, content = state(t, db, fresh)
	if deleted {
		t.Error("a message inside the window was deleted early")
	}
	if content == "" {
		t.Error("a message inside the window lost its content")
	}
}

func TestAutoDeleteLeavesChatsWithoutATimerAlone(t *testing.T) {
	// Zero means off, and it is the default for every chat. A sweep that
	// treated it as "expire immediately" would empty the entire deployment.
	db := testDB(t)
	runner := &Runner{db: db}

	chatID, alice := chatWithTimer(t, db, 0)
	old := insertMessage(t, db, chatID, alice, 1, 400*24*time.Hour)

	if _, err := runner.autoDeleteMessages(context.Background()); err != nil {
		t.Fatalf("autoDeleteMessages: %v", err)
	}

	deleted, content := state(t, db, old)
	if deleted || content == "" {
		t.Error("a chat with no timer lost a message")
	}
}

func TestAutoDeleteKeepsTheRowSoSequencesStayContiguous(t *testing.T) {
	// A gap in `seq` would break every client's cursor arithmetic, so the row
	// survives as a tombstone exactly as an ordinary deletion does.
	db := testDB(t)
	runner := &Runner{db: db}

	chatID, alice := chatWithTimer(t, db, 60)
	insertMessage(t, db, chatID, alice, 1, time.Hour)
	insertMessage(t, db, chatID, alice, 2, time.Hour)

	if _, err := runner.autoDeleteMessages(context.Background()); err != nil {
		t.Fatalf("autoDeleteMessages: %v", err)
	}

	var rows int
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM messages WHERE chat_id = $1`, chatID).Scan(&rows); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rows != 2 {
		t.Errorf("%d rows remain, want 2 tombstones", rows)
	}
}

func TestAutoDeleteIsIdempotent(t *testing.T) {
	// The sweep runs every tick. A second pass must find nothing left to do
	// rather than churning the same rows for ever.
	db := testDB(t)
	runner := &Runner{db: db}

	chatID, alice := chatWithTimer(t, db, 60)
	insertMessage(t, db, chatID, alice, 1, time.Hour)

	first, err := runner.autoDeleteMessages(context.Background())
	if err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	second, err := runner.autoDeleteMessages(context.Background())
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if first != 1 || second != 0 {
		t.Errorf("swept %d then %d, want 1 then 0", first, second)
	}
}
