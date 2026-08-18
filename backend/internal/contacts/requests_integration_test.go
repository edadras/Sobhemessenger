package contacts_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/contacts"
	"github.com/sobh/messenger/backend/internal/database"
)

// Contact requests (§54).

// openRequests is what "who is waiting on me" should show.
func openRequests(t *testing.T, service *contacts.Service, userID uuid.UUID, direction string) []contacts.Request {
	t.Helper()
	requests, err := service.Requests(context.Background(), userID, direction, 100)
	if err != nil {
		t.Fatalf("Requests(%s): %v", direction, err)
	}
	return requests
}

// allowEveryone widens the privacy rule that governs unsolicited approaches.
// The registration default is "contacts", which would refuse every request in
// this file for the right reason and prove nothing about the rest.
func allowEveryone(t *testing.T, db *database.DB, userID uuid.UUID) {
	t.Helper()
	if _, err := db.Pool.Exec(context.Background(), `
		INSERT INTO user_privacy_settings (user_id, key, rule) VALUES ($1, 'messages', 'everyone')
		ON CONFLICT (user_id, key) DO UPDATE SET rule = 'everyone'`, userID); err != nil {
		t.Fatalf("set privacy: %v", err)
	}
}

func isContact(t *testing.T, db *database.DB, ownerID, contactID uuid.UUID) bool {
	t.Helper()
	var exists bool
	if err := db.Pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM contacts WHERE owner_id = $1 AND contact_id = $2)`,
		ownerID, contactID).Scan(&exists); err != nil {
		t.Fatalf("read contacts: %v", err)
	}
	return exists
}

func TestAcceptingARequestMakesTheContactMutual(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	request, err := service.SendRequest(ctx, alice, bob, "hello, it's Alice")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if request.Status != "pending" {
		t.Errorf("status = %q, want pending", request.Status)
	}

	// Before acceptance neither address book has changed: a request is a
	// question, not a change.
	if isContact(t, db, alice, bob) || isContact(t, db, bob, alice) {
		t.Error("sending a request already added a contact")
	}

	incoming := openRequests(t, service, bob, "incoming")
	if len(incoming) != 1 || incoming[0].ID != request.ID {
		t.Fatalf("bob has %d incoming requests, want the one alice sent", len(incoming))
	}
	if incoming[0].DisplayName != "alice" {
		t.Errorf("incoming request names %q, want alice", incoming[0].DisplayName)
	}

	if _, err := service.ResolveRequest(ctx, request.ID, bob, "accepted"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !isContact(t, db, alice, bob) {
		t.Error("alice did not gain bob as a contact")
	}
	if !isContact(t, db, bob, alice) {
		t.Error("bob did not gain alice as a contact; acceptance is what makes it mutual")
	}
}

func TestOnlyTheTargetMayAcceptARequest(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	carol, _ := createUser(t, db, "carol")
	allowEveryone(t, db, bob)

	request, err := service.SendRequest(ctx, alice, bob, "")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}

	// The sender accepting their own request would be a way to add themselves
	// to someone else's address book.
	if _, err := service.ResolveRequest(ctx, request.ID, alice, "accepted"); err == nil {
		t.Error("the sender accepted their own request")
	}
	if _, err := service.ResolveRequest(ctx, request.ID, carol, "accepted"); err == nil {
		t.Error("an unrelated person accepted somebody else's request")
	}
	if isContact(t, db, bob, alice) {
		t.Error("a refused acceptance still wrote a contact")
	}
}

func TestOnlyTheSenderMayCancelARequest(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	request, err := service.SendRequest(ctx, alice, bob, "")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if _, err := service.ResolveRequest(ctx, request.ID, bob, "cancelled"); err == nil {
		t.Error("the target cancelled a request they were meant to accept or reject")
	}
	if _, err := service.ResolveRequest(ctx, request.ID, alice, "cancelled"); err != nil {
		t.Fatalf("the sender could not cancel their own request: %v", err)
	}
	if got := openRequests(t, service, bob, "incoming"); len(got) != 0 {
		t.Errorf("bob still sees %d incoming requests after it was withdrawn", len(got))
	}
}

func TestARequestIsResolvedOnlyOnce(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	request, err := service.SendRequest(ctx, alice, bob, "")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if _, err := service.ResolveRequest(ctx, request.ID, bob, "rejected"); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if _, err := service.ResolveRequest(ctx, request.ID, bob, "accepted"); err == nil {
		t.Error("a rejected request was then accepted")
	}
	if isContact(t, db, bob, alice) {
		t.Error("accepting an already-rejected request still wrote a contact")
	}
}

func TestRejectionIsVisibleToTheSenderAndNobodyElse(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	request, err := service.SendRequest(ctx, alice, bob, "")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if _, err := service.ResolveRequest(ctx, request.ID, bob, "rejected"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Bob's list is for things to act on, so a resolved request leaves it.
	if got := openRequests(t, service, bob, "incoming"); len(got) != 0 {
		t.Errorf("bob still has %d incoming requests after resolving one", len(got))
	}
	// Alice keeps hers, or she cannot tell a decline from a request that
	// never arrived.
	outgoing := openRequests(t, service, alice, "outgoing")
	if len(outgoing) != 1 || outgoing[0].Status != "rejected" {
		t.Errorf("alice's outgoing list is %+v; she should see it was declined", outgoing)
	}
}

func TestResendingARequestDoesNotCreateASecond(t *testing.T) {
	db := testDB(t)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	first, err := service.SendRequest(ctx, alice, bob, "hello")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	second, err := service.SendRequest(ctx, alice, bob, "hello again")
	if err != nil {
		t.Fatalf("SendRequest (again): %v", err)
	}
	if first.ID != second.ID {
		t.Errorf("a retry created a second request (%s and %s)", first.ID, second.ID)
	}
	if second.Message != "hello again" {
		t.Errorf("message = %q; the newer note should replace the older", second.Message)
	}
	if got := openRequests(t, service, bob, "incoming"); len(got) != 1 {
		t.Errorf("bob sees %d requests from one person", len(got))
	}
}

func TestBlockingRefusesARequestWithoutSayingSo(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	quiet, _ := createUser(t, db, "quiet")
	allowEveryone(t, db, bob)

	if err := repo.Block(ctx, bob, alice, "no thanks"); err != nil {
		t.Fatalf("Block: %v", err)
	}

	_, blockedErr := service.SendRequest(ctx, alice, bob, "")
	if blockedErr == nil {
		t.Fatal("a blocked user sent a contact request")
	}

	// Someone who simply does not accept approaches gives the same answer, so
	// a request cannot be used to detect a block.
	_, privacyErr := service.SendRequest(ctx, alice, quiet, "")
	if privacyErr == nil {
		t.Fatal("a request reached someone whose privacy rule forbids it")
	}
	if blockedErr.Error() != privacyErr.Error() {
		t.Errorf("blocking and a privacy rule give different answers:\n  %v\n  %v",
			blockedErr, privacyErr)
	}
}

func TestARequestToAnExistingMutualContactIsRefused(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := repo.Add(ctx, bob, alice, "Alice", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	if _, err := service.SendRequest(ctx, alice, bob, ""); err == nil {
		t.Error("a request was opened between two people who are already contacts")
	}
}

func TestARequestToYourselfIsRefused(t *testing.T) {
	db := testDB(t)
	service := newService(db)

	alice, _ := createUser(t, db, "alice")
	if _, err := service.SendRequest(context.Background(), alice, alice, ""); err == nil {
		t.Error("a request to oneself was accepted")
	}
}

func TestAOneSidedContactCanStillBeAskedToReciprocate(t *testing.T) {
	db := testDB(t)
	repo := contacts.NewRepository(db)
	service := newService(db)
	ctx := context.Background()

	alice, _ := createUser(t, db, "alice")
	bob, _ := createUser(t, db, "bob")
	allowEveryone(t, db, bob)

	// Alice has Bob in her address book; Bob has never heard of her. That is
	// exactly the state a request exists to resolve.
	if err := repo.Add(ctx, alice, bob, "Bob", ""); err != nil {
		t.Fatalf("Add: %v", err)
	}

	request, err := service.SendRequest(ctx, alice, bob, "we met at the conference")
	if err != nil {
		t.Fatalf("SendRequest: %v", err)
	}
	if _, err := service.ResolveRequest(ctx, request.ID, bob, "accepted"); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if !isContact(t, db, bob, alice) {
		t.Error("bob did not gain alice")
	}
}
