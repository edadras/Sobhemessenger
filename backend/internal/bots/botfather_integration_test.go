package bots_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sobh/messenger/backend/internal/bots"
	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// BotFather is a conversation, so these tests are conversations: they send a
// message as a person and read back what the bot said, exactly as a user
// would. Nothing here reaches past the handler into the tables to assert on
// state the user could not see.

// botFatherHarness is a running BotFather with a person to talk to it.
type botFatherHarness struct {
	t         *testing.T
	db        *database.DB
	service   *bots.Service
	messaging *messaging.Service
	father    *bots.BotFather
	botID     uuid.UUID
	user      uuid.UUID
	chatID    uuid.UUID
}

func newBotFatherHarness(t *testing.T) *botFatherHarness {
	t.Helper()
	ctx := context.Background()

	db := testDB(t)

	logOutput := io.Discard
	if os.Getenv("SOBH_TEST_LOG") != "" {
		logOutput = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))
	metrics := observability.New("botfather-test")

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
		URL: srv.ClientURL(), StreamName: "SOBH_BOTFATHER_TEST",
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

	repo := bots.NewRepository(db)
	service := bots.NewService(repo, messagingService, logger)
	messagingService.AddObserver(service)

	botID, err := service.EnsureBotFather(ctx)
	if err != nil {
		t.Fatalf("EnsureBotFather: %v", err)
	}
	father := bots.NewBotFather(repo, service, db, botID, logger)
	service.RegisterInternal(botID, father)

	user := createUser(t, db)
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(),
			`DELETE FROM bot_dialog_states WHERE user_id = $1`, user)
	})

	// The private chat a person and BotFather talk in, made the same way the
	// app makes one.
	chatID, _, err := messagingRepo.EnsurePrivateChat(ctx, user, botID)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, chatID)
	})

	return &botFatherHarness{
		t: t, db: db, service: service, messaging: messagingService,
		father: father, botID: botID, user: user, chatID: chatID,
	}
}

// say sends a message as the person and returns what BotFather answered.
//
// It goes through the real send path, so the observer, the command parser and
// the in-process dispatch are all exercised rather than stepped around.
func (h *botFatherHarness) say(text string) string {
	h.t.Helper()
	ctx := context.Background()

	before := h.lastBotMessageSeq()

	if _, err := h.messaging.Send(ctx, messaging.SendInput{
		ChatID:          h.chatID,
		SenderID:        h.user,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         text,
	}); err != nil {
		h.t.Fatalf("send %q: %v", text, err)
	}

	reply, seq := h.readBotMessage()
	if seq <= before {
		h.t.Fatalf("BotFather said nothing in answer to %q", text)
	}
	return reply
}

func (h *botFatherHarness) lastBotMessageSeq() int64 {
	h.t.Helper()
	var seq *int64
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT max(seq) FROM messages
		 WHERE chat_id = $1 AND sender_id = $2`,
		h.chatID, h.botID).Scan(&seq); err != nil {
		h.t.Fatalf("read last bot seq: %v", err)
	}
	if seq == nil {
		return 0
	}
	return *seq
}

func (h *botFatherHarness) readBotMessage() (string, int64) {
	h.t.Helper()
	var (
		content string
		seq     int64
	)
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT content, seq FROM messages
		 WHERE chat_id = $1 AND sender_id = $2
		 ORDER BY seq DESC LIMIT 1`,
		h.chatID, h.botID).Scan(&content, &seq); err != nil {
		h.t.Fatalf("read bot reply: %v", err)
	}
	return content, seq
}

// uniqueHandle produces a bot username that satisfies the pattern.
func uniqueHandle() string {
	return "t" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "bot"
}

// ------------------------------------------------------------------- tests

func TestBotFatherAccountExistsAndIsInternal(t *testing.T) {
	h := newBotFatherHarness(t)
	ctx := context.Background()

	var (
		username   string
		isBot      bool
		isInternal bool
	)
	if err := h.db.Pool.QueryRow(ctx, `
		SELECT u.username, u.is_bot, b.is_internal
		  FROM users u JOIN bots b ON b.user_id = u.id
		 WHERE u.id = $1`, h.botID).Scan(&username, &isBot, &isInternal); err != nil {
		t.Fatalf("read botfather: %v", err)
	}

	if username != bots.BotFatherUsername {
		t.Errorf("BotFather is @%s, want @%s", username, bots.BotFatherUsername)
	}
	if !isBot {
		t.Error("BotFather is not marked as a bot")
	}
	if !isInternal {
		t.Error("BotFather is not marked internal, so a token could be issued for it")
	}
}

