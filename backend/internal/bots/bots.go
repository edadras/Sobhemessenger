// Package bots is the bot platform (§20).
//
// A bot is an ordinary account with `is_bot` set, not a parallel kind of
// principal. Every table that already references a user — chat_members,
// messages, reactions, permissions — therefore works for bots unchanged, and
// there is one authorisation path rather than two that drift apart.
//
// What differs is how a bot authenticates and how it learns about events. A
// person holds a session and a socket; a bot holds a long-lived token and
// receives updates by webhook or long poll. Those two differences are what
// this package implements.
package bots

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/security"
)

var (
	ErrNotFound     = errors.New("bots: not found")
	ErrInvalidToken = errors.New("bots: token is not valid")
	ErrNotOwner     = errors.New("bots: you do not own this bot")
)

// Limits chosen so one owner cannot exhaust the namespace or the queue.
const (
	// MaxBotsPerOwner bounds how many bots one account may register.
	MaxBotsPerOwner = 20
	// MaxTokensPerBot allows an overlapping rotation without becoming a way
	// to keep dozens of live credentials.
	MaxTokensPerBot = 5
	// MaxCommands is what a client can reasonably show in a menu.
	MaxCommands = 100
	// tokenSecretBytes is the entropy behind the secret half of a token.
	tokenSecretBytes = 32
	// UpdateBacklog is how many undelivered updates one bot may accumulate
	// before the oldest are dropped. A bot that never collects its updates
	// must not be able to grow the table without bound.
	UpdateBacklog = 10000
)

// Bot is a registered bot and the account behind it.
type Bot struct {
	UserID            uuid.UUID `json:"user_id"`
	OwnerID           uuid.UUID `json:"owner_id"`
	Username          *string   `json:"username,omitempty"`
	DisplayName       string    `json:"display_name"`
	Description       string    `json:"description,omitempty"`
	About             string    `json:"about,omitempty"`
	CanJoinGroups     bool      `json:"can_join_groups"`
	PrivacyMode       bool      `json:"privacy_mode"`
	InlineEnabled     bool      `json:"inline_enabled"`
	InlinePlaceholder string    `json:"inline_placeholder,omitempty"`
	IsActive          bool      `json:"is_active"`
	CreatedAt         time.Time `json:"created_at"`
}

// Token is a credential, listed without its secret.
type Token struct {
	ID         uuid.UUID  `json:"id"`
	Prefix     string     `json:"prefix"`
	Label      string     `json:"label,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// IssuedToken is returned exactly once, when the token is created.
type IssuedToken struct {
	Token
	// Secret is the only time the plaintext exists outside the caller. It is
	// stored as a SHA-256 hash, so a lost token cannot be recovered — only
	// replaced.
	Secret string `json:"token"`
}

// Command is one entry in a bot's "/" menu.
type Command struct {
	Command     string  `json:"command"`
	Description string  `json:"description"`
	Position    int     `json:"position"`
	Locale      *string `json:"locale,omitempty"`
}

// Webhook is where updates are delivered.
type Webhook struct {
	URL            string     `json:"url"`
	MaxConnections int        `json:"max_connections"`
	AllowedUpdates []string   `json:"allowed_updates,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	LastErrorAt    *time.Time `json:"last_error_at,omitempty"`
	FailureCount   int        `json:"failure_count"`
}

// The update kinds a bot can be sent, matching the column's CHECK.
//
// These are Telegram's names on purpose: a bot author moving a bot across
// already has code that switches on them. A command is not among them because
// a command is a message that starts with a slash, there as well as here.
const (
	UpdateMessage       = "message"
	UpdateEditedMessage = "edited_message"
	UpdateCallbackQuery = "callback_query"
	UpdateInlineQuery   = "inline_query"
	UpdateChatMember    = "chat_member"
	UpdateMyChatMember  = "my_chat_member"
)

