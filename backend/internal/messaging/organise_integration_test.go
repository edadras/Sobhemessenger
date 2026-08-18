package messaging_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Forwarding, pinning, scheduling and chat organisation (§12).
//
// The scheduling tests drive the real publisher rather than calling the
// repository directly, because the property that matters — a queued post
// becomes an ordinary message, with a sequence number and recipient events —
// only holds if publication really does go through the send path.

// newService builds a messaging service with the collaborators it cannot work
// without: a real NATS server in process and a Redis protocol implementation
// for the rate limiter. Nothing here is a stub.
func newService(t *testing.T, db *database.DB) *messaging.Service {
	t.Helper()
	service, _ := newServiceWithBus(t, db)
	return service
}

// newServiceWithBus also hands back the message bus, so a test can consume the
// jobs the service queues rather than assuming they were queued.
func newServiceWithBus(t *testing.T, db *database.DB) (*messaging.Service, *bus.Bus) {
	t.Helper()
	ctx := context.Background()

	logOutput := io.Discard
	if os.Getenv("SOBH_TEST_LOG") != "" {
		logOutput = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metrics := observability.New("messaging-test")

	opts := &natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true,
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(srv.Shutdown)

	messageBus, err := bus.Connect(config.NATS{
		URL:           srv.ClientURL(),
		StreamName:    "SOBH_ORGANISE_TEST",
		MaxReconnects: 3,
		ReconnectWait: time.Second,
	}, logger, metrics)
	if err != nil {
		t.Fatalf("connect to test nats: %v", err)
	}
	t.Cleanup(messageBus.Close)

	redis := miniredis.RunT(t)
	cacheClient, err := cache.Connect(ctx, config.Redis{Addr: redis.Addr(), PoolSize: 8})
	if err != nil {
		t.Fatalf("connect to test redis: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })

	// The limits are widened so a test that sends a handful of messages in a
	// row is not measuring the rate limiter.
	rules := ratelimit.NewRules(config.RateLimits{
		OTPPerPhonePerHour: 10000, OTPPerIPPerHour: 10000, LoginPerIPPerHour: 10000,
		APIPerUserPerMin: 10000, APIPerIPPerMin: 10000,
		MessagesPerMin: 10000, UploadsPerHour: 10000,
	})

	return messaging.NewService(messaging.NewRepository(db), messageBus,
		ratelimit.New(cacheClient, metrics), rules, metrics, logger), messageBus
}

// groupChat makes a chat with three members, which is what forwarding and
// pinning need: a private chat has no permission model worth testing.
func groupChat(t *testing.T, db *database.DB, owner uuid.UUID, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx, `
		INSERT INTO chats (type, title, creator_id, member_count)
		VALUES ('group', 'test group', $1, $2) RETURNING id`,
		owner, len(members)+1).Scan(&chatID); err != nil {
		t.Fatalf("create group chat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})

	if _, err := db.Pool.Exec(ctx,
		`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'owner')`,
		chatID, owner); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	for _, member := range members {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}
	return chatID
}

func send(t *testing.T, repo *messaging.Repository, chatID, senderID uuid.UUID, content string) *messaging.Message {
	t.Helper()
	result, err := repo.Send(context.Background(), messaging.SendParams{
		ChatID:          chatID,
		SenderID:        senderID,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         content,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return result.Message
}

// ---------------------------------------------------------------- forwarding

func TestForwardCarriesTheOriginalAuthor(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	forwarder := createUser(t, db, "forwarder")
	source := groupChat(t, db, author, forwarder)
	destination := groupChat(t, db, forwarder)

	original := send(t, repo, source, author, "the original text")

	forwarded, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: source,
		ToChatID:   destination,
		MessageIDs: []uuid.UUID{original.ID},
		SenderID:   forwarder,
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if len(forwarded) != 1 {
		t.Fatalf("forwarded %d messages, want 1", len(forwarded))
	}

	copied := forwarded[0]
	if copied.Content != original.Content {
		t.Errorf("forwarded content is %q, want %q", copied.Content, original.Content)
	}
	if copied.ChatID != destination {
		t.Errorf("forward landed in %s, want %s", copied.ChatID, destination)
	}
	// The credit belongs to whoever wrote it, not to whoever passed it on.
	if copied.ForwardFrom == nil || copied.ForwardFrom.UserID == nil || *copied.ForwardFrom.UserID != author {
		t.Fatalf("forward attributes %+v, want author %s", copied.ForwardFrom, author)
	}
	if copied.SenderID == nil || *copied.SenderID != forwarder {
		t.Errorf("forward was sent by %v, want the forwarder %s", copied.SenderID, forwarder)
	}
}

// Forwarding a forward must still credit the first author: otherwise a chain
// slowly reassigns authorship to the last person who passed it on.
func TestForwardChainKeepsTheFirstAuthor(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	middle := createUser(t, db, "middle")
	last := createUser(t, db, "last")

	first := groupChat(t, db, author, middle)
	second := groupChat(t, db, middle, last)
	third := groupChat(t, db, last)

	original := send(t, repo, first, author, "written once")

	once, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: first, ToChatID: second,
		MessageIDs: []uuid.UUID{original.ID}, SenderID: middle,
	})
	if err != nil {
		t.Fatalf("first forward: %v", err)
	}

	twice, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: second, ToChatID: third,
		MessageIDs: []uuid.UUID{once[0].ID}, SenderID: last,
	})
	if err != nil {
		t.Fatalf("second forward: %v", err)
	}

	final := twice[0]
	if final.ForwardFrom == nil || final.ForwardFrom.UserID == nil {
		t.Fatalf("the twice-forwarded message credits nobody: %+v", final.ForwardFrom)
	}
	if *final.ForwardFrom.UserID != author {
		t.Errorf("credit went to %s, want the original author %s", *final.ForwardFrom.UserID, author)
	}
}

func TestForwardWithoutQuotingDropsTheAuthor(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	forwarder := createUser(t, db, "forwarder")
	source := groupChat(t, db, author, forwarder)
	destination := groupChat(t, db, forwarder)

	original := send(t, repo, source, author, "keep the text, drop the name")

	forwarded, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: source, ToChatID: destination,
		MessageIDs: []uuid.UUID{original.ID}, SenderID: forwarder,
		DropAuthor: true,
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if forwarded[0].ForwardFrom != nil && forwarded[0].ForwardFrom.UserID != nil {
		t.Errorf("the author survived a drop-author forward: %v", *forwarded[0].ForwardFrom.UserID)
	}
	if forwarded[0].Content != original.Content {
		t.Error("dropping the author also dropped the text")
	}
}

// A message id is guessable, so being able to name one must not be enough to
// forward it out of a chat the caller cannot read.
func TestForwardRefusesMessagesTheCallerCannotSee(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	insider := createUser(t, db, "insider")
	outsider := createUser(t, db, "outsider")
	private := groupChat(t, db, insider)
	destination := groupChat(t, db, outsider)

	secret := send(t, repo, private, insider, "not for you")

	if _, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: private, ToChatID: destination,
		MessageIDs: []uuid.UUID{secret.ID}, SenderID: outsider,
	}); err == nil {
		t.Fatal("an outsider forwarded a message out of a chat they are not in")
	}
}