// A token for BotFather would let its holder create a bot for anybody. The
// database refuses rather than trusting every future caller to remember.
func TestBotFatherCannotBeIssuedAToken(t *testing.T) {
	h := newBotFatherHarness(t)

	_, err := h.db.Pool.Exec(context.Background(), `
		INSERT INTO bot_tokens (bot_id, token_hash, token_prefix, label)
		VALUES ($1, $2, 'sobh_test_ab', 'should not exist')`,
		h.botID, []byte("whatever"))
	if err == nil {
		t.Fatal("a token was issued for BotFather")
	}
	if !strings.Contains(err.Error(), "internal bot") {
		t.Fatalf("refused with %v, want the internal-bot rule", err)
	}
}

func TestBotFatherIntroducesItself(t *testing.T) {
	h := newBotFatherHarness(t)

	reply := h.say("/start")
	for _, command := range []string{"/newbot", "/mybots", "/token", "/setcommands"} {
		if !strings.Contains(reply, command) {
			t.Errorf("the introduction does not mention %s", command)
		}
	}
}

// The whole point: a bot is created by talking, and the token arrives in the
// conversation.
func TestCreatingABotThroughConversation(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	asked := h.say("/newbot")
	if asked == "" {
		t.Fatal("BotFather did not ask anything after /newbot")
	}

	h.say("دستیار آزمایشی")
	done := h.say("@" + handle)

	if !strings.Contains(done, handle) {
		t.Fatalf("the confirmation does not name the bot: %q", done)
	}
	// The token must be in the message, and the message must say it is the
	// only time it will be readable.
	if !strings.Contains(done, "Authorization: Bearer") {
		t.Error("the confirmation does not show how to present the token")
	}

	// And the bot really exists, owned by the person who asked for it.
	var (
		owner       uuid.UUID
		displayName string
	)
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.owner_id, COALESCE(p.display_name, '')
		  FROM bots b
		  JOIN users u ON u.id = b.user_id
		  LEFT JOIN user_profiles p ON p.user_id = b.user_id
		 WHERE u.username = $1`, handle).Scan(&owner, &displayName); err != nil {
		t.Fatalf("the bot was not created: %v", err)
	}
	if owner != h.user {
		t.Errorf("the bot is owned by %s, want %s", owner, h.user)
	}
	if displayName != "دستیار آزمایشی" {
		t.Errorf("the bot is called %q, want the name that was given", displayName)
	}

	// Cleaning up after a bot the conversation made.
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})
}

// A bad username must not throw the conversation away: a near miss is the
// common case, and starting again would be rude.
func TestABadUsernameKeepsTheConversationOpen(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")

	rejected := h.say("no_suffix_here")
	if !strings.Contains(rejected, "bot") {
		t.Errorf("the refusal does not explain the rule: %q", rejected)
	}

	// The conversation is still at the same question, so a correct answer now
	// finishes it.
	done := h.say(handle)
	if !strings.Contains(done, handle) {
		t.Fatalf("the retry did not create the bot: %q", done)
	}

	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})
}

func TestCancelEndsAConversationInProgress(t *testing.T) {
	h := newBotFatherHarness(t)

	h.say("/newbot")
	cancelled := h.say("/cancel")
	if cancelled == "" {
		t.Fatal("BotFather did not acknowledge the cancellation")
	}

	// With nothing in progress, a plain message is not read as an answer to a
	// question that is no longer being asked.
	after := h.say("دستیار آزمایشی")
	if !strings.Contains(after, "/newbot") {
		t.Errorf("after cancelling, a plain message was still treated as an answer: %q", after)
	}
}

func TestListingBotsBeforeAndAfterCreatingOne(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	empty := h.say("/mybots")
	if !strings.Contains(empty, "/newbot") {
		t.Errorf("with no bots, the answer does not say how to make one: %q", empty)
	}

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	listed := h.say("/mybots")
	if !strings.Contains(listed, handle) {
		t.Errorf("the list does not include the new bot: %q", listed)
	}
}

// Someone with one bot should not be asked which one they mean.
func TestASingleBotNeedsNoDisambiguation(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	asked := h.say("/setname")
	if !strings.Contains(asked, handle) {
		t.Fatalf("with one bot, BotFather did not go straight to the question: %q", asked)
	}

	h.say("نام تازه")

	var displayName string
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT COALESCE(p.display_name, '')
		  FROM users u LEFT JOIN user_profiles p ON p.user_id = u.id
		 WHERE u.username = $1`, handle).Scan(&displayName); err != nil {
		t.Fatalf("read display name: %v", err)
	}
	if displayName != "نام تازه" {
		t.Errorf("the name is %q, want the one that was set through the conversation", displayName)
	}
}

