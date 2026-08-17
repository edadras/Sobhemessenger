package bots_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bots"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
)

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

func createUser(t *testing.T, db *database.DB) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	phone := "+9891" + uuid.NewString()[:9]
	var id uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
		phone, []byte(uuid.NewString())).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, id)
	})
	return id
}

// uniqueBotName keeps parallel runs from colliding on the username, which is
// globally unique.
func uniqueBotName() string {
	return "t" + strings.ReplaceAll(uuid.NewString()[:8], "-", "") + "bot"
}

func registerBot(t *testing.T, repo *bots.Repository, owner uuid.UUID) (*bots.Bot, *bots.IssuedToken) {
	t.Helper()

	bot, token, err := repo.Create(context.Background(), bots.CreateParams{
		OwnerID:     owner,
		Username:    uniqueBotName(),
		DisplayName: "Test Bot",
	})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	// The bot's account cascades from the owner's, which the user helper
	// already cleans up.
	return bot, token
}

func TestRegisteredBotIsAnAccount(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db)
	bot, token, err := repo.Create(ctx, bots.CreateParams{
		OwnerID:     owner,
		Username:    uniqueBotName(),
		DisplayName: "Helper",
	})
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}

	// A bot must be a real row in users with is_bot set, so every table that
	// references a user works for it unchanged.
	var isBot bool
	if err := db.Pool.QueryRow(ctx,
		`SELECT is_bot FROM users WHERE id = $1`, bot.UserID).Scan(&isBot); err != nil {
		t.Fatalf("read bot account: %v", err)
	}
	if !isBot {
		t.Error("the bot's account does not have is_bot set")
	}

	// The counter row is what every write path expects to exist.
	var counters int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM user_event_counters WHERE user_id = $1`,
		bot.UserID).Scan(&counters); err != nil {
		t.Fatalf("read event counter: %v", err)
	}
	if counters != 1 {
		t.Error("the bot has no event counter row")
	}

	if token.Secret == "" {
		t.Fatal("registration returned no token")
	}
	if !strings.Contains(token.Secret, ":") {
		t.Errorf("token %q is not in the <name>:<secret> form", token.Secret)
	}
}

// The plaintext token must never be recoverable from the database.
func TestTokenIsStoredOnlyAsAHash(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	_, token := registerBot(t, repo, createUser(t, db))

	var stored []byte
	if err := db.Pool.QueryRow(ctx,
		`SELECT token_hash FROM bot_tokens WHERE id = $1`, token.ID).Scan(&stored); err != nil {
		t.Fatalf("read token row: %v", err)
	}
	if strings.Contains(string(stored), token.Secret) {
		t.Error("the plaintext token is recoverable from the database")
	}

	var matches int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bot_tokens WHERE token_prefix = $1`,
		token.Secret).Scan(&matches); err != nil {
		t.Fatalf("search by plaintext: %v", err)
	}
	if matches != 0 {
		t.Error("the plaintext token is stored in the prefix column")
	}
}

func TestAuthenticateResolvesTheBot(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, token := registerBot(t, repo, createUser(t, db))

	authenticated, err := repo.Authenticate(ctx, token.Secret)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if authenticated.UserID != bot.UserID {
		t.Errorf("authenticated as %s, want %s", authenticated.UserID, bot.UserID)
	}

	if _, err := repo.Authenticate(ctx, "not-a-real-token"); err == nil {
		t.Error("a forged token authenticated")
	}
}

