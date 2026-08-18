package messaging_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// These tests run against a real PostgreSQL instance with the migrations
// applied. They are skipped when SOBH_TEST_POSTGRES_DSN is unset, so `go test
// ./...` stays runnable without a database.
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

// createUser inserts the minimum rows a user needs to participate in a chat.
func createUser(t *testing.T, db *database.DB, label string) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte(label)).Scan(&id)
	if err != nil {
		t.Fatalf("create user %s: %v", label, err)
	}

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`, id, label); err != nil {
		t.Fatalf("create profile for %s: %v", label, err)
	}
	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO user_event_counters (user_id) VALUES ($1)`, id); err != nil {
		t.Fatalf("create event counter for %s: %v", label, err)
	}

	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

func TestPrivateChatIsCreatedOnceForAPair(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")

	first, created, err := repo.EnsurePrivateChat(ctx, alice, bob)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}
	if !created {
		t.Error("expected the first call to create the chat")
	}

	// The reverse order must resolve to the same chat: the key is normalised.
	second, created, err := repo.EnsurePrivateChat(ctx, bob, alice)
	if err != nil {
		t.Fatalf("EnsurePrivateChat (reversed): %v", err)
	}
	if created {
		t.Error("expected the second call to reuse the existing chat")
	}
	if first != second {
		t.Errorf("got two chats for one pair: %s and %s", first, second)
	}
}

func TestConcurrentOpensShareOnePrivateChat(t *testing.T) {
	// Two people can tap each other's name at the same moment, and one person's
	// two devices can do it on the same restore. Only one of the racers wins the
	// insert; the rest must find and adopt the winner's chat.
	//
	// The losing path is easy to get wrong: the unique violation aborts the
	// whole transaction, so a re-read attempted inside it fails with 25P02
	// rather than returning the winner's row. This test is what distinguishes
	// the two.
	db := testDB(t)
	repo := messaging.NewRepository(db)

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")

	const racers = 8
	ids := make([]uuid.UUID, racers)
	errs := make([]error, racers)
	start := make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			<-start
			a, b := alice, bob
			if slot%2 == 1 {
				a, b = bob, alice
			}
			ids[slot], _, errs[slot] = repo.EnsurePrivateChat(context.Background(), a, b)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Errorf("racer %d got chat %s, want %s", i, id, ids[0])
		}
	}
}

func TestSendAssignsContiguousSequenceNumbers(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, err := repo.EnsurePrivateChat(ctx, alice, bob)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}

	for i := 1; i <= 5; i++ {
		result, err := repo.Send(ctx, messaging.SendParams{
			ChatID:          chatID,
			SenderID:        alice,
			ClientMessageID: uuid.New(),
			Type:            messaging.TypeText,
			Content:         "message",
		})
		if err != nil {
			t.Fatalf("Send #%d: %v", i, err)
		}
		if result.Message.Seq != int64(i) {
			t.Errorf("message %d got seq %d, want %d", i, result.Message.Seq, i)
		}
	}
}

