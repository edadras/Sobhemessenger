package search

import (
	"context"
	"io"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Which chats a search may reach (§26, §61).
//
// This is the boundary that keeps message search from returning a conversation
// the searcher is not part of, and from ever touching an encrypted one. It is
// tested against real SQL rather than a mock because the boundary *is* the
// query: a mock would only assert that the test agrees with itself.

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

// scopeService builds a service with only what the scope query needs. The
// OpenSearch client and rate limiter are nil deliberately: memberChatIDs must
// not reach either, and a nil panic here would say it had started to.
func scopeService(t *testing.T, db *database.DB) *Service {
	t.Helper()
	return NewService(nil, db, nil, ratelimit.Rules{},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func createUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	if err := db.Pool.QueryRow(context.Background(),
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		"+9891"+uuid.NewString()[:9], []byte(uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// chatWith creates a chat of the given type and puts every named user in it.
func chatWith(t *testing.T, db *database.DB, chatType string, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id, member_count, last_message_at)
		 VALUES ($1, $2, $3, now()) RETURNING id`,
		chatType, members[0], len(members)).Scan(&chatID); err != nil {
		t.Fatalf("create %s chat: %v", chatType, err)
	}
	for _, member := range members {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add chat member: %v", err)
		}
	}
	return chatID
}

func TestSearchScopeIsLimitedToTheCallersChats(t *testing.T) {
	db := testDB(t)
	service := scopeService(t, db)
	ctx := context.Background()

	alice := createUser(t, db)
	bob := createUser(t, db)
	carol := createUser(t, db)

	mine := chatWith(t, db, "group", alice, bob)
	theirs := chatWith(t, db, "group", bob, carol)

	ids, err := service.memberChatIDs(ctx, alice)
	if err != nil {
		t.Fatalf("memberChatIDs: %v", err)
	}

	if !slices.Contains(ids, mine.String()) {
		t.Error("a chat the caller is in was not searchable")
	}
	// The one that matters: a chat Alice is not in must never enter the scope,
	// because the scope is the only thing standing between her query and
	// other people's conversations.
	if slices.Contains(ids, theirs.String()) {
		t.Error("a chat the caller is not in was searchable")
	}
}

func TestEncryptedChatsAreNeverSearchable(t *testing.T) {
	// Their bodies are never indexed, because the server has never seen them.
	// Excluding them from the scope as well means a stale index left over from
	// an earlier build could not surface one either.
	db := testDB(t)
	service := scopeService(t, db)
	ctx := context.Background()

	alice := createUser(t, db)
	bob := createUser(t, db)

	secret := chatWith(t, db, "secret", alice, bob)
	ordinary := chatWith(t, db, "private", alice, bob)

	ids, err := service.memberChatIDs(ctx, alice)
	if err != nil {
		t.Fatalf("memberChatIDs: %v", err)
	}

	if slices.Contains(ids, secret.String()) {
		t.Error("an encrypted chat was searchable")
	}
	if !slices.Contains(ids, ordinary.String()) {
		t.Error("an ordinary private chat was not searchable")
	}
}

func TestLeavingAChatRemovesItFromSearch(t *testing.T) {
	// Membership is read at query time rather than baked into the index, so
	// this takes effect immediately instead of waiting for a reindex. Someone
	// removed from a group must stop being able to search it.
	db := testDB(t)
	service := scopeService(t, db)
	ctx := context.Background()

	alice := createUser(t, db)
	bob := createUser(t, db)
	chatID := chatWith(t, db, "group", alice, bob)

	if _, err := db.Pool.Exec(ctx,
		`UPDATE chat_members SET left_at = now() WHERE chat_id = $1 AND user_id = $2`,
		chatID, alice); err != nil {
		t.Fatalf("mark alice as having left: %v", err)
	}

	ids, err := service.memberChatIDs(ctx, alice)
	if err != nil {
		t.Fatalf("memberChatIDs: %v", err)
	}
	if slices.Contains(ids, chatID.String()) {
		t.Error("a chat the caller has left was still searchable")
	}
}

func TestDeletedChatsAreNotSearchable(t *testing.T) {
	db := testDB(t)
	service := scopeService(t, db)
	ctx := context.Background()

	alice := createUser(t, db)
	bob := createUser(t, db)
	chatID := chatWith(t, db, "group", alice, bob)

	if _, err := db.Pool.Exec(ctx,
		`UPDATE chats SET deleted_at = now() WHERE id = $1`, chatID); err != nil {
		t.Fatalf("delete the chat: %v", err)
	}

	ids, err := service.memberChatIDs(ctx, alice)
	if err != nil {
		t.Fatalf("memberChatIDs: %v", err)
	}
	if slices.Contains(ids, chatID.String()) {
		t.Error("a deleted chat was still searchable")
	}
}

func TestSearchScopeIsBounded(t *testing.T) {
	// An unbounded terms filter is a clause per chat, so one person in very
	// many chats could otherwise make a single query arbitrarily expensive for
	// the whole cluster.
	if maxSearchableChats <= 0 {
		t.Fatalf("maxSearchableChats = %d, want a positive bound", maxSearchableChats)
	}
}