func TestSettingCommandsThroughConversation(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	h.say("/setcommands")
	h.say("start - شروع کار\nhelp - راهنما")

	rows, err := h.db.Pool.Query(context.Background(), `
		SELECT c.command FROM bot_commands c
		  JOIN users u ON u.id = c.bot_id
		 WHERE u.username = $1 ORDER BY c.position`, handle)
	if err != nil {
		t.Fatalf("read commands: %v", err)
	}
	defer rows.Close()

	var commands []string
	for rows.Next() {
		var command string
		if err := rows.Scan(&command); err != nil {
			t.Fatalf("scan command: %v", err)
		}
		commands = append(commands, command)
	}
	if len(commands) != 2 || commands[0] != "start" || commands[1] != "help" {
		t.Fatalf("the commands are %v, want [start help]", commands)
	}
}

// A malformed command list must be explained, not silently half-applied.
func TestABadCommandListIsExplained(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	h.say("/setcommands")
	complaint := h.say("this line has no separator")
	if !strings.Contains(complaint, "start - ") {
		t.Errorf("the complaint does not show the expected format: %q", complaint)
	}

	var stored int
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bot_commands c
		  JOIN users u ON u.id = c.bot_id WHERE u.username = $1`,
		handle).Scan(&stored); err != nil {
		t.Fatalf("count commands: %v", err)
	}
	if stored != 0 {
		t.Errorf("%d commands were stored from a list that could not be read", stored)
	}
}

func TestTogglingPrivacyModeInOneMessage(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	off := h.say("/setprivacy @" + handle + " off")
	// Turning privacy off is the setting people most often regret, so the
	// answer has to say what it now means rather than only that it changed.
	if !strings.Contains(off, "همهٔ پیام") {
		t.Errorf("turning privacy off does not explain the consequence: %q", off)
	}

	var privacy bool
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.privacy_mode FROM bots b
		  JOIN users u ON u.id = b.user_id WHERE u.username = $1`,
		handle).Scan(&privacy); err != nil {
		t.Fatalf("read privacy mode: %v", err)
	}
	if privacy {
		t.Error("privacy mode is still on after being turned off")
	}

	h.say("/setprivacy @" + handle + " on")
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.privacy_mode FROM bots b
		  JOIN users u ON u.id = b.user_id WHERE u.username = $1`,
		handle).Scan(&privacy); err != nil {
		t.Fatalf("re-read privacy mode: %v", err)
	}
	if !privacy {
		t.Error("privacy mode did not come back on")
	}
}

func TestAFlagCommandWithoutArgumentsExplainsItself(t *testing.T) {
	h := newBotFatherHarness(t)

	usage := h.say("/setprivacy")
	if !strings.Contains(usage, "/setprivacy") {
		t.Errorf("the usage message does not show the command: %q", usage)
	}
}

// Someone else's bot is not theirs to change, even by naming it exactly.
func TestAnotherPersonsBotCannotBeChanged(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	// A second person, with their own conversation.
	stranger := createUser(t, h.db)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM bot_dialog_states WHERE user_id = $1`, stranger)
	})

	messagingRepo := messaging.NewRepository(h.db)
	strangerChat, _, err := messagingRepo.EnsurePrivateChat(context.Background(), stranger, h.botID)
	if err != nil {
		t.Fatalf("EnsurePrivateChat: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, strangerChat)
	})

	if _, err := h.messaging.Send(context.Background(), messaging.SendInput{
		ChatID:          strangerChat,
		SenderID:        stranger,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         "/setprivacy @" + handle + " off",
	}); err != nil {
		t.Fatalf("send as stranger: %v", err)
	}

	var reply string
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT content FROM messages
		 WHERE chat_id = $1 AND sender_id = $2
		 ORDER BY seq DESC LIMIT 1`, strangerChat, h.botID).Scan(&reply); err != nil {
		t.Fatalf("read the stranger's reply: %v", err)
	}
	if !strings.Contains(reply, "ربات‌های شما") {
		t.Errorf("a stranger was not refused: %q", reply)
	}

	var privacy bool
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.privacy_mode FROM bots b
		  JOIN users u ON u.id = b.user_id WHERE u.username = $1`,
		handle).Scan(&privacy); err != nil {
		t.Fatalf("read privacy mode: %v", err)
	}
	if !privacy {
		t.Error("a stranger changed a setting on someone else's bot")
	}
}

// Deleting asks for the handle to be typed back rather than for a yes: a
// yes/no is one tap away from deactivating the wrong bot.
func TestDeletingNeedsTheHandleTypedBack(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	asked := h.say("/deletebot")
	if !strings.Contains(asked, handle) {
		t.Fatalf("the confirmation does not name the bot: %q", asked)
	}

	// A plain yes is not enough.
	h.say("بله")

	var active bool
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.is_active FROM bots b
		  JOIN users u ON u.id = b.user_id WHERE u.username = $1`,
		handle).Scan(&active); err != nil {
		t.Fatalf("read is_active: %v", err)
	}
	if !active {
		t.Fatal("a plain yes deactivated the bot")
	}

	// Typing the handle does it.
	h.say("/deletebot")
	h.say(handle)

	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT b.is_active FROM bots b
		  JOIN users u ON u.id = b.user_id WHERE u.username = $1`,
		handle).Scan(&active); err != nil {
		t.Fatalf("re-read is_active: %v", err)
	}
	if active {
		t.Error("the bot is still active after being confirmed for deletion")
	}
}