func TestForwardRefusesADestinationTheCallerIsNotIn(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	stranger := createUser(t, db, "stranger")
	source := groupChat(t, db, author)
	elsewhere := groupChat(t, db, stranger)

	message := send(t, repo, source, author, "mine to read")

	if _, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: source, ToChatID: elsewhere,
		MessageIDs: []uuid.UUID{message.ID}, SenderID: author,
	}); err == nil {
		t.Fatal("a message was forwarded into a chat the sender is not a member of")
	}
}

func TestForwardKeepsTheOrderOfTheOriginals(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	source := groupChat(t, db, author)
	destination := groupChat(t, db, author)

	var ids []uuid.UUID
	for _, text := range []string{"first", "second", "third"} {
		ids = append(ids, send(t, repo, source, author, text).ID)
	}
	// Asking for them jumbled must not jumble the result: the conversation's
	// order is a property of the messages, not of the request.
	jumbled := []uuid.UUID{ids[2], ids[0], ids[1]}

	forwarded, err := svc.Forward(ctx, messaging.ForwardParams{
		FromChatID: source, ToChatID: destination,
		MessageIDs: jumbled, SenderID: author,
	})
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if len(forwarded) != 3 {
		t.Fatalf("forwarded %d messages, want 3", len(forwarded))
	}
	for i, want := range []string{"first", "second", "third"} {
		if forwarded[i].Content != want {
			t.Errorf("forward %d is %q, want %q", i, forwarded[i].Content, want)
		}
	}
}

// ------------------------------------------------------------------ pinning