// Update is one thing that happened, addressed to a bot.
type Update struct {
	ID      int64           `json:"update_id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateParams is a new bot registration.
type CreateParams struct {
	OwnerID     uuid.UUID
	Username    string
	DisplayName string
	Description string
}

// Create registers a bot, creating the account behind it.
//
// The account and the bot row are written in one transaction: an account with
// is_bot set but no bots row would be an unmanageable ghost that still
// occupies a username.
func (r *Repository) Create(ctx context.Context, in CreateParams) (*Bot, *IssuedToken, error) {
	var (
		bot    Bot
		issued *IssuedToken
	)

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM bots WHERE owner_id = $1`, in.OwnerID).Scan(&count); err != nil {
			return fmt.Errorf("bots: count owned: %w", err)
		}
		if count >= MaxBotsPerOwner {
			return ErrTooManyBots
		}

		var available bool
		if err := tx.QueryRow(ctx,
			`SELECT username_available($1, NULL)`, in.Username).Scan(&available); err != nil {
			return fmt.Errorf("bots: check username: %w", err)
		}
		if !available {
			return ErrUsernameTaken
		}

		// A bot has no phone number, but the column is NOT NULL and unique.
		// A namespaced synthetic value keeps the constraint meaningful while
		// making it obvious that no real number is involved.
		syntheticPhone := "bot:" + in.Username
		var userID uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO users (phone_number, phone_hash, username, is_bot)
			VALUES ($1, $2, $3, TRUE)
			RETURNING id`,
			syntheticPhone, security.HashToken(syntheticPhone), in.Username,
		).Scan(&userID); err != nil {
			if database.IsUniqueViolation(err) {
				return ErrUsernameTaken
			}
			return fmt.Errorf("bots: create account: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`,
			userID, in.DisplayName); err != nil {
			return fmt.Errorf("bots: create profile: %w", err)
		}
		// Bots receive messages through their update queue, but the counter
		// row is what every write path expects to exist.
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_event_counters (user_id) VALUES ($1)`, userID); err != nil {
			return fmt.Errorf("bots: create event counter: %w", err)
		}

		if err := tx.QueryRow(ctx, `
			INSERT INTO bots (user_id, owner_id, description)
			VALUES ($1, $2, $3)
			RETURNING user_id, owner_id, description, about, can_join_groups,
			          privacy_mode, inline_enabled, inline_placeholder, is_active, created_at`,
			userID, in.OwnerID, in.Description,
		).Scan(&bot.UserID, &bot.OwnerID, &bot.Description, &bot.About,
			&bot.CanJoinGroups, &bot.PrivacyMode, &bot.InlineEnabled,
			&bot.InlinePlaceholder, &bot.IsActive, &bot.CreatedAt); err != nil {
			return fmt.Errorf("bots: create bot: %w", err)
		}
		bot.Username = &in.Username
		bot.DisplayName = in.DisplayName

		token, err := issueToken(ctx, tx, userID, in.Username, "default")
		if err != nil {
			return err
		}
		issued = token
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return &bot, issued, nil
}

var (
	ErrTooManyBots   = errors.New("bots: you have reached the bot limit")
	ErrUsernameTaken = errors.New("bots: that username is taken")
	ErrTooManyTokens = errors.New("bots: too many live tokens")
)

// issueToken mints a credential shaped like "<botname>:<secret>".
//
// The readable prefix is what makes a leaked token identifiable in a log or a
// commit without the secret half being present.
func issueToken(ctx context.Context, tx pgx.Tx, botID uuid.UUID, username, label string) (*IssuedToken, error) {
	secret, err := security.RandomToken(tokenSecretBytes)
	if err != nil {
		return nil, fmt.Errorf("bots: generate token: %w", err)
	}

	plaintext := username + ":" + secret
	prefix := username

	issued := &IssuedToken{Secret: plaintext}
	if err := tx.QueryRow(ctx, `
		INSERT INTO bot_tokens (bot_id, token_hash, token_prefix, label)
		VALUES ($1, $2, $3, $4)
		RETURNING id, token_prefix, label, created_at`,
		botID, security.HashToken(plaintext), prefix, label,
	).Scan(&issued.ID, &issued.Prefix, &issued.Label, &issued.CreatedAt); err != nil {
		return nil, fmt.Errorf("bots: store token: %w", err)
	}
	return issued, nil
}

// Authenticate resolves a token to its bot.
//
// The lookup is by hash, so the plaintext is never compared in the database
// and a dump of bot_tokens does not let anyone drive a bot.
func (r *Repository) Authenticate(ctx context.Context, token string) (*Bot, error) {
	var bot Bot
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE bot_tokens SET last_used_at = now()
		 WHERE token_hash = $1 AND revoked_at IS NULL
		 RETURNING bot_id`,
		security.HashToken(token),
	).Scan(&bot.UserID)
	if database.IsNoRows(err) {
		return nil, ErrInvalidToken
	}
	if err != nil {
		return nil, fmt.Errorf("bots: authenticate: %w", err)
	}

	loaded, err := r.ByID(ctx, bot.UserID)
	if err != nil {
		return nil, err
	}
	if !loaded.IsActive {
		return nil, ErrInvalidToken
	}
	return loaded, nil
}