func TestRevokedTokenStopsWorking(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, token := registerBot(t, repo, createUser(t, db))

	if err := repo.RevokeToken(ctx, bot.UserID, token.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := repo.Authenticate(ctx, token.Secret); err == nil {
		t.Error("a revoked token still authenticates")
	}
}

// Rotation must be able to overlap, or every rotation is an outage.
func TestTwoTokensCanBeLiveAtOnce(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, first := registerBot(t, repo, createUser(t, db))

	second, err := repo.IssueToken(ctx, bot.UserID, "rotation")
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	for _, token := range []*bots.IssuedToken{first, second} {
		if _, err := repo.Authenticate(ctx, token.Secret); err != nil {
			t.Errorf("token %s does not authenticate during rotation: %v", token.Prefix, err)
		}
	}

	// Retiring the old one leaves the new one working.
	if err := repo.RevokeToken(ctx, bot.UserID, first.ID); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := repo.Authenticate(ctx, second.Secret); err != nil {
		t.Errorf("the new token stopped working after retiring the old one: %v", err)
	}
}

func TestDeactivatedBotCannotAuthenticate(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, token := registerBot(t, repo, createUser(t, db))

	inactive := false
	if err := repo.UpdateSettings(ctx, bot.UserID, bots.Settings{IsActive: &inactive}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	if _, err := repo.Authenticate(ctx, token.Secret); err == nil {
		t.Error("a deactivated bot still authenticates")
	}
}

func TestUpdateQueueIsPolledInOrderAndConfirmed(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, _ := registerBot(t, repo, createUser(t, db))

	for i := 0; i < 3; i++ {
		if err := repo.Enqueue(ctx, bot.UserID, "message",
			map[string]any{"n": i}); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	updates, err := repo.Poll(ctx, bot.UserID, 0, 10)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(updates) != 3 {
		t.Fatalf("polled %d updates, want 3", len(updates))
	}
	if updates[0].ID >= updates[1].ID {
		t.Error("updates are not in ascending id order")
	}

	// Asking for updates after the last id confirms everything up to it,
	// which is how a polling client acknowledges.
	remaining, err := repo.Poll(ctx, bot.UserID, updates[2].ID, 10)
	if err != nil {
		t.Fatalf("Poll (after offset): %v", err)
	}
	if len(remaining) != 0 {
		t.Errorf("%d updates remain after confirming them all", len(remaining))
	}

	var undelivered int
	if err := db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM bot_updates WHERE bot_id = $1 AND delivered_at IS NULL`,
		bot.UserID).Scan(&undelivered); err != nil {
		t.Fatalf("count undelivered: %v", err)
	}
	if undelivered != 0 {
		t.Errorf("%d updates are still marked undelivered", undelivered)
	}
}

// Privacy mode is what stops a bot in a group seeing every message in it.
func TestPrivacyModeFiltersWhichBotsAreNotified(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	owner := createUser(t, db)
	private, _ := registerBot(t, repo, owner)
	open, _ := registerBot(t, repo, owner)

	// One bot opts out of privacy mode and so sees everything.
	privacyOff := false
	if err := repo.UpdateSettings(ctx, open.UserID,
		bots.Settings{PrivacyMode: &privacyOff}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	chatID := createGroupWith(t, db, owner, private.UserID, open.UserID)

	// An ordinary message reaches only the bot without privacy mode.
	notified, err := repo.SubscribedBots(ctx, chatID, false, nil)
	if err != nil {
		t.Fatalf("SubscribedBots: %v", err)
	}
	if len(notified) != 1 || notified[0] != open.UserID {
		t.Errorf("ordinary message notified %v, want just the open bot %s",
			notified, open.UserID)
	}

	// A command reaches both: privacy mode never hides commands.
	notified, err = repo.SubscribedBots(ctx, chatID, true, nil)
	if err != nil {
		t.Fatalf("SubscribedBots (command): %v", err)
	}
	if len(notified) != 2 {
		t.Errorf("a command notified %d bots, want 2", len(notified))
	}

	// A reply addressed to the private bot reaches it even without a command.
	notified, err = repo.SubscribedBots(ctx, chatID, false, &private.UserID)
	if err != nil {
		t.Fatalf("SubscribedBots (reply): %v", err)
	}
	if len(notified) != 2 {
		t.Errorf("a reply to the private bot notified %d bots, want 2", len(notified))
	}
}

func TestWebhookSecretIsGeneratedPerRegistration(t *testing.T) {
	db := testDB(t)
	repo := bots.NewRepository(db)
	ctx := context.Background()

	bot, _ := registerBot(t, repo, createUser(t, db))

	first, err := repo.SetWebhook(ctx, bot.UserID, "https://example.org/hook", 40, nil)
	if err != nil {
		t.Fatalf("SetWebhook: %v", err)
	}
	second, err := repo.SetWebhook(ctx, bot.UserID, "https://example.org/hook", 40, nil)
	if err != nil {
		t.Fatalf("SetWebhook (again): %v", err)
	}
	if first == second {
		t.Error("re-registering a webhook reused the signing secret")
	}

	hook, err := repo.Webhook(ctx, bot.UserID)
	if err != nil {
		t.Fatalf("Webhook: %v", err)
	}
	if hook.URL != "https://example.org/hook" {
		t.Errorf("stored URL = %q", hook.URL)
	}
}

func TestSignPayloadIsStableAndKeyed(t *testing.T) {
	body := []byte(`{"update_id":1}`)

	first := bots.SignPayload([]byte("secret"), body)
	if first != bots.SignPayload([]byte("secret"), body) {
		t.Error("the same key and body produced different signatures")
	}
	if first == bots.SignPayload([]byte("other"), body) {
		t.Error("a different key produced the same signature")
	}
	if first == bots.SignPayload([]byte("secret"), []byte(`{"update_id":2}`)) {
		t.Error("a different body produced the same signature")
	}
}

// createGroupWith makes a group containing the owner and the given bots.
func createGroupWith(t *testing.T, db *database.DB, owner uuid.UUID, members ...uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()

	var chatID uuid.UUID
	if err := db.Pool.QueryRow(ctx,
		`INSERT INTO chats (type, creator_id, title) VALUES ('group', $1, 'Bots') RETURNING id`,
		owner).Scan(&chatID); err != nil {
		t.Fatalf("create group: %v", err)
	}

	for _, member := range append([]uuid.UUID{owner}, members...) {
		if _, err := db.Pool.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')`,
			chatID, member); err != nil {
			t.Fatalf("add chat member: %v", err)
		}
	}
	return chatID
}