func TestPinningIsVisibleToEveryMember(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	message := send(t, repo, chatID, owner, "read this")

	if err := svc.SetPinned(ctx, message.ID, owner, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	// Pinning is a property of the chat, not of the person who pinned it.
	pinned, err := svc.PinnedMessages(ctx, chatID, member)
	if err != nil {
		t.Fatalf("PinnedMessages: %v", err)
	}
	if len(pinned) != 1 || pinned[0].ID != message.ID {
		t.Fatalf("member sees %d pinned messages, want the one that was pinned", len(pinned))
	}

	if err := svc.SetPinned(ctx, message.ID, owner, false); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	pinned, err = svc.PinnedMessages(ctx, chatID, member)
	if err != nil {
		t.Fatalf("PinnedMessages after unpin: %v", err)
	}
	if len(pinned) != 0 {
		t.Fatalf("%d messages are still pinned after unpinning", len(pinned))
	}
}

func TestPinningNeedsThePermission(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner, member)

	message := send(t, repo, chatID, owner, "important")

	// An ordinary member of a group does not get to pin for everyone.
	if err := svc.SetPinned(ctx, message.ID, member, true); err == nil {
		t.Error("an ordinary member pinned a message")
	}
	if err := svc.SetPinned(ctx, message.ID, outsider, true); err == nil {
		t.Error("a non-member pinned a message")
	}
}

func TestPinnedListIsScopedToMembers(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)

	message := send(t, repo, chatID, owner, "members only")
	if err := svc.SetPinned(ctx, message.ID, owner, true); err != nil {
		t.Fatalf("SetPinned: %v", err)
	}

	pinned, err := svc.PinnedMessages(ctx, chatID, outsider)
	if err != nil {
		t.Fatalf("PinnedMessages: %v", err)
	}
	if len(pinned) != 0 {
		t.Fatalf("an outsider read %d pinned messages from a chat they are not in", len(pinned))
	}
}

// ------------------------------------------------------- flags and muting

func TestArchivingAndPinningAChatIsPerMember(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	member := createUser(t, db, "member")
	chatID := groupChat(t, db, owner, member)

	yes := true
	if err := svc.SetChatFlags(ctx, chatID, owner, &yes, &yes); err != nil {
		t.Fatalf("SetChatFlags: %v", err)
	}

	var ownerPinned, ownerArchived, memberPinned, memberArchived bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT is_pinned, is_archived FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, owner).Scan(&ownerPinned, &ownerArchived); err != nil {
		t.Fatalf("read owner flags: %v", err)
	}
	if !ownerPinned || !ownerArchived {
		t.Fatal("the owner's own flags were not set")
	}

	if err := db.Pool.QueryRow(ctx,
		`SELECT is_pinned, is_archived FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, member).Scan(&memberPinned, &memberArchived); err != nil {
		t.Fatalf("read member flags: %v", err)
	}
	if memberPinned || memberArchived {
		t.Fatal("one member archiving a group archived it for everyone else too")
	}

	// A nil field must leave the stored value alone: unarchiving is not the
	// same request as unpinning.
	no := false
	if err := svc.SetChatFlags(ctx, chatID, owner, nil, &no); err != nil {
		t.Fatalf("SetChatFlags partial: %v", err)
	}
	if err := db.Pool.QueryRow(ctx,
		`SELECT is_pinned, is_archived FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, owner).Scan(&ownerPinned, &ownerArchived); err != nil {
		t.Fatalf("re-read owner flags: %v", err)
	}
	if !ownerPinned {
		t.Error("unarchiving also unpinned the chat")
	}
	if ownerArchived {
		t.Error("the chat is still archived")
	}
}

func TestMutingAndUnmutingAChat(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	chatID := groupChat(t, db, owner)

	until := time.Now().Add(2 * time.Hour)
	if err := svc.SetMuted(ctx, chatID, owner, &until); err != nil {
		t.Fatalf("SetMuted: %v", err)
	}

	var muted *time.Time
	if err := db.Pool.QueryRow(ctx,
		`SELECT muted_until FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, owner).Scan(&muted); err != nil {
		t.Fatalf("read muted_until: %v", err)
	}
	if muted == nil {
		t.Fatal("the chat was not muted")
	}
	if muted.Sub(until).Abs() > time.Second {
		t.Errorf("muted until %s, want %s", muted, until)
	}

	if err := svc.SetMuted(ctx, chatID, owner, nil); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if err := db.Pool.QueryRow(ctx,
		`SELECT muted_until FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		chatID, owner).Scan(&muted); err != nil {
		t.Fatalf("re-read muted_until: %v", err)
	}
	if muted != nil {
		t.Errorf("the chat is still muted until %s", muted)
	}
}

func TestFlagsAndMuteRefuseNonMembers(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)

	yes := true
	if err := svc.SetChatFlags(ctx, chatID, outsider, &yes, nil); err == nil {
		t.Error("a non-member set flags on a chat")
	}
	until := time.Now().Add(time.Hour)
	if err := svc.SetMuted(ctx, chatID, outsider, &until); err == nil {
		t.Error("a non-member muted a chat")
	}
}

// --------------------------------------------------------------- scheduling