func (r *Repository) ByID(ctx context.Context, botID uuid.UUID) (*Bot, error) {
	var bot Bot
	err := r.db.Pool.QueryRow(ctx, `
		SELECT b.user_id, b.owner_id, u.username, COALESCE(p.display_name, ''),
		       b.description, b.about, b.can_join_groups, b.privacy_mode,
		       b.inline_enabled, b.inline_placeholder, b.is_active, b.created_at
		  FROM bots b
		  JOIN users u ON u.id = b.user_id AND u.deleted_at IS NULL
		  LEFT JOIN user_profiles p ON p.user_id = b.user_id
		 WHERE b.user_id = $1`, botID,
	).Scan(&bot.UserID, &bot.OwnerID, &bot.Username, &bot.DisplayName,
		&bot.Description, &bot.About, &bot.CanJoinGroups, &bot.PrivacyMode,
		&bot.InlineEnabled, &bot.InlinePlaceholder, &bot.IsActive, &bot.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bots: read bot: %w", err)
	}
	return &bot, nil
}

func (r *Repository) ListForOwner(ctx context.Context, ownerID uuid.UUID) ([]Bot, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT b.user_id, b.owner_id, u.username, COALESCE(p.display_name, ''),
		       b.description, b.about, b.can_join_groups, b.privacy_mode,
		       b.inline_enabled, b.inline_placeholder, b.is_active, b.created_at
		  FROM bots b
		  JOIN users u ON u.id = b.user_id AND u.deleted_at IS NULL
		  LEFT JOIN user_profiles p ON p.user_id = b.user_id
		 WHERE b.owner_id = $1
		 ORDER BY b.created_at`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("bots: list: %w", err)
	}
	defer rows.Close()

	var list []Bot
	for rows.Next() {
		var bot Bot
		if err := rows.Scan(&bot.UserID, &bot.OwnerID, &bot.Username, &bot.DisplayName,
			&bot.Description, &bot.About, &bot.CanJoinGroups, &bot.PrivacyMode,
			&bot.InlineEnabled, &bot.InlinePlaceholder, &bot.IsActive,
			&bot.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, bot)
	}
	return list, rows.Err()
}

// Settings are the fields an owner may change after registration.
type Settings struct {
	// DisplayName lives in user_profiles rather than in bots, because a bot is
	// a user and that is where a user's name is kept.
	DisplayName       *string
	Description       *string
	About             *string
	CanJoinGroups     *bool
	PrivacyMode       *bool
	InlineEnabled     *bool
	InlinePlaceholder *string
	IsActive          *bool
}

func (r *Repository) UpdateSettings(ctx context.Context, botID uuid.UUID, in Settings) error {
	if in.DisplayName != nil {
		if _, err := r.db.Pool.Exec(ctx, `
			INSERT INTO user_profiles (user_id, display_name)
			VALUES ($1, $2)
			ON CONFLICT (user_id) DO UPDATE
			SET display_name = EXCLUDED.display_name, updated_at = now()`,
			botID, *in.DisplayName); err != nil {
			return fmt.Errorf("bots: update display name: %w", err)
		}
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE bots SET
			description        = COALESCE($2, description),
			about              = COALESCE($3, about),
			can_join_groups    = COALESCE($4, can_join_groups),
			privacy_mode       = COALESCE($5, privacy_mode),
			inline_enabled     = COALESCE($6, inline_enabled),
			inline_placeholder = COALESCE($7, inline_placeholder),
			is_active          = COALESCE($8, is_active),
			updated_at         = now()
		 WHERE user_id = $1`,
		botID, in.Description, in.About, in.CanJoinGroups, in.PrivacyMode,
		in.InlineEnabled, in.InlinePlaceholder, in.IsActive)
	if err != nil {
		return fmt.Errorf("bots: update settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// IssueToken adds a credential, for an overlapping rotation.
func (r *Repository) IssueToken(ctx context.Context, botID uuid.UUID, label string) (*IssuedToken, error) {
	var issued *IssuedToken

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var username string
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(u.username::text, '') FROM users u WHERE u.id = $1`,
			botID).Scan(&username); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("bots: read bot username: %w", err)
		}

		var live int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM bot_tokens WHERE bot_id = $1 AND revoked_at IS NULL`,
			botID).Scan(&live); err != nil {
			return fmt.Errorf("bots: count tokens: %w", err)
		}
		if live >= MaxTokensPerBot {
			return ErrTooManyTokens
		}

		token, err := issueToken(ctx, tx, botID, username, label)
		if err != nil {
			return err
		}
		issued = token
		return nil
	})
	if err != nil {
		return nil, err
	}
	return issued, nil
}