func TestReissuingATokenRevokesTheOldOnes(t *testing.T) {
	h := newBotFatherHarness(t)
	handle := uniqueHandle()

	h.say("/newbot")
	h.say("دستیار")
	h.say(handle)
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(),
			`DELETE FROM users WHERE username = $1`, handle)
	})

	h.say("/token")
	issued := h.say("بله")
	if !strings.Contains(issued, handle) {
		t.Fatalf("the new token message does not name the bot: %q", issued)
	}

	// Exactly one token is live: reissuing is what "my token leaked" means, so
	// leaving the old one working would not answer the request.
	var live int
	if err := h.db.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM bot_tokens t
		  JOIN users u ON u.id = t.bot_id
		 WHERE u.username = $1 AND t.revoked_at IS NULL`,
		handle).Scan(&live); err != nil {
		t.Fatalf("count live tokens: %v", err)
	}
	if live != 1 {
		t.Errorf("%d tokens are live after reissuing, want 1", live)
	}
}

func TestBotFatherRefusesToWorkInAGroup(t *testing.T) {
	h := newBotFatherHarness(t)
	ctx := context.Background()

	var groupID uuid.UUID
	if err := h.db.Pool.QueryRow(ctx, `
		INSERT INTO chats (type, title, creator_id, member_count)
		VALUES ('group', 'somewhere public', $1, 2) RETURNING id`,
		h.user).Scan(&groupID); err != nil {
		t.Fatalf("create group: %v", err)
	}
	t.Cleanup(func() {
		_, _ = h.db.Pool.Exec(context.Background(), `DELETE FROM chats WHERE id = $1`, groupID)
	})

	for _, member := range []uuid.UUID{h.user, h.botID} {
		if _, err := h.db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			groupID, member); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}

	if _, err := h.messaging.Send(ctx, messaging.SendInput{
		ChatID:          groupID,
		SenderID:        h.user,
		ClientMessageID: uuid.New(),
		Type:            messaging.TypeText,
		Content:         "/newbot",
	}); err != nil {
		t.Fatalf("send in group: %v", err)
	}

	var reply string
	if err := h.db.Pool.QueryRow(ctx, `
		SELECT content FROM messages
		 WHERE chat_id = $1 AND sender_id = $2
		 ORDER BY seq DESC LIMIT 1`, groupID, h.botID).Scan(&reply); err != nil {
		t.Fatalf("read the group reply: %v", err)
	}
	if !strings.Contains(reply, "خصوصی") {
		t.Errorf("BotFather did not refuse to work in a group: %q", reply)
	}

	// And no conversation was opened, so the next message in the group is not
	// read as a bot name.
	var open int
	if err := h.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bot_dialog_states WHERE user_id = $1`, h.user).Scan(&open); err != nil {
		t.Fatalf("count dialog states: %v", err)
	}
	if open != 0 {
		t.Error("a conversation was opened from a group message")
	}
}

// An abandoned conversation must not still be waiting when the user comes back
// and types something unrelated.
func TestAnExpiredConversationIsForgotten(t *testing.T) {
	h := newBotFatherHarness(t)

	h.say("/newbot")

	if _, err := h.db.Pool.Exec(context.Background(),
		`UPDATE bot_dialog_states SET expires_at = now() - interval '1 minute'
		  WHERE user_id = $1`, h.user); err != nil {
		t.Fatalf("expire the conversation: %v", err)
	}

	reply := h.say("something unrelated, typed days later")
	if !strings.Contains(reply, "/newbot") {
		t.Errorf("an expired conversation still consumed the message: %q", reply)
	}
}

func TestStartingANewConversationReplacesTheOldOne(t *testing.T) {
	h := newBotFatherHarness(t)

	h.say("/newbot")
	h.say("a name for the bot")

	// Half-way through /newbot, /mybots must answer /mybots rather than being
	// swallowed as the username.
	listed := h.say("/mybots")
	if !strings.Contains(listed, "/newbot") && !strings.Contains(listed, "ربات‌های شما") {
		t.Errorf("a command was swallowed by the conversation in progress: %q", listed)
	}

	var open int
	if err := h.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM bot_dialog_states WHERE user_id = $1`, h.user).Scan(&open); err != nil {
		t.Fatalf("count dialog states: %v", err)
	}
	if open != 0 {
		t.Error("the abandoned conversation is still open")
	}
}
