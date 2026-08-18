package messaging_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Clearing a chat's history (§12).
//
// The property under test throughout is asymmetry: one member emptying their
// own copy must be invisible to the other, and must not touch a single row of
// `messages`. The destructive form is the exception, and it is gated.

func privateChat(t *testing.T, db *database.DB, a, b uuid.UUID) uuid.UUID {
	t.Helper()
	repo := messaging.NewRepository(db)
	chatID, _, err := repo.EnsurePrivateChat(context.Background(), a, b)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})
	return chatID
}

func historySeqs(t *testing.T, repo *messaging.Repository, chatID, viewerID uuid.UUID) []int64 {
	t.Helper()
	messages, err := repo.History(context.Background(), chatID, viewerID, nil, nil, 100)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	seqs := make([]int64, 0, len(messages))
	for _, message := range messages {
		seqs = append(seqs, message.Seq)
	}
	return seqs
}

func TestClearingHistoryEmptiesOnlyTheCallersView(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := privateChat(t, db, alice, bob)

	send(t, repo, chatID, alice, "first")
	send(t, repo, chatID, bob, "second")

	watermark, err := service.ClearHistory(ctx, chatID, alice, false)
	if err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	if watermark != 2 {
		t.Errorf("cleared up to seq %d, want 2", watermark)
	}

	if got := historySeqs(t, repo, chatID, alice); len(got) != 0 {
		t.Errorf("alice still sees %v after clearing her copy", got)
	}
	if got := historySeqs(t, repo, chatID, bob); len(got) != 2 {
		t.Errorf("bob sees %v; alice clearing her own copy must not touch his", got)
	}

	// Nothing was deleted: the rows are all still there, which is what makes
	// the operation reversible for everyone who did not ask for it.
	var live int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1 AND deleted_at IS NULL`,
		chatID).Scan(&live); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if live != 2 {
		t.Errorf("%d live messages remain, want 2 — a one-sided clear must delete nothing", live)
	}
}

func TestClearingHistoryLeavesLaterMessagesVisible(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := privateChat(t, db, alice, bob)

	send(t, repo, chatID, bob, "before")
	if _, err := service.ClearHistory(context.Background(), chatID, alice, false); err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	after := send(t, repo, chatID, bob, "after")

	got := historySeqs(t, repo, chatID, alice)
	if len(got) != 1 || got[0] != after.Seq {
		t.Errorf("alice sees %v after clearing; want just the later message at seq %d", got, after.Seq)
	}
}

func TestClearingHistoryResetsTheUnreadCount(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := privateChat(t, db, alice, bob)

	send(t, repo, chatID, bob, "unread one")
	send(t, repo, chatID, bob, "unread two")

	if _, err := service.ClearHistory(ctx, chatID, alice, false); err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}

	var unread int
	if err := db.Pool.QueryRow(ctx,
		`SELECT unread_count FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, alice).Scan(&unread); err != nil {
		t.Fatalf("read unread count: %v", err)
	}
	if unread != 0 {
		t.Errorf("unread count is %d after clearing; a chat with no visible history cannot be unread", unread)
	}
}

func TestClearingForEveryoneEmptiesBothSides(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := privateChat(t, db, alice, bob)

	send(t, repo, chatID, alice, "first")
	send(t, repo, chatID, bob, "second")

	if _, err := service.ClearHistory(ctx, chatID, alice, true); err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}

	if got := historySeqs(t, repo, chatID, alice); len(got) != 0 {
		t.Errorf("alice still sees %v", got)
	}
	if got := historySeqs(t, repo, chatID, bob); len(got) != 0 {
		t.Errorf("bob still sees %v after a clear for everyone", got)
	}

	var live int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1 AND deleted_at IS NULL`,
		chatID).Scan(&live); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if live != 0 {
		t.Errorf("%d messages survived a clear for everyone", live)
	}
}

func TestClearingForEveryoneNeedsThePermission(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	send(t, repo, chatID, owner, "the owner's message")

	if _, err := service.ClearHistory(ctx, chatID, member, true); err == nil {
		t.Fatal("an ordinary member cleared a group for everyone")
	}

	// The same member may still empty their own copy: that is theirs to do.
	if _, err := service.ClearHistory(ctx, chatID, member, false); err != nil {
		t.Fatalf("clearing their own copy: %v", err)
	}
	if got := historySeqs(t, repo, chatID, member); len(got) != 0 {
		t.Errorf("member still sees %v", got)
	}
	if got := historySeqs(t, repo, chatID, owner); len(got) != 1 {
		t.Errorf("owner sees %v; a member clearing their own copy must not touch the owner's", got)
	}
}

func TestClearingHistoryRefusesNonMembers(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)

	if _, err := service.ClearHistory(context.Background(), chatID, outsider, false); err == nil {
		t.Fatal("an outsider cleared a chat they are not in")
	}
}

func TestClearingHistoryTellsTheCallersOtherDevices(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := privateChat(t, db, alice, bob)
	send(t, repo, chatID, bob, "hello")

	before, _, err := repo.EventsSince(ctx, bob, 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}

	if _, err := service.ClearHistory(ctx, chatID, alice, false); err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}

	events, _, err := repo.EventsSince(ctx, alice, 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	var cleared int
	for _, event := range events {
		if event.Type == messaging.EventChatHistoryCleared {
			cleared++
		}
	}
	if cleared != 1 {
		t.Errorf("alice has %d history-cleared events, want 1 — her other devices are the only ones that need to know", cleared)
	}

	// Bob's log must be untouched: he was not asked to drop anything.
	after, _, err := repo.EventsSince(ctx, bob, 0, 100)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("bob gained %d events from alice clearing her own copy", len(after)-len(before))
	}
}
