package messaging_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/messaging"
)

// A restricted account and the narrow shape of the restriction (§34).

// stubGuard stands in for the anti-spam module. The module's own scoring is
// tested against a real database in its own package; what matters here is what
// the messaging service does once something is restricted, so the verdict is
// supplied directly rather than earned.
type stubGuard struct {
	restricted  map[uuid.UUID]bool
	rateLimited []uuid.UUID
}

func (g *stubGuard) Restricted(_ context.Context, userID uuid.UUID) (bool, time.Time) {
	if g.restricted[userID] {
		return true, time.Now().Add(time.Hour)
	}
	return false, time.Time{}
}

func (g *stubGuard) RateLimited(_ context.Context, userID uuid.UUID) {
	g.rateLimited = append(g.rateLimited, userID)
}

func TestARestrictedAccountCannotMessageAStranger(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	spammer := createUser(t, db, "spammer")
	stranger := createUser(t, db, "stranger")
	chatID := privateChat(t, db, spammer, stranger)

	guard := &stubGuard{restricted: map[uuid.UUID]bool{spammer: true}}
	service.SetSpamGuard(guard)

	_, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: spammer, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "buy my thing",
	})
	if err == nil {
		t.Fatal("a restricted account reached someone who does not know it")
	}
}

func TestARestrictedAccountCanStillTalkToPeopleWhoKnowIt(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	spammer := createUser(t, db, "spammer")
	friend := createUser(t, db, "friend")
	chatID := privateChat(t, db, spammer, friend)

	// The friend has the restricted account in their address book, which is
	// what invites the conversation.
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO contacts (owner_id, contact_id) VALUES ($1, $2)`,
		friend, spammer); err != nil {
		t.Fatalf("add contact: %v", err)
	}

	service.SetSpamGuard(&stubGuard{restricted: map[uuid.UUID]bool{spammer: true}})

	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: spammer, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "hello again",
	}); err != nil {
		t.Fatalf("a restriction silenced a conversation the other side invited: %v", err)
	}
}

func TestARestrictedAccountCanStillPostInItsGroups(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	spammer := createUser(t, db, "spammer")
	chatID := groupChat(t, db, owner, spammer)

	service.SetSpamGuard(&stubGuard{restricted: map[uuid.UUID]bool{spammer: true}})

	// The restriction is about approaching strangers, not about silencing an
	// account. A group it is already a member of is not an approach.
	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: spammer, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "hello group",
	}); err != nil {
		t.Fatalf("a restricted member could not post in a group it belongs to: %v", err)
	}
}

func TestAnUnrestrictedAccountIsUnaffected(t *testing.T) {
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	stranger := createUser(t, db, "stranger")
	chatID := privateChat(t, db, alice, stranger)

	service.SetSpamGuard(&stubGuard{restricted: map[uuid.UUID]bool{}})

	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "hello, we have not met",
	}); err != nil {
		t.Fatalf("an ordinary first message was refused: %v", err)
	}
}