// scheduleAt queues a post directly through the repository, so a test can put
// one in the past — which the service refuses, and rightly.
func scheduleAt(t *testing.T, repo *messaging.Repository, chatID, senderID uuid.UUID, content string, at time.Time) *messaging.ScheduledMessage {
	t.Helper()
	scheduled, err := repo.Schedule(context.Background(), messaging.ScheduleParams{
		ChatID:          chatID,
		SenderID:        senderID,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         content,
		PublishAt:       at,
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	return scheduled
}

// A queued post is not in the conversation: it must not appear in history, and
// it must not move the chat's sequence on.
func TestAScheduledPostIsNotYetInTheConversation(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	reader := createUser(t, db, "reader")
	chatID := groupChat(t, db, author, reader)

	live := send(t, repo, chatID, author, "sent now")
	scheduleAt(t, repo, chatID, author, "sent later", time.Now().Add(time.Hour))

	history, err := repo.History(ctx, chatID, reader, nil, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 || history[0].ID != live.ID {
		t.Fatalf("history holds %d messages, want only the live one", len(history))
	}

	var lastSeq int64
	if err := db.Pool.QueryRow(ctx, `SELECT last_seq FROM chats WHERE id = $1`, chatID).Scan(&lastSeq); err != nil {
		t.Fatalf("read last_seq: %v", err)
	}
	if lastSeq != live.Seq {
		t.Errorf("the chat is at seq %d, want %d — a queued post consumed a sequence number", lastSeq, live.Seq)
	}
}

func TestPublishingTurnsAQueuedPostIntoAnOrdinaryMessage(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	reader := createUser(t, db, "reader")
	chatID := groupChat(t, db, author, reader)

	send(t, repo, chatID, author, "first, live")
	// Already due, so the very next publisher tick takes it.
	queued := scheduleAt(t, repo, chatID, author, "second, queued", time.Now().Add(-time.Minute))

	published, err := svc.PublishDue(ctx, 10)
	if err != nil {
		t.Fatalf("PublishDue: %v", err)
	}
	if published != 1 {
		t.Fatalf("published %d posts, want 1", published)
	}

	history, err := repo.History(ctx, chatID, reader, nil, nil, 50)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history holds %d messages, want 2", len(history))
	}
	// History is newest first, and the published post got the later sequence:
	// order follows when a message became visible, not when it was written.
	if history[0].Content != "second, queued" {
		t.Errorf("newest message is %q, want the published post", history[0].Content)
	}
	if history[0].Seq != 2 {
		t.Errorf("the published post got seq %d, want 2", history[0].Seq)
	}

	// The reader must have been told, exactly as for a live message.
	var events int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM user_events WHERE user_id = $1 AND type = $2`,
		reader, messaging.EventMessageNew).Scan(&events); err != nil {
		t.Fatalf("count reader events: %v", err)
	}
	if events != 2 {
		t.Errorf("the reader has %d new-message events, want 2", events)
	}

	// And the queue row now records which message it became.
	var publishedID *uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`SELECT published_message_id FROM scheduled_messages WHERE id = $1`,
		queued.ID).Scan(&publishedID); err != nil {
		t.Fatalf("read scheduled row: %v", err)
	}
	if publishedID == nil || *publishedID != history[0].ID {
		t.Errorf("the queue row points at %v, want %s", publishedID, history[0].ID)
	}
}

func TestPublishingLeavesFuturePostsAlone(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	scheduleAt(t, repo, chatID, author, "not yet", time.Now().Add(time.Hour))

	published, err := svc.PublishDue(ctx, 10)
	if err != nil {
		t.Fatalf("PublishDue: %v", err)
	}
	if published != 0 {
		t.Fatalf("published %d posts that are not due yet", published)
	}
}

// The publisher can crash between sending and recording the result. The next
// tick re-sends, and the idempotency key must make that the same message
// rather than a second one.
func TestRepublishingDoesNotDuplicateTheMessage(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	queued := scheduleAt(t, repo, chatID, author, "exactly once", time.Now().Add(-time.Minute))

	if _, err := svc.PublishDue(ctx, 10); err != nil {
		t.Fatalf("first PublishDue: %v", err)
	}

	// Undo only the bookkeeping and release the lease, which is exactly what a
	// crash after the send but before the acknowledgement leaves behind once
	// the lease has run out.
	if _, err := db.Pool.Exec(ctx, `
		UPDATE scheduled_messages
		   SET published_at = NULL, published_message_id = NULL, claimed_until = NULL
		 WHERE id = $1`, queued.ID); err != nil {
		t.Fatalf("simulate a crash: %v", err)
	}

	if _, err := svc.PublishDue(ctx, 10); err != nil {
		t.Fatalf("second PublishDue: %v", err)
	}

	var messages int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE chat_id = $1 AND deleted_at IS NULL`,
		chatID).Scan(&messages); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if messages != 1 {
		t.Fatalf("the chat holds %d messages after a repeated publish, want 1", messages)
	}
}

// A post whose author has left the chat can never publish. It must not be
// retried for ever, and it must not disappear without a recorded reason.
func TestAnUnpublishablePostIsAbandonedWithItsReason(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	queued := scheduleAt(t, repo, chatID, author, "no longer allowed", time.Now().Add(-time.Minute))

	if _, err := db.Pool.Exec(ctx,
		`UPDATE chat_members SET left_at = now() WHERE chat_id = $1 AND user_id = $2`,
		chatID, author); err != nil {
		t.Fatalf("remove the author from the chat: %v", err)
	}

	for attempt := 1; attempt <= 6; attempt++ {
		published, err := svc.PublishDue(ctx, 10)
		if err != nil {
			t.Fatalf("PublishDue attempt %d: %v", attempt, err)
		}
		if published != 0 {
			t.Fatalf("attempt %d published a post the author may no longer send", attempt)
		}
		// Expire the claim lease, which is what the passage of time between
		// two publisher ticks does.
		if _, err := db.Pool.Exec(ctx,
			`UPDATE scheduled_messages SET claimed_until = NULL WHERE id = $1`,
			queued.ID); err != nil {
			t.Fatalf("expire the lease: %v", err)
		}
	}

	var (
		lastError   string
		abandonedAt *time.Time
	)
	if err := db.Pool.QueryRow(ctx,
		`SELECT last_error, abandoned_at FROM scheduled_messages WHERE id = $1`,
		queued.ID).Scan(&lastError, &abandonedAt); err != nil {
		t.Fatalf("read the queue row: %v", err)
	}
	if lastError == "" {
		t.Error("the failure was not recorded, so the author cannot be told why")
	}
	if abandonedAt == nil {
		t.Error("the post is still being retried after six failures")
	}

	// Once abandoned it must stop being claimed at all.
	due, err := repo.ClaimDueScheduled(ctx, 10)
	if err != nil {
		t.Fatalf("ClaimDueScheduled: %v", err)
	}
	for _, item := range due {
		if item.ID == queued.ID {
			t.Fatal("an abandoned post was claimed again")
		}
	}
}

func TestReschedulingReplacesRatherThanDuplicates(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	clientID := uuid.New()
	first := time.Now().Add(time.Hour)
	second := time.Now().Add(3 * time.Hour)

	original, err := repo.Schedule(ctx, messaging.ScheduleParams{
		ChatID: chatID, SenderID: author, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "draft one", PublishAt: first,
	})
	if err != nil {
		t.Fatalf("first Schedule: %v", err)
	}

	revised, err := repo.Schedule(ctx, messaging.ScheduleParams{
		ChatID: chatID, SenderID: author, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "draft two", PublishAt: second,
	})
	if err != nil {
		t.Fatalf("second Schedule: %v", err)
	}
	if revised.ID != original.ID {
		t.Fatalf("rescheduling made a second row: %s then %s", original.ID, revised.ID)
	}

	queued, err := repo.ScheduledFor(ctx, chatID, author)
	if err != nil {
		t.Fatalf("ScheduledFor: %v", err)
	}
	if len(queued) != 1 {
		t.Fatalf("%d posts are queued, want 1", len(queued))
	}
	if queued[0].Content != "draft two" {
		t.Errorf("the queued post says %q, want the revised text", queued[0].Content)
	}
	if queued[0].ScheduledAt.Sub(second).Abs() > time.Second {
		t.Errorf("the queued post is due at %s, want %s", queued[0].ScheduledAt, second)
	}
}

// Once a post has gone out it is a message, and messages are edited, not
// rescheduled — otherwise a stale retry would re-send it.
func TestAPublishedPostCannotBeRescheduled(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	clientID := uuid.New()
	if _, err := repo.Schedule(ctx, messaging.ScheduleParams{
		ChatID: chatID, SenderID: author, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "going out", PublishAt: time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if _, err := svc.PublishDue(ctx, 10); err != nil {
		t.Fatalf("PublishDue: %v", err)
	}

	_, err := repo.Schedule(ctx, messaging.ScheduleParams{
		ChatID: chatID, SenderID: author, ClientMessageID: clientID,
		Type: messaging.TypeText, Content: "too late", PublishAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Fatal("a message that had already gone out was rescheduled")
	}
}

func TestCancellingRemovesAQueuedPost(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	stranger := createUser(t, db, "stranger")
	chatID := groupChat(t, db, author)

	queued := scheduleAt(t, repo, chatID, author, "changed my mind", time.Now().Add(time.Hour))

	// Someone else's queue is not yours to empty.
	if err := svc.CancelScheduled(ctx, queued.ID, stranger); err == nil {
		t.Error("a stranger cancelled someone else's scheduled post")
	}

	if err := svc.CancelScheduled(ctx, queued.ID, author); err != nil {
		t.Fatalf("CancelScheduled: %v", err)
	}
	remaining, err := repo.ScheduledFor(ctx, chatID, author)
	if err != nil {
		t.Fatalf("ScheduledFor: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("%d posts remain queued after cancelling", len(remaining))
	}
}

func TestScheduledListIsPrivateToItsAuthor(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	other := createUser(t, db, "other")
	chatID := groupChat(t, db, author, other)

	scheduleAt(t, repo, chatID, author, "my draft", time.Now().Add(time.Hour))

	theirs, err := repo.ScheduledFor(ctx, chatID, other)
	if err != nil {
		t.Fatalf("ScheduledFor: %v", err)
	}
	if len(theirs) != 0 {
		t.Fatalf("another member sees %d of my queued posts", len(theirs))
	}
}

func TestSchedulingRejectsThePastAndTheFarFuture(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	base := messaging.ScheduleParams{
		ChatID: chatID, SenderID: author, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "when?",
	}

	past := base
	past.PublishAt = time.Now().Add(-time.Hour)
	if _, err := svc.Schedule(ctx, past); err == nil {
		t.Error("a post was scheduled in the past")
	}

	tooFar := base
	tooFar.PublishAt = time.Now().AddDate(2, 0, 0)
	if _, err := svc.Schedule(ctx, tooFar); err == nil {
		t.Error("a post was scheduled two years ahead")
	}

	noClientID := base
	noClientID.ClientMessageID = uuid.Nil
	noClientID.PublishAt = time.Now().Add(time.Hour)
	if _, err := svc.Schedule(ctx, noClientID); err == nil {
		t.Error("a post was scheduled with no client_message_id, so a retry would duplicate it")
	}
}

func TestSchedulingRefusesAChatTheCallerCannotPostIn(t *testing.T) {
	db := testDB(t)
	svc := newService(t, db)
	ctx := context.Background()

	owner := createUser(t, db, "owner")
	outsider := createUser(t, db, "outsider")
	chatID := groupChat(t, db, owner)

	if _, err := svc.Schedule(ctx, messaging.ScheduleParams{
		ChatID: chatID, SenderID: outsider, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "let me in",
		PublishAt: time.Now().Add(time.Hour),
	}); err == nil {
		t.Fatal("a non-member queued a post into a chat")
	}
}

// A claimed post must not be offered again while its lease is live. Without
// this, a publisher still working through a batch would be handed its own
// backlog on the next tick, and each re-offer would spend one of the post's
// five attempts — so a publisher that was merely slow would abandon healthy
// posts.
func TestAClaimedPostIsNotOfferedAgainWhileItsLeaseIsLive(t *testing.T) {
	db := testDB(t)
	repo := messaging.NewRepository(db)
	ctx := context.Background()

	author := createUser(t, db, "author")
	chatID := groupChat(t, db, author)

	const posts = 6
	queued := make(map[uuid.UUID]bool, posts)
	for i := 0; i < posts; i++ {
		queued[scheduleAt(t, repo, chatID, author, "batch", time.Now().Add(-time.Minute)).ID] = true
	}

	first, err := repo.ClaimDueScheduled(ctx, posts)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	claimed := make(map[uuid.UUID]bool, posts)
	for _, item := range first {
		if queued[item.ID] {
			claimed[item.ID] = true
		}
	}
	if len(claimed) != posts {
		t.Fatalf("the first pass claimed %d of %d posts", len(claimed), posts)
	}

	second, err := repo.ClaimDueScheduled(ctx, posts)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	for _, item := range second {
		if claimed[item.ID] {
			t.Fatalf("post %s was handed out twice while its lease was live", item.ID)
		}
	}

	// Once the lease runs out the post is retryable again — a publisher that
	// died mid-batch must not strand its work for ever.
	for id := range claimed {
		if _, err := db.Pool.Exec(ctx,
			`UPDATE scheduled_messages SET claimed_until = now() - interval '1 second' WHERE id = $1`,
			id); err != nil {
			t.Fatalf("expire the lease: %v", err)
		}
	}

	third, err := repo.ClaimDueScheduled(ctx, posts)
	if err != nil {
		t.Fatalf("third claim: %v", err)
	}
	recovered := 0
	for _, item := range third {
		if claimed[item.ID] {
			recovered++
		}
	}
	if recovered != posts {
		t.Fatalf("%d of %d posts came back after the lease expired", recovered, posts)
	}
}

// secretChat makes an encrypted two-person chat, as POST /secret/chats does.
func secretChat(t *testing.T, db *database.DB, a, b uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id, member_count) VALUES ('secret', $1, 2) RETURNING id`,
		a).Scan(&chatID); err != nil {
		t.Fatalf("create secret chat: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
		INSERT INTO chat_members (chat_id, user_id, role)
		VALUES ($1, $2, 'member'), ($1, $3, 'member')`, chatID, a, b); err != nil {
		t.Fatalf("add secret chat members: %v", err)
	}
	return chatID
}

func TestPlaintextCannotBeSentIntoASecretChat(t *testing.T) {
	// The failure this prevents is the quiet one: the sender is a member and
	// has every permission, so without the guard the message is written to
	// `messages` in clear, the conversation stops being encrypted, and nothing
	// on either screen says so.
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := secretChat(t, db, alice, bob)

	_, err := service.Send(ctx, messaging.SendInput{
		ChatID:          chatID,
		SenderID:        alice,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         "this must never be stored in clear",
	})
	if err == nil {
		t.Fatal("Send into a secret chat succeeded, want a refusal")
	}

	// The refusal must be the reason, not an accident of some other check.
	var stored int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM messages WHERE chat_id = $1`, chatID).Scan(&stored); err != nil {
		t.Fatalf("count stored messages: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d plaintext messages reached an encrypted chat", stored)
	}
}

func TestForwardingIntoASecretChatIsRefused(t *testing.T) {
	// Forward routes through Send, so this asserts the guard is not bypassed by
	// a second door into the same table.
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	source := groupChat(t, db, alice, bob)
	secret := secretChat(t, db, alice, bob)

	original, err := service.Send(ctx, messaging.SendInput{
		ChatID:          source,
		SenderID:        alice,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         "an ordinary message",
	})
	if err != nil {
		t.Fatalf("seed a message to forward: %v", err)
	}

	if _, err := service.Forward(ctx, messaging.ForwardParams{
		FromChatID: source,
		ToChatID:   secret,
		SenderID:   alice,
		MessageIDs: []uuid.UUID{original.ID},
	}); err == nil {
		t.Fatal("forwarding into a secret chat succeeded, want a refusal")
	}

	var stored int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*)::int FROM messages WHERE chat_id = $1`, secret).Scan(&stored); err != nil {
		t.Fatalf("count stored messages: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d forwarded messages reached an encrypted chat", stored)
	}
}

func TestDraftsAreNotStoredForASecretChat(t *testing.T) {
	// A draft is plaintext in an ordinary server-side column. Storing one would
	// put the beginning of a secret message on the server in clear.
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	secret := secretChat(t, db, alice, bob)
	ordinary := groupChat(t, db, alice, bob)

	if err := service.SetDraft(ctx, secret, alice, "half a secret"); err == nil {
		t.Error("SetDraft on a secret chat succeeded, want a refusal")
	}

	var draft string
	if err := db.Pool.QueryRow(ctx,
		`SELECT draft FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		secret, alice).Scan(&draft); err != nil {
		t.Fatalf("read the draft: %v", err)
	}
	if draft != "" {
		t.Errorf("a draft was stored for an encrypted chat: %q", draft)
	}

	// And an ordinary chat still works, so the guard is not simply breaking
	// drafts for everyone.
	if err := service.SetDraft(ctx, ordinary, alice, "half an ordinary message"); err != nil {
		t.Fatalf("SetDraft on an ordinary chat: %v", err)
	}
	if err := db.Pool.QueryRow(ctx,
		`SELECT draft FROM chat_members WHERE chat_id = $1 AND user_id = $2`,
		ordinary, alice).Scan(&draft); err != nil {
		t.Fatalf("read the ordinary draft: %v", err)
	}
	if draft != "half an ordinary message" {
		t.Errorf("ordinary draft = %q, want it stored", draft)
	}
}

func TestSendingQueuesTheMessageForSearch(t *testing.T) {
	// The search index has a query side that was fully written and a write side
	// that was not, so message search returned nothing at all. This asserts the
	// write side: sending a message queues it for indexing.
	db := testDB(t)
	service, messageBus := newServiceWithBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan bus.Job, 8)
	if err := messageBus.ConsumeJobs(ctx, bus.SubjectJobSearchIndex,
		"search-index-test", 1, func(_ context.Context, job bus.Job) error {
			jobs <- job
			return nil
		}); err != nil {
		t.Fatalf("consume search jobs: %v", err)
	}

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := groupChat(t, db, alice, bob)

	sent, err := service.Send(ctx, messaging.SendInput{
		ChatID:          chatID,
		SenderID:        alice,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         "پیام قابل جست‌وجو",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	select {
	case job := <-jobs:
		var payload struct {
			Index      string         `json:"index"`
			DocumentID string         `json:"document_id"`
			Document   map[string]any `json:"document"`
			Delete     bool           `json:"delete"`
		}
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Fatalf("decode the queued job: %v", err)
		}
		if payload.Index != "messages" {
			t.Errorf("index = %q, want messages", payload.Index)
		}
		if payload.DocumentID != sent.ID.String() {
			t.Errorf("document_id = %q, want %s", payload.DocumentID, sent.ID)
		}
		// The text has to reach the index or there is nothing to match on.
		if payload.Document["content"] != "پیام قابل جست‌وجو" {
			t.Errorf("indexed content = %v, want the message text", payload.Document["content"])
		}
		// Scoping is by chat, so this field is what keeps a search inside the
		// caller's own conversations.
		if payload.Document["chat_id"] != chatID.String() {
			t.Errorf("indexed chat_id = %v, want %s", payload.Document["chat_id"], chatID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no search-index job was queued for a sent message")
	}
}

func TestDeletingAMessageRemovesItFromSearch(t *testing.T) {
	// A deleted message that stays searchable is a deletion that did not happen
	// as far as the person who asked for it is concerned.
	db := testDB(t)
	service, messageBus := newServiceWithBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan bus.Job, 8)
	if err := messageBus.ConsumeJobs(ctx, bus.SubjectJobSearchIndex,
		"search-delete-test", 1, func(_ context.Context, job bus.Job) error {
			jobs <- job
			return nil
		}); err != nil {
		t.Fatalf("consume search jobs: %v", err)
	}

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := groupChat(t, db, alice, bob)

	sent, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "to be deleted",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := service.Delete(ctx, sent.ID, alice); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case job := <-jobs:
			var payload struct {
				DocumentID string `json:"document_id"`
				Delete     bool   `json:"delete"`
			}
			if err := json.Unmarshal(job.Payload, &payload); err != nil {
				t.Fatalf("decode the queued job: %v", err)
			}
			if payload.Delete && payload.DocumentID == sent.ID.String() {
				return
			}
		case <-deadline:
			t.Fatal("no removal was queued for a deleted message")
		}
	}
}

func TestSecretChatMessagesAreNeverQueuedForSearch(t *testing.T) {
	// Belt and braces. Sending into a secret chat is already refused, so this
	// asserts the second guard independently: if that refusal were ever relaxed,
	// the plaintext still must not reach an index.
	db := testDB(t)
	service, messageBus := newServiceWithBus(t, db)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobs := make(chan bus.Job, 8)
	if err := messageBus.ConsumeJobs(ctx, bus.SubjectJobSearchIndex,
		"search-secret-test", 1, func(_ context.Context, job bus.Job) error {
			jobs <- job
			return nil
		}); err != nil {
		t.Fatalf("consume search jobs: %v", err)
	}

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	secret := secretChat(t, db, alice, bob)

	if _, err := service.Send(ctx, messaging.SendInput{
		ChatID: secret, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "never indexed",
	}); err == nil {
		t.Fatal("Send into a secret chat succeeded")
	}

	select {
	case job := <-jobs:
		t.Fatalf("a secret chat produced a search-index job: %s", job.Payload)
	case <-time.After(2 * time.Second):
		// Nothing queued, which is the point.
	}
}

func TestPinningWorksInAOneToOneChat(t *testing.T) {
	// A private chat has no hierarchy: both people join as members, and members
	// were denied pinning because the permission set was written for groups.
	// The result was that nobody at all could pin in a private conversation.
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID, _, err := messaging.NewRepository(db).EnsurePrivateChat(ctx, alice, bob)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}

	sent, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "worth keeping",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := service.SetPinned(ctx, sent.ID, alice, true); err != nil {
		t.Fatalf("the sender could not pin in their own private chat: %v", err)
	}
	// And the other party too — neither of them is an owner, so if pinning
	// depended on rank it would work for nobody.
	if err := service.SetPinned(ctx, sent.ID, bob, false); err != nil {
		t.Fatalf("the other party could not unpin: %v", err)
	}
}

func TestOrdinaryGroupMembersStillCannotPin(t *testing.T) {
	// The fix must not hand every group member the moderator's permissions.
	db := testDB(t)
	service := newService(t, db)
	ctx := context.Background()

	alice := createUser(t, db, "alice")
	bob := createUser(t, db, "bob")
	chatID := groupChat(t, db, alice, bob)

	sent, err := service.Send(ctx, messaging.SendInput{
		ChatID: chatID, SenderID: alice, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: "in a group",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if err := service.SetPinned(ctx, sent.ID, bob, true); err == nil {
		t.Error("an ordinary group member pinned a message, want a refusal")
	}
}
