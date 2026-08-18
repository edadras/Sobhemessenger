package messaging_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/messaging"
)

// Who has read a message (§7).
//
// The cursor answers "how far has this person read", which is what a chat list
// needs and all `chat_members` holds. It cannot answer "who has read *this*",
// which is what a sender looks for in a group — `message_reads` does, and had
// been in the schema since migration 0004 without a single row being written.

func readerIDs(t *testing.T, service *messaging.Service, messageID, viewerID uuid.UUID) []uuid.UUID {
	t.Helper()
	receipts, err := service.ReadReceipts(context.Background(), messageID, viewerID, 100)
	if err != nil {
		t.Fatalf("ReadReceipts: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(receipts))
	for _, receipt := range receipts {
		ids = append(ids, receipt.UserID)
	}
	return ids
}

func TestReadingAMessageRecordsWhoReadIt(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	first := createUser(t, db, "first")
	second := createUser(t, db, "second")
	chatID := groupChat(t, db, owner, first, second)

	message := send(t, repo, chatID, owner, "has anyone seen this")

	if got := readerIDs(t, service, message.ID, owner); len(got) != 0 {
		t.Errorf("%d people had read it before anyone opened the chat", len(got))
	}

	if _, err := service.MarkRead(ctx, chatID, first, message.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	got := readerIDs(t, service, message.ID, owner)
	if len(got) != 1 || got[0] != first {
		t.Errorf("readers = %v, want just the one who read it", got)
	}

	if _, err := service.MarkRead(ctx, chatID, second, message.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := readerIDs(t, service, message.ID, owner); len(got) != 2 {
		t.Errorf("readers = %v, want both", got)
	}
}

func TestTheSenderIsNotListedAsHavingReadTheirOwnMessage(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	message := send(t, repo, chatID, owner, "mine")
	// The sender's own cursor moves with the send, and reading the chat
	// afterwards must not add them to their own receipt list — it is noise in
	// every one of them.
	if _, err := service.MarkRead(ctx, chatID, owner, message.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	for _, reader := range readerIDs(t, service, message.ID, owner) {
		if reader == owner {
			t.Error("the sender is listed as having read their own message")
		}
	}
}

func TestReadingTwiceDoesNotDuplicateAReceipt(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	message := send(t, repo, chatID, owner, "once")

	for i := 0; i < 3; i++ {
		if _, err := service.MarkRead(ctx, chatID, member, message.Seq); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
	}

	if got := readerIDs(t, service, message.ID, owner); len(got) != 1 {
		t.Errorf("%d receipts after reading three times", len(got))
	}
}

func TestOnlyTheMessagesTheCursorPassedGetAReceipt(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	first := send(t, repo, chatID, owner, "one")
	second := send(t, repo, chatID, owner, "two")

	// Read as far as the first only.
	if _, err := service.MarkRead(ctx, chatID, member, first.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := readerIDs(t, service, first.ID, owner); len(got) != 1 {
		t.Errorf("the first message has %d readers, want 1", len(got))
	}
	if got := readerIDs(t, service, second.ID, owner); len(got) != 0 {
		t.Errorf("the second message has %d readers before it was reached", len(got))
	}

	if _, err := service.MarkRead(ctx, chatID, member, second.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := readerIDs(t, service, second.ID, owner); len(got) != 1 {
		t.Errorf("the second message has %d readers after being reached", len(got))
	}
}

func TestAReceiptListIsOnlyForMembersOfTheChat(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)
	message := send(t, repo, chatID, owner, "private to the group")

	// A receipt list names people. An id from another conversation must not be
	// a way to enumerate them.
	if _, err := service.ReadReceipts(context.Background(), message.ID, outsider, 100); err == nil {
		t.Error("an outsider read the receipt list for a message in a chat they are not in")
	}
}

func TestACursorThatDoesNotMoveRecordsNothing(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)
	message := send(t, repo, chatID, owner, "one")

	if _, err := service.MarkRead(ctx, chatID, member, message.Seq); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	// A cursor never moves backwards, and a call that moves it nowhere must
	// not rewrite the receipts it already has.
	if _, err := service.MarkRead(ctx, chatID, member, 1); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}

	if got := readerIDs(t, service, message.ID, owner); len(got) != 1 {
		t.Errorf("%d receipts after a backwards read", len(got))
	}
}