func (r *Repository) ListTokens(ctx context.Context, botID uuid.UUID) ([]Token, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, token_prefix, label, created_at, last_used_at, revoked_at
		  FROM bot_tokens WHERE bot_id = $1 ORDER BY created_at`, botID)
	if err != nil {
		return nil, fmt.Errorf("bots: list tokens: %w", err)
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var token Token
		if err := rows.Scan(&token.ID, &token.Prefix, &token.Label,
			&token.CreatedAt, &token.LastUsedAt, &token.RevokedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (r *Repository) RevokeToken(ctx context.Context, botID, tokenID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE bot_tokens SET revoked_at = now()
		  WHERE id = $1 AND bot_id = $2 AND revoked_at IS NULL`, tokenID, botID)
	if err != nil {
		return fmt.Errorf("bots: revoke token: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetCommands replaces the command list wholesale.
//
// Replacement rather than merge is what the caller means: the list they send
// is the menu they want, and a command they removed must disappear.
func (r *Repository) SetCommands(ctx context.Context, botID uuid.UUID, commands []Command) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`DELETE FROM bot_commands WHERE bot_id = $1`, botID); err != nil {
			return fmt.Errorf("bots: clear commands: %w", err)
		}
		for i, command := range commands {
			if _, err := tx.Exec(ctx, `
				INSERT INTO bot_commands (bot_id, command, description, position, locale)
				VALUES ($1, $2, $3, $4, $5)`,
				botID, strings.ToLower(command.Command), command.Description,
				i, command.Locale); err != nil {
				if database.IsCheckViolation(err) {
					return ErrInvalidCommand
				}
				return fmt.Errorf("bots: insert command: %w", err)
			}
		}
		return nil
	})
}

var ErrInvalidCommand = errors.New("bots: a command name is not valid")