// A retried send must return the original message rather than duplicating it.
func TestSendIsIdempotentPerClientMessageID(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	clientID := uuid.New()
	first, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "hello",
	})
	if err != nil {
		t.Fatalf("first send: %v", err)
	}
	if first.Duplicate {
		t.Error("first send was reported as a duplicate")
	}

	second, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "hello",
	})
	if err != nil {
		t.Fatalf("retried send: %v", err)
	}
	if !second.Duplicate {
		t.Error("retried send was not reported as a duplicate")
	}
	if first.Message.ID != second.Message.ID {
		t.Errorf("retry produced a different message: %s vs %s", first.Message.ID, second.Message.ID)
	}

	var count int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1`, chatID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 1 {
		t.Errorf("stored %d messages, want 1", count)
	}
}

// Concurrent sends must not collide on the (chat_id, seq) unique key.
func TestConcurrentSendsDoNotCollide(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	const senders = 8
	var wg sync.WaitGroup
	errs := make(chan error, senders)
	seqs := make(chan int64, senders)

	for i := 0; i < senders; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := repo.Send(ctx, messaging.SendParams{
				ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
				Type: messaging.TypeText, Content: "concurrent",
			})
			if err != nil {
				errs <- err
				return
			}
			seqs <- result.Message.Seq
		}()
	}
	wg.Wait()
	close(errs)
	close(seqs)

	for err := range errs {
		t.Fatalf("concurrent send failed: %v", err)
	}

	seen := make(map[int64]bool)
	for seq := range seqs {
		if seen[seq] {
			t.Errorf("sequence %d was assigned twice", seq)
		}
		seen[seq] = true
	}
	if len(seen) != senders {
		t.Errorf("got %d distinct sequences, want %d", len(seen), senders)
	}
}

func TestSendWritesRecipientSyncEvents(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	result, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "hello bob",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Both members get an event, the sender included. The log is per user and
	// it is what a second device reads to catch up (§9), so leaving the sender
	// out would mean their tablet never learned what their phone had sent.
	if len(result.Recipients) != 2 {
		t.Fatalf("recipients = %v, want both members", result.Recipients)
	}
	found := map[uuid.UUID]bool{}
	for _, recipient := range result.Recipients {
		found[recipient] = true
	}
	if !found[alice] || !found[bob] {
		t.Fatalf("recipients = %v, want both %s and %s", result.Recipients, alice, bob)
	}

	events, latest, err := repo.EventsSince(ctx, bob, 0, 10)
	if err != nil {
		t.Fatalf("EventsSince(bob): %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("bob has %d events, want 1", len(events))
	}
	if events[0].Type != messaging.EventMessageNew {
		t.Errorf("event type = %q, want %q", events[0].Type, messaging.EventMessageNew)
	}
	if latest != events[0].Seq {
		t.Errorf("server head = %d, want %d", latest, events[0].Seq)
	}

	// The sender gets one too. This used to assert zero, which is what made a
	// second device blind to anything sent from the first — it would find the
	// message only by refetching the whole conversation.
	aliceEvents, _, err := repo.EventsSince(ctx, alice, 0, 10)
	if err != nil {
		t.Fatalf("EventsSince(alice): %v", err)
	}
	if len(aliceEvents) != 1 {
		t.Errorf("sender received %d events for their own message, want 1 for their other devices",
			len(aliceEvents))
	}
}

func TestUnreadCountsAndReadCursor(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	for i := 0; i < 3; i++ {
		if _, err := repo.Send(ctx, messaging.SendParams{
			ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
			Type: messaging.TypeText, Content: "unread",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	var unread int
	if err := db.Pool.QueryRow(ctx,
		`SELECT unread_count FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, bob).Scan(&unread); err != nil {
		t.Fatalf("read unread count: %v", err)
	}
	if unread != 3 {
		t.Errorf("bob's unread count = %d, want 3", unread)
	}

	// The sender's own messages never count as unread for them.
	var senderUnread int
	if err := db.Pool.QueryRow(ctx,
		`SELECT unread_count FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, alice).Scan(&senderUnread); err != nil {
		t.Fatalf("read sender unread count: %v", err)
	}
	if senderUnread != 0 {
		t.Errorf("alice's unread count = %d, want 0", senderUnread)
	}

	if _, err := repo.MarkRead(ctx, chatID, bob, 3); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if err := db.Pool.QueryRow(ctx,
		`SELECT unread_count FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, bob).Scan(&unread); err != nil {
		t.Fatalf("read unread count after MarkRead: %v", err)
	}
	if unread != 0 {
		t.Errorf("bob's unread count after reading = %d, want 0", unread)
	}

	// The cursor must never move backwards.
	if _, err := repo.MarkRead(ctx, chatID, bob, 1); err != nil {
		t.Fatalf("MarkRead (backwards): %v", err)
	}
	var lastRead int64
	if err := db.Pool.QueryRow(ctx,
		`SELECT last_read_seq FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, bob).Scan(&lastRead); err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if lastRead != 3 {
		t.Errorf("read cursor = %d after a lower MarkRead, want 3", lastRead)
	}
}

func TestReactionTogglesOffOnRepeat(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	sent, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "react to me",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	added, _, err := repo.React(ctx, sent.Message.ID, bob, "❤️")
	if err != nil {
		t.Fatalf("React: %v", err)
	}
	if !added {
		t.Error("first reaction should have been added")
	}

	added, _, err = repo.React(ctx, sent.Message.ID, bob, "❤️")
	if err != nil {
		t.Fatalf("React (toggle off): %v", err)
	}
	if added {
		t.Error("repeating the same reaction should remove it")
	}
}

func TestDeleteLeavesATombstone(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	sent, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "delete me",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	_, seq, recipients, err := repo.Delete(ctx, sent.Message.ID, alice)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if seq != sent.Message.Seq {
		t.Errorf("deleted seq = %d, want %d", seq, sent.Message.Seq)
	}
	// Both members, the deleter included: their other devices have to remove it
	// from the conversation too.
	if len(recipients) != 2 {
		t.Errorf("delete notified %d recipients, want both members", len(recipients))
	}

	// The row survives so the sequence stays contiguous, but the body is gone.
	messages, err := repo.History(ctx, chatID, bob, nil, nil, 10)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("history has %d messages, want 1", len(messages))
	}
	if messages[0].DeletedAt == nil {
		t.Error("deleted message is not marked as deleted")
	}
	if messages[0].Content != "" {
		t.Errorf("deleted message still has content %q", messages[0].Content)
	}
}

func TestHistoryPagesBackwardsBySequence(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	for i := 0; i < 10; i++ {
		if _, err := repo.Send(ctx, messaging.SendParams{
			ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
			Type: messaging.TypeText, Content: "page me",
		}); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	first, err := repo.History(ctx, chatID, bob, nil, nil, 4)
	if err != nil {
		t.Fatalf("History page 1: %v", err)
	}
	if len(first) != 4 {
		t.Fatalf("page 1 has %d messages, want 4", len(first))
	}
	if first[0].Seq != 10 {
		t.Errorf("newest message seq = %d, want 10", first[0].Seq)
	}

	cursor := first[len(first)-1].Seq
	second, err := repo.History(ctx, chatID, bob, &cursor, nil, 4)
	if err != nil {
		t.Fatalf("History page 2: %v", err)
	}
	if len(second) != 4 {
		t.Fatalf("page 2 has %d messages, want 4", len(second))
	}
	if second[0].Seq >= cursor {
		t.Errorf("page 2 starts at seq %d, want below the cursor %d", second[0].Seq, cursor)
	}
}

func TestOneToOneChatsCarryTheirPeer(t *testing.T) {
	// A private chat has no title of its own, so without the resolved peer the
	// whole chat list renders nameless — the single most visible thing a
	// messenger can get wrong. The peer is also what a client needs to open an
	// encrypted chat, which is addressed to a person rather than a chat.
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")

	if _, err := db.Pool.Exec(ctx,
		`UPDATE user_profiles SET display_name = $2 WHERE user_id = $1`,
		bob, "باب"); err != nil {
		t.Fatalf("give bob a display name: %v", err)
	}

	privateID, _, err := repo.EnsurePrivateChat(ctx, alice, bob)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}

	chats, err := repo.ListChats(ctx, alice, 50, nil)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}

	found := false
	for _, chat := range chats {
		if chat.ID != privateID {
			continue
		}
		found = true
		if chat.Peer == nil {
			t.Fatal("the private chat came back without a peer")
		}
		// Alice must see Bob, not herself.
		if chat.Peer.UserID != bob {
			t.Errorf("peer = %s, want bob (%s)", chat.Peer.UserID, bob)
		}
		if chat.Peer.DisplayName != "باب" {
			t.Errorf("peer display name = %q, want %q", chat.Peer.DisplayName, "باب")
		}
	}
	if !found {
		t.Fatal("the private chat was not in the list")
	}

	// And the same chat from Bob's side names Alice, not himself.
	fromBob, err := repo.ListChats(ctx, bob, 50, nil)
	if err != nil {
		t.Fatalf("ListChats for bob: %v", err)
	}
	for _, chat := range fromBob {
		if chat.ID == privateID {
			if chat.Peer == nil || chat.Peer.UserID != alice {
				t.Errorf("bob's view names %v, want alice (%s)", chat.Peer, alice)
			}
		}
	}
}

func TestGroupChatsHaveNoPeer(t *testing.T) {
	// The peer only means something for a two-person conversation. A group
	// naming itself after whichever member the query happened to reach first
	// would be worse than having no name at all.
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")

	var groupID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id, title, member_count) VALUES ('group', $1, 'گروه', 2) RETURNING id`,
		alice).Scan(&groupID); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, role)
		VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, groupID, alice, bob); err != nil {
		t.Fatalf("add group members: %v", err)
	}

	chats, err := repo.ListChats(ctx, alice, 50, nil)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	for _, chat := range chats {
		if chat.ID == groupID && chat.Peer != nil {
			t.Errorf("the group came back with a peer: %+v", chat.Peer)
		}
	}
}

