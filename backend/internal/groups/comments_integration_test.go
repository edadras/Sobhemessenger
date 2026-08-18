package groups_test

import (
	"context"
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
	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Channel comments, the linked discussion group, post signatures, the group
// sticker set and broadcast mode (§15).

// commentHarness assembles the real services these features need. Posting a
// comment is an ordinary send, so the messaging service has to be the real one
// with its real bus and rate limiter: a stub would prove nothing about whether
// a comment is a message.
type commentHarness struct {
	db        *database.DB
	repo      *groups.Repository
	service   *groups.Service
	messaging *messaging.Repository
	sender    *messaging.Service
}

func newCommentHarness(t *testing.T) *commentHarness {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()

	logOutput := io.Discard
	if os.Getenv("SOBH_TEST_LOG") != "" {
		logOutput = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metrics := observability.New("groups-test")

	srv, err := natsserver.NewServer(&natsserver.Options{
		Host: "127.0.0.1", Port: -1, JetStream: true,
		StoreDir: t.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		t.Fatalf("create nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(srv.Shutdown)

	messageBus, err := bus.Connect(config.NATS{
		URL: srv.ClientURL(), StreamName: "SOBH_COMMENTS_TEST",
		MaxReconnects: 3, ReconnectWait: time.Second,
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

	rules := ratelimit.NewRules(config.RateLimits{
		OTPPerPhonePerHour: 10000, OTPPerIPPerHour: 10000, LoginPerIPPerHour: 10000,
		APIPerUserPerMin: 10000, APIPerIPPerMin: 10000,
		MessagesPerMin: 10000, UploadsPerHour: 10000,
	})

	messagingRepo := messaging.NewRepository(db)
	messagingService := messaging.NewService(messagingRepo, messageBus,
		ratelimit.New(cacheClient, metrics), rules, metrics, logger)

	repo := groups.NewRepository(db)
	cfg := &config.Config{}
	service := groups.NewService(repo, messagingRepo, messagingService, messageBus, cfg, logger)
	// Production registers the groups service as an observer so a channel post
	// is mirrored as it goes out. A harness that skipped this would only ever
	// exercise the lazy path.
	messagingService.AddObserver(service)

	return &commentHarness{
		db: db, repo: repo, service: service,
		messaging: messagingRepo, sender: messagingService,
	}
}

func (h *commentHarness) chat(t *testing.T, chatType, title string, owner uuid.UUID, public bool) uuid.UUID {
	t.Helper()
	var username *string
	if public {
		handle := "c" + uuid.NewString()[:8]
		username = &handle
	}
	chatID, err := h.repo.CreateChat(context.Background(), chatType, title, "", owner, public, username)
	if err != nil {
		t.Fatalf("CreateChat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})
	return chatID
}

func (h *commentHarness) post(t *testing.T, chatID, senderID uuid.UUID, content string) *messaging.Message {
	t.Helper()
	result, err := h.messaging.Send(context.Background(), messaging.SendParams{
		ChatID: chatID, SenderID: senderID, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: content,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return result.Message
}

// postThroughService sends the way the API does, so the observers that make a
// post's comment thread actually run.
func (h *commentHarness) postThroughService(t *testing.T, chatID, senderID uuid.UUID, content string) *messaging.Message {
	t.Helper()
	message, err := h.sender.Send(context.Background(), messaging.SendInput{
		ChatID: chatID, SenderID: senderID, ClientMessageID: uuid.New(),
		Type: messaging.TypeText, Content: content,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return message
}

// ------------------------------------------------------------- signatures

func TestChannelPostsAreUnsignedUntilSignaturesAreOn(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "editor")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)

	unsigned := h.post(t, channelID, owner, "before")
	if unsigned.AuthorSignature != "" {
		t.Errorf("post is signed %q with signatures off", unsigned.AuthorSignature)
	}

	on := true
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		SlowModeSeconds: 0, HistoryVisibleToNew: true, MaxMembers: 200000,
		SignatureEnabled: &on,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	signed := h.post(t, channelID, owner, "after")
	if signed.AuthorSignature != "editor" {
		t.Errorf("signature = %q, want the admin's display name", signed.AuthorSignature)
	}

	// The earlier post keeps its unsigned state: switching signatures on is
	// not retroactive, because nobody agreed to be named on it.
	history, err := h.messaging.History(ctx, messaging.HistoryQuery{
		ChatID: channelID, ViewerID: owner, Limit: 10,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, message := range history {
		if message.ID == unsigned.ID && message.AuthorSignature != "" {
			t.Errorf("an older post gained the signature %q", message.AuthorSignature)
		}
	}
}

func TestASignatureSurvivesTheAdminBeingRenamed(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "first name")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)

	on := true
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, SignatureEnabled: &on,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	post := h.post(t, channelID, owner, "attributed")

	if _, err := h.db.Pool.Exec(ctx,
		`UPDATE user_profiles SET display_name = 'second name' WHERE user_id = $1`,
		owner); err != nil {
		t.Fatalf("rename: %v", err)
	}

	history, err := h.messaging.History(ctx, messaging.HistoryQuery{
		ChatID: channelID, ViewerID: owner, Limit: 10,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	for _, message := range history {
		if message.ID == post.ID && message.AuthorSignature != "first name" {
			t.Errorf("signature = %q; a rename must not rewrite an old attribution",
				message.AuthorSignature)
		}
	}
}

func TestACustomTitleIsWhatTheSignatureShows(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	writer := createUser(t, h.db, "writer")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)

	if err := h.repo.AddMember(ctx, channelID, writer, messaging.RoleAdmin, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := h.repo.SetRole(ctx, channelID, writer, messaging.RoleAdmin, nil, "Chief Editor"); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	on := true
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, SignatureEnabled: &on,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	post := h.post(t, channelID, writer, "by the chief")
	if post.AuthorSignature != "Chief Editor" {
		t.Errorf("signature = %q, want the custom title", post.AuthorSignature)
	}
}

func TestAnOrdinaryMemberNeverSignsAPost(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	// A group, where members do post, is the case that matters: signatures
	// belong to channel staff and nothing else must pick them up.
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	post := h.post(t, chatID, member, "hello")
	if post.AuthorSignature != "" {
		t.Errorf("a group message was signed %q", post.AuthorSignature)
	}
}

// -------------------------------------------------------------- comments

func (h *commentHarness) linkedChannel(t *testing.T, owner uuid.UUID) (channelID, groupID uuid.UUID) {
	t.Helper()
	channelID = h.chat(t, messaging.ChatChannel, "Newsroom", owner, true)
	groupID = h.chat(t, messaging.ChatGroup, "Newsroom chat", owner, false)
	if err := h.service.LinkDiscussion(context.Background(), channelID, groupID, owner); err != nil {
		t.Fatalf("LinkDiscussion: %v", err)
	}
	return channelID, groupID
}

func TestLinkingADiscussionGroupPointsBothWays(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID, groupID := h.linkedChannel(t, owner)

	channelSettings, err := h.repo.Settings(ctx, channelID)
	if err != nil {
		t.Fatalf("channel settings: %v", err)
	}
	if channelSettings.DiscussionChatID == nil || *channelSettings.DiscussionChatID != groupID {
		t.Errorf("channel points at %v, want %s", channelSettings.DiscussionChatID, groupID)
	}

	groupSettings, err := h.repo.Settings(ctx, groupID)
	if err != nil {
		t.Fatalf("group settings: %v", err)
	}
	if groupSettings.LinkedChannelID == nil || *groupSettings.LinkedChannelID != channelID {
		t.Errorf("group points at %v, want %s", groupSettings.LinkedChannelID, channelID)
	}

	// A settings read for the wrong type reports nothing rather than a
	// misleading default: a group has no signature setting to report.
	if groupSettings.SignatureEnabled != nil {
		t.Error("a group reported a channel-only setting")
	}
	if channelSettings.StickerSet != nil {
		t.Error("a channel reported a group-only setting")
	}
}

func TestADiscussionGroupCannotBeLinkedTwice(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	_, groupID := h.linkedChannel(t, owner)
	otherChannel := h.chat(t, messaging.ChatChannel, "Second", owner, false)

	if err := h.service.LinkDiscussion(ctx, otherChannel, groupID, owner); err == nil {
		t.Fatal("one group was linked to two channels")
	}
}

func TestOnlyAChannelGetsADiscussionGroup(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	groupA := h.chat(t, messaging.ChatGroup, "A", owner, false)
	groupB := h.chat(t, messaging.ChatGroup, "B", owner, false)

	if err := h.service.LinkDiscussion(ctx, groupA, groupB, owner); err == nil {
		t.Fatal("a group was given a discussion group")
	}
}

func TestAPostIsMirroredIntoTheDiscussionGroupAsItGoesOut(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID, groupID := h.linkedChannel(t, owner)

	// Sent through the service, because the mirror is made by the observer
	// the service notifies — the repository knows nothing about it.
	post := h.postThroughService(t, channelID, owner, "the announcement")

	thread, err := h.repo.CommentThread(ctx, post.ID)
	if err != nil {
		t.Fatalf("no thread was created for the post: %v", err)
	}
	if thread.DiscussionChatID != groupID {
		t.Errorf("thread is in chat %s, want the discussion group %s",
			thread.DiscussionChatID, groupID)
	}

	mirrored, err := h.messaging.History(ctx, messaging.HistoryQuery{
		ChatID: groupID, ViewerID: owner, Limit: 10,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	var found *messaging.Message
	for i := range mirrored {
		if mirrored[i].ID == thread.RootMessageID {
			found = &mirrored[i]
		}
	}
	if found == nil {
		t.Fatal("the mirror is not in the discussion group's history")
	}
	if found.Content != "the announcement" {
		t.Errorf("mirror content = %q", found.Content)
	}
	// Posted by the chat, not by the admin: the mirror must not reveal an
	// authorship the channel has not chosen to publish.
	if found.SenderID != nil {
		t.Errorf("the mirror names a sender (%s); it is posted by the channel", found.SenderID)
	}
	if found.ForwardFrom == nil || found.ForwardFrom.ChatID == nil || *found.ForwardFrom.ChatID != channelID {
		t.Error("the mirror does not say which channel it came from")
	}
}

func TestCommentingOnAPostJoinsTheDiscussionGroup(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	reader := createUser(t, h.db, "reader")
	channelID, groupID := h.linkedChannel(t, owner)
	if err := h.repo.AddMember(ctx, channelID, reader, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	post := h.postThroughService(t, channelID, owner, "what do you think?")

	comment, err := h.service.Comment(ctx, channelID, post.ID, reader, messaging.SendInput{
		ClientMessageID: uuid.New(), Type: messaging.TypeText, Content: "I think so",
	})
	if err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if comment.ChatID != groupID {
		t.Errorf("the comment landed in %s, want the discussion group", comment.ChatID)
	}

	memberCtx, err := h.messaging.ChatContextFor(ctx, groupID, reader)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !memberCtx.IsMember {
		t.Error("commenting did not join the reader to the discussion group")
	}

	thread, comments, err := h.service.Comments(ctx, channelID, post.ID, reader, nil, 50)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 || comments[0].ID != comment.ID {
		t.Errorf("the thread lists %d comments, want the one just posted", len(comments))
	}
	if thread.CommentCount != 1 {
		t.Errorf("comment count = %d, want 1", thread.CommentCount)
	}
}

func TestOnlyRepliesToThePostCountAsItsComments(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID, groupID := h.linkedChannel(t, owner)
	post := h.postThroughService(t, channelID, owner, "the announcement")

	if _, err := h.service.Comment(ctx, channelID, post.ID, owner, messaging.SendInput{
		ClientMessageID: uuid.New(), Type: messaging.TypeText, Content: "a comment",
	}); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	// An unrelated message in the same group is not a comment on the post.
	h.post(t, groupID, owner, "unrelated chatter")

	_, comments, err := h.service.Comments(ctx, channelID, post.ID, owner, nil, 50)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if len(comments) != 1 {
		t.Errorf("the thread lists %d comments; group chatter is not a comment", len(comments))
	}
}

func TestCommentsAreRefusedWhenTheyAreSwitchedOff(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID, _ := h.linkedChannel(t, owner)

	off := false
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, CommentsEnabled: &off,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	post := h.postThroughService(t, channelID, owner, "no comments please")
	if _, _, err := h.service.Comments(ctx, channelID, post.ID, owner, nil, 50); err == nil {
		t.Fatal("comments were served on a channel that has them switched off")
	}
}

func TestAPostMadeBeforeTheLinkStillGetsAThread(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)
	post := h.postThroughService(t, channelID, owner, "posted before the group existed")

	if _, err := h.repo.CommentThread(ctx, post.ID); err == nil {
		t.Fatal("a thread existed before there was anywhere to put it")
	}

	groupID := h.chat(t, messaging.ChatGroup, "Newsroom chat", owner, false)
	if err := h.service.LinkDiscussion(ctx, channelID, groupID, owner); err != nil {
		t.Fatalf("LinkDiscussion: %v", err)
	}

	// Opening the comments is what creates the mirror for a post that
	// predates the link.
	thread, _, err := h.service.Comments(ctx, channelID, post.ID, owner, nil, 50)
	if err != nil {
		t.Fatalf("Comments: %v", err)
	}
	if thread.DiscussionChatID != groupID {
		t.Errorf("thread is in %s, want %s", thread.DiscussionChatID, groupID)
	}
}

func TestUnlinkingLeavesTheRepliesAlone(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID, groupID := h.linkedChannel(t, owner)
	post := h.postThroughService(t, channelID, owner, "the announcement")
	if _, err := h.service.Comment(ctx, channelID, post.ID, owner, messaging.SendInput{
		ClientMessageID: uuid.New(), Type: messaging.TypeText, Content: "a reply",
	}); err != nil {
		t.Fatalf("Comment: %v", err)
	}

	if err := h.service.UnlinkDiscussion(ctx, channelID, owner); err != nil {
		t.Fatalf("UnlinkDiscussion: %v", err)
	}

	// The conversation is a real conversation in a real group; detaching the
	// channel must not destroy other people's replies.
	remaining, err := h.messaging.History(ctx, messaging.HistoryQuery{
		ChatID: groupID, ViewerID: owner, Limit: 50,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(remaining) < 2 {
		t.Errorf("the discussion group holds %d messages after unlinking; the replies are gone", len(remaining))
	}
}

// ------------------------------------------------ sticker set and broadcast

func TestAGroupStickerSetHasToNameOneThatExists(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	chatID := h.chat(t, messaging.ChatGroup, "Team", owner, false)

	missing := "no_such_set"
	err := h.repo.UpdateSettings(ctx, chatID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, StickerSet: &missing,
	})
	if err == nil {
		t.Fatal("a group was given a sticker set that does not exist")
	}

	var setID uuid.UUID
	if err := h.db.Pool.QueryRow(ctx, `
		INSERT INTO sticker_sets (slug, title, kind) VALUES ($1, 'Test set', 'static')
		RETURNING id`, "test_set_"+uuid.NewString()[:6]).Scan(&setID); err != nil {
		t.Fatalf("create sticker set: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(), `DELETE FROM sticker_sets WHERE id = $1`, setID)
	})
	var slug string
	if err := h.db.Pool.QueryRow(ctx,
		`SELECT slug FROM sticker_sets WHERE id = $1`, setID).Scan(&slug); err != nil {
		t.Fatalf("read slug: %v", err)
	}

	if err := h.repo.UpdateSettings(ctx, chatID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, StickerSet: &slug,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	settings, err := h.repo.Settings(ctx, chatID)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if settings.StickerSet == nil || *settings.StickerSet != slug {
		t.Errorf("sticker set = %v, want %q", settings.StickerSet, slug)
	}
}

func TestAnOmittedSettingIsLeftAlone(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	channelID := h.chat(t, messaging.ChatChannel, "Newsroom", owner, false)

	on := true
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, SignatureEnabled: &on,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// A second update that says nothing about signatures — an older client
	// that has never heard of them — must not switch them off.
	if err := h.repo.UpdateSettings(ctx, channelID, groups.Settings{
		SlowModeSeconds: 30, HistoryVisibleToNew: true, MaxMembers: 200000,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	settings, err := h.repo.Settings(ctx, channelID)
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if settings.SignatureEnabled == nil || !*settings.SignatureEnabled {
		t.Error("an update that never mentioned signatures switched them off")
	}
}

func TestBroadcastModeSilencesMembersButNotStaff(t *testing.T) {
	h := newCommentHarness(t)
	ctx := context.Background()

	owner := createUser(t, h.db, "owner")
	member := createUser(t, h.db, "member")
	chatID := h.chat(t, messaging.ChatGroup, "Announcements", owner, false)
	if err := h.repo.AddMember(ctx, chatID, member, messaging.RoleMember, &owner); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	on := true
	if err := h.repo.UpdateSettings(ctx, chatID, groups.Settings{
		HistoryVisibleToNew: true, MaxMembers: 200000, IsBroadcast: &on,
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	memberCtx, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if memberCtx.Permissions.SendMessages {
		t.Error("an ordinary member may still post in a broadcast group")
	}

	ownerCtx, err := h.messaging.ChatContextFor(ctx, chatID, owner)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !ownerCtx.Permissions.SendMessages {
		t.Error("broadcast mode silenced the owner, who would then run a chat nobody can post in")
	}

	// A personal grant still outranks it, which is the point of layering it
	// under the per-member overrides.
	if err := h.repo.SetRole(ctx, chatID, member, messaging.RoleMember,
		map[string]bool{"send_messages": true}, ""); err != nil {
		t.Fatalf("SetRole: %v", err)
	}
	grantedCtx, err := h.messaging.ChatContextFor(ctx, chatID, member)
	if err != nil {
		t.Fatalf("ChatContextFor: %v", err)
	}
	if !grantedCtx.Permissions.SendMessages {
		t.Error("a personal grant did not override broadcast mode")
	}
}