func (r *Repository) Commands(ctx context.Context, botID uuid.UUID, locale string) ([]Command, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT command, description, position, locale
		  FROM bot_commands
		 WHERE bot_id = $1 AND (locale IS NULL OR locale = $2)
		 ORDER BY position`, botID, locale)
	if err != nil {
		return nil, fmt.Errorf("bots: read commands: %w", err)
	}
	defer rows.Close()

	var commands []Command
	for rows.Next() {
		var command Command
		if err := rows.Scan(&command.Command, &command.Description,
			&command.Position, &command.Locale); err != nil {
			return nil, err
		}
		commands = append(commands, command)
	}
	return commands, rows.Err()
}

// SetWebhook points a bot at a URL, returning the signing secret once.
func (r *Repository) SetWebhook(ctx context.Context, botID uuid.UUID, url string, maxConnections int, allowed []string) (string, error) {
	secret, err := security.RandomToken(32)
	if err != nil {
		return "", fmt.Errorf("bots: generate webhook secret: %w", err)
	}

	// A nil slice arrives as SQL NULL, which the NOT NULL column refuses; an
	// empty list is what "deliver every update kind" means.
	if allowed == nil {
		allowed = []string{}
	}

	if _, err := r.db.Pool.Exec(ctx, `
		INSERT INTO bot_webhooks (bot_id, url, secret, max_connections, allowed_updates)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (bot_id) DO UPDATE
		SET url = EXCLUDED.url, secret = EXCLUDED.secret,
		    max_connections = EXCLUDED.max_connections,
		    allowed_updates = EXCLUDED.allowed_updates,
		    last_error = '', last_error_at = NULL, failure_count = 0`,
		botID, url, []byte(secret), maxConnections, allowed); err != nil {
		return "", fmt.Errorf("bots: set webhook: %w", err)
	}
	return secret, nil
}

func (r *Repository) Webhook(ctx context.Context, botID uuid.UUID) (*Webhook, error) {
	var hook Webhook
	err := r.db.Pool.QueryRow(ctx, `
		SELECT url, max_connections, allowed_updates, last_error, last_error_at, failure_count
		  FROM bot_webhooks WHERE bot_id = $1`, botID,
	).Scan(&hook.URL, &hook.MaxConnections, &hook.AllowedUpdates,
		&hook.LastError, &hook.LastErrorAt, &hook.FailureCount)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bots: read webhook: %w", err)
	}
	return &hook, nil
}

func (r *Repository) DeleteWebhook(ctx context.Context, botID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM bot_webhooks WHERE bot_id = $1`, botID)
	return err
}

// WebhookTarget is what the delivery worker needs to post an update.
type WebhookTarget struct {
	BotID          uuid.UUID
	URL            string
	Secret         []byte
	AllowedUpdates []string
	FailureCount   int
}