func TestTheSenderGetsTheirOwnEventForOtherDevices(t *testing.T) {
	// The sync log is per user, not per device, and it is the only thing a
	// second device reads to catch up (§9). Leaving the sender out meant their
	// tablet never learned what their phone had just sent: it would find the
	// message only by refetching the whole conversation, and the chat list
	// would go on showing a stale last message until it did.
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, err := repo.EnsurePrivateChat(ctx, alice, bob)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}

	result, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "from my phone",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, party := range []struct {
		name string
		id   uuid.UUID
	}{{"the sender", alice}, {"the recipient", bob}} {
		events, _, err := repo.EventsSince(ctx, party.id, 0, 100)
		if err != nil {
			t.Fatalf("EventsSince for %s: %v", party.name, err)
		}
		found := false
		for _, event := range events {
			if event.Type == messaging.EventMessageNew &&
				strings.Contains(string(event.Payload), result.Message.ID.String()) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no event for the message; %d events in the log", party.name, len(events))
		}
	}
}

func TestSendingDoesNotMakeAMessageUnreadForItsSender(t *testing.T) {
	// The sender now receives an event, which must not be mistaken for the
	// unread counter also counting it. Those are separate statements and only
	// one of them should skip the sender.
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	if _, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "hello",
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	unread := func(user uuid.UUID) int {
		var count int
		if err := db.Pool.QueryRow(ctx,
			`SELECT unread_count FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
			chatID, user).Scan(&count); err != nil {
			t.Fatalf("read unread count: %v", err)
		}
		return count
	}

	if got := unread(alice); got != 0 {
		t.Errorf("the sender's unread count is %d, want 0", got)
	}
	if got := unread(bob); got != 1 {
		t.Errorf("the recipient's unread count is %d, want 1", got)
	}
}

func TestEditingAndDeletingReachTheActorsOwnDevices(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, _ := repo.EnsurePrivateChat(ctx, alice, bob)

	result, err := repo.Send(ctx, messaging.SendParams{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "before",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	countFor := func(user uuid.UUID, eventType string) int {
		events, _, err := repo.EventsSince(ctx, user, 0, 200)
		if err != nil {
			t.Fatalf("EventsSince: %v", err)
		}
		n := 0
		for _, event := range events {
			if event.Type == eventType {
				n++
			}
		}
		return n
	}

	if _, _, err := repo.Edit(ctx, result.Message.ID, alice, "after", nil); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if countFor(alice, messaging.EventMessageEdited) == 0 {
		t.Error("the editor's own devices were told nothing about the edit")
	}

	if _, _, _, err := repo.Delete(ctx, result.Message.ID, alice); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if countFor(alice, messaging.EventMessageDeleted) == 0 {
		t.Error("the deleter's own devices were told nothing about the deletion")
	}
}