// PendingWebhooks lists bots with undelivered updates and a webhook set.
func (r *Repository) PendingWebhooks(ctx context.Context, limit int) ([]WebhookTarget, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT DISTINCT w.bot_id, w.url, w.secret, w.allowed_updates, w.failure_count
		  FROM bot_webhooks w
		  JOIN bot_updates u ON u.bot_id = w.bot_id AND u.delivered_at IS NULL
		 -- A permanently failing endpoint is left alone rather than retried
		 -- for ever; the owner has to fix it and set the webhook again.
		 WHERE w.failure_count < 20
		 LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("bots: list pending webhooks: %w", err)
	}
	defer rows.Close()

	var targets []WebhookTarget
	for rows.Next() {
		var target WebhookTarget
		if err := rows.Scan(&target.BotID, &target.URL, &target.Secret,
			&target.AllowedUpdates, &target.FailureCount); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (r *Repository) RecordWebhookResult(ctx context.Context, botID uuid.UUID, failure string) error {
	if failure == "" {
		_, err := r.db.Pool.Exec(ctx,
			`UPDATE bot_webhooks SET failure_count = 0, last_error = '', last_error_at = NULL
			  WHERE bot_id = $1`, botID)
		return err
	}
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE bot_webhooks
		    SET failure_count = failure_count + 1, last_error = $2, last_error_at = now()
		  WHERE bot_id = $1`, botID, failure)
	return err
}

// ---------------------------------------------------------------- updates

// Enqueue stores an update for a bot.
//
// It is stored before it is delivered, so a bot that is offline or polling
// slowly loses nothing — the same "durable log, best-effort realtime" split
// the messenger itself uses.
func (r *Repository) Enqueue(ctx context.Context, botID uuid.UUID, updateType string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bots: marshal update: %w", err)
	}

	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO bot_updates (bot_id, type, payload) VALUES ($1, $2, $3)`,
			botID, updateType, encoded); err != nil {
			return fmt.Errorf("bots: enqueue update: %w", err)
		}

		// A bot that never collects must not grow the table without bound, so
		// the oldest undelivered updates are dropped past the backlog.
		if _, err := tx.Exec(ctx, `
			DELETE FROM bot_updates
			 WHERE bot_id = $1 AND delivered_at IS NULL
			   AND id NOT IN (
			       SELECT id FROM bot_updates
			        WHERE bot_id = $1 AND delivered_at IS NULL
			        ORDER BY id DESC LIMIT $2)`,
			botID, UpdateBacklog); err != nil {
			return fmt.Errorf("bots: trim update backlog: %w", err)
		}
		return nil
	})
}

// Poll hands over undelivered updates after `offset`.
//
// Acknowledgement is implicit and matches what a polling client expects:
// asking for updates after id N confirms everything up to N, so a bot that
// crashes mid-batch simply asks again from the last id it processed.
func (r *Repository) Poll(ctx context.Context, botID uuid.UUID, offset int64, limit int) ([]Update, error) {
	if offset > 0 {
		if _, err := r.db.Pool.Exec(ctx,
			`UPDATE bot_updates SET delivered_at = now()
			  WHERE bot_id = $1 AND id <= $2 AND delivered_at IS NULL`,
			botID, offset); err != nil {
			return nil, fmt.Errorf("bots: confirm updates: %w", err)
		}
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, type, payload
		  FROM bot_updates
		 WHERE bot_id = $1 AND id > $2 AND delivered_at IS NULL
		 ORDER BY id
		 LIMIT $3`, botID, offset, limit)
	if err != nil {
		return nil, fmt.Errorf("bots: poll updates: %w", err)
	}
	defer rows.Close()

	var updates []Update
	for rows.Next() {
		var update Update
		if err := rows.Scan(&update.ID, &update.Type, &update.Payload); err != nil {
			return nil, err
		}
		updates = append(updates, update)
	}
	return updates, rows.Err()
}

// PendingUpdates reads what a webhook delivery should send.
func (r *Repository) PendingUpdates(ctx context.Context, botID uuid.UUID, limit int) ([]Update, error) {
	return r.Poll(ctx, botID, 0, limit)
}

func (r *Repository) MarkDelivered(ctx context.Context, botID uuid.UUID, ids []int64) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE bot_updates SET delivered_at = now()
		  WHERE bot_id = $1 AND id = ANY($2::bigint[])`, botID, ids)
	return err
}

// SubscribedBots lists the bots in a chat that should receive an update.
//
// Privacy mode is applied here rather than at delivery: a bot with privacy on
// sees only commands, replies to itself and messages that name it, so
// filtering at the source means the rest never reaches its queue at all.
//
// It does not apply in a one-to-one chat. Privacy mode answers "should this
// bot see other people's conversation?", and in a private chat there is no
// other conversation — every message is addressed to the bot by the act of
// being sent there. Applying it anyway would mean a bot could only be talked
// to in commands, which is not a messenger.
func (r *Repository) SubscribedBots(
	ctx context.Context,
	chatID uuid.UUID,
	isCommand bool,
	replyToBot *uuid.UUID,
	mentioned []uuid.UUID,
) ([]uuid.UUID, error) {
	if mentioned == nil {
		mentioned = []uuid.UUID{}
	}
	rows, err := r.db.Pool.Query(ctx, `
		SELECT b.user_id
		  FROM chat_members cm
		  JOIN bots b ON b.user_id = cm.user_id AND b.is_active
		  JOIN chats c ON c.id = cm.chat_id AND c.deleted_at IS NULL
		 WHERE cm.chat_id = $1
		   AND cm.left_at IS NULL
		   AND (c.type = 'private'
		        OR NOT b.privacy_mode
		        OR $2
		        OR b.user_id = $3
		        OR b.user_id = ANY($4::uuid[]))`,
		chatID, isCommand, replyToBot, mentioned)
	if err != nil {
		return nil, fmt.Errorf("bots: list subscribed: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// RevokeAllTokens retires every token a bot holds.
//
// This is what "my token leaked" needs: revoking one and leaving the rest live
// would not answer the request.
func (r *Repository) RevokeAllTokens(ctx context.Context, botID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE bot_tokens SET revoked_at = now()
		  WHERE bot_id = $1 AND revoked_at IS NULL`, botID)
	if err != nil {
		return fmt.Errorf("bots: revoke all tokens: %w", err)
	}
	return nil
}

// IsPrivateChat answers whether a chat is one-to-one.
//
// BotFather refuses to work anywhere else: in a group its replies would hand
// one member's token to everyone in the room.
func (r *Repository) IsPrivateChat(ctx context.Context, chatID uuid.UUID) (bool, error) {
	var chatType string
	err := r.db.Pool.QueryRow(ctx,
		`SELECT type FROM chats WHERE id = $1 AND deleted_at IS NULL`, chatID).Scan(&chatType)
	if database.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("bots: read chat type: %w", err)
	}
	return chatType == "private", nil
}

// MessageAuthorIfBot returns the author of a message when that author is a
// bot, and nil otherwise.
//
// Replying to a bot is how someone addresses it without typing a command, so
// this decides whether a reply reaches a bot that has privacy mode on.
func (r *Repository) MessageAuthorIfBot(ctx context.Context, messageID uuid.UUID) (*uuid.UUID, error) {
	var author *uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT m.sender_id
		  FROM messages m
		  JOIN bots b ON b.user_id = m.sender_id
		 WHERE m.id = $1`, messageID).Scan(&author)
	if database.IsNoRows(err) {
		return nil, nil // not a bot's message, which is the usual case
	}
	if err != nil {
		return nil, fmt.Errorf("bots: resolve reply author: %w", err)
	}
	return author, nil
}

// BotIDByUsername resolves a handle to a bot, or nil when the handle is not a
// bot's.
func (r *Repository) BotIDByUsername(ctx context.Context, username string) (*uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT b.user_id
		  FROM bots b
		  JOIN users u ON u.id = b.user_id
		 WHERE u.username = $1 AND u.deleted_at IS NULL AND b.is_active`,
		username).Scan(&id)
	if database.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bots: resolve bot username: %w", err)
	}
	return &id, nil
}

// BotIDsByUsernames resolves the handles a message mentions to bot ids,
// ignoring the ones that name people rather than bots.
func (r *Repository) BotIDsByUsernames(ctx context.Context, usernames []string) ([]uuid.UUID, error) {
	if len(usernames) == 0 {
		return nil, nil
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT b.user_id
		  FROM bots b
		  JOIN users u ON u.id = b.user_id
		 WHERE u.username = ANY($1::citext[])
		   AND u.deleted_at IS NULL AND b.is_active`, usernames)
	if err != nil {
		return nil, fmt.Errorf("bots: resolve mentioned bots: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SignPayload is the signature a bot verifies on a webhook delivery.
//
// HMAC-SHA256 over the exact body, hex encoded — the same construction every
// webhook provider uses, so a bot author already knows how to check it.
func SignPayload(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}
