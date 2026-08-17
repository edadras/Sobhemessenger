package bots

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/netguard"
)

// Inline keyboards and the taps they produce (§13).
//
// A keyboard is what turns a bot from a correspondent into an interface: the
// answer to "what can I do here?" is buttons rather than a list of commands to
// type correctly.

var (
	ErrCallbackNotFound = errors.New("bots: callback query not found")
	ErrAlreadyAnswered  = errors.New("bots: that query has already been answered")
)

// Limits on a keyboard.
//
// These bound what one bot can make every recipient's client render. They are
// generous for real use and small enough that a keyboard cannot become a
// denial of service against the people it is sent to.
const (
	MaxKeyboardRows      = 20
	MaxButtonsPerRow     = 8
	MaxButtonTextRunes   = 64
	MaxCallbackDataBytes = 64
)

// callbackTTL is how long a tap waits to be answered before the client should
// stop expecting one.
const callbackTTL = time.Hour

// Button is one key on an inline keyboard.
//
// Exactly one action must be set. A button that does nothing is a button that
// looks tappable and is not, which is worse than no button.
type Button struct {
	Text string `json:"text"`
	// CallbackData is sent back to the bot when the button is tapped. It never
	// leaves the platform, so it can hold whatever the bot needs to identify
	// the tap — but it is bounded, because it travels with every render.
	CallbackData string `json:"callback_data,omitempty"`
	// URL opens a link instead of calling the bot.
	URL string `json:"url,omitempty"`
	// SwitchInlineQuery puts `@bot <query>` into the compose box of a chat the
	// user picks, which is how a bot hands its own inline mode to someone.
	SwitchInlineQuery *string `json:"switch_inline_query,omitempty"`
}

// Keyboard is the rows of buttons under a message.
type Keyboard struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}

// Validate refuses a keyboard the platform should not render.
func (k *Keyboard) Validate() error {
	if len(k.InlineKeyboard) == 0 {
		return httpx.Validation("A keyboard needs at least one row").
			WithField("reply_markup", "at least one row")
	}
	if len(k.InlineKeyboard) > MaxKeyboardRows {
		return httpx.Validation("Too many keyboard rows").
			WithField("reply_markup", fmt.Sprintf("at most %d rows", MaxKeyboardRows))
	}

	for _, row := range k.InlineKeyboard {
		if len(row) == 0 {
			return httpx.Validation("A keyboard row cannot be empty").
				WithField("reply_markup", "every row needs a button")
		}
		if len(row) > MaxButtonsPerRow {
			return httpx.Validation("Too many buttons in a row").
				WithField("reply_markup",
					fmt.Sprintf("at most %d buttons per row", MaxButtonsPerRow))
		}

		for _, button := range row {
			if err := button.validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b Button) validate() error {
	text := strings.TrimSpace(b.Text)
	if text == "" {
		return httpx.Validation("A button needs a label").
			WithField("reply_markup", "every button needs text")
	}
	if len([]rune(text)) > MaxButtonTextRunes {
		return httpx.Validation("That button label is too long").
			WithField("reply_markup",
				fmt.Sprintf("at most %d characters", MaxButtonTextRunes))
	}

	// Exactly one action, so a tap has one unambiguous meaning.
	actions := 0
	if b.CallbackData != "" {
		actions++
	}
	if b.URL != "" {
		actions++
	}
	if b.SwitchInlineQuery != nil {
		actions++
	}
	if actions != 1 {
		return httpx.Validation("A button needs exactly one action").
			WithField("reply_markup",
				"set one of callback_data, url or switch_inline_query")
	}

	if len(b.CallbackData) > MaxCallbackDataBytes {
		return httpx.Validation("That callback data is too long").
			WithField("reply_markup",
				fmt.Sprintf("at most %d bytes", MaxCallbackDataBytes))
	}

	if b.URL != "" {
		// A button URL is rendered to every recipient and tapped by them, so it
		// goes through the same check as a webhook: no private addresses, and
		// nothing that is not http or https.
		if _, err := netguard.ValidateURL(b.URL); err != nil {
			return httpx.Validation("That button URL is not usable").
				WithField("reply_markup", "must be a public http or https URL")
		}
	}
	return nil
}

// CallbackQuery is a tap, waiting for its bot to answer.
type CallbackQuery struct {
	ID        uuid.UUID  `json:"id"`
	BotID     uuid.UUID  `json:"bot_id"`
	UserID    uuid.UUID  `json:"user_id"`
	ChatID    *uuid.UUID `json:"chat_id,omitempty"`
	MessageID *uuid.UUID `json:"message_id,omitempty"`
	Data      string     `json:"data"`
	CreatedAt time.Time  `json:"created_at"`
}

// RecordCallback stores a tap and returns it.
//
// The button is checked against the message it is on rather than trusted from
// the request: otherwise anyone could post any callback data to any bot, and a
// bot that keys on `data` alone would act on it.
func (r *Repository) RecordCallback(ctx context.Context, messageID, userID uuid.UUID, data string) (*CallbackQuery, error) {
	query := &CallbackQuery{Data: data, UserID: userID}

	err := r.db.Pool.QueryRow(ctx, `
		WITH target AS (
			SELECT m.id, m.chat_id, m.sender_id, m.reply_markup
			  FROM messages m
			  JOIN chat_members cm
			    ON cm.chat_id = m.chat_id AND cm.user_id = $2 AND cm.left_at IS NULL
			  JOIN bots b ON b.user_id = m.sender_id AND b.is_active
			 WHERE m.id = $1
			   AND m.deleted_at IS NULL
			   AND m.reply_markup IS NOT NULL
			   -- The data must name a button that is really on this message.
			   AND EXISTS (
			       SELECT 1
			         FROM jsonb_array_elements(m.reply_markup -> 'inline_keyboard') AS row
			         CROSS JOIN jsonb_array_elements(row) AS button
			        WHERE button ->> 'callback_data' = $3
			   )
		)
		INSERT INTO bot_callback_queries (bot_id, user_id, chat_id, message_id, data, expires_at)
		SELECT t.sender_id, $2, t.chat_id, t.id, $3, now() + $4::interval
		  FROM target t
		RETURNING id, bot_id, chat_id, message_id, created_at`,
		messageID, userID, data, callbackTTL.String(),
	).Scan(&query.ID, &query.BotID, &query.ChatID, &query.MessageID, &query.CreatedAt)
	if database.IsNoRows(err) {
		// Either the message is not a bot's, the caller is not in the chat, or
		// the data names no button on it. All three are the same answer.
		return nil, ErrCallbackNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("bots: record callback: %w", err)
	}
	return query, nil
}

// AnswerCallback records what the bot said back to a tap.
func (r *Repository) AnswerCallback(ctx context.Context, botID, queryID uuid.UUID, text string, showAlert bool) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE bot_callback_queries
		   SET answered_at = now(), answer_text = $3, show_alert = $4
		 WHERE id = $1 AND bot_id = $2 AND answered_at IS NULL`,
		queryID, botID, text, showAlert)
	if err != nil {
		return fmt.Errorf("bots: answer callback: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyAnswered
	}
	return nil
}

// SetReplyMarkup replaces the keyboard on a message the bot sent.
//
// Editing a keyboard rather than the text is how a bot shows a choice being
// made — a menu becoming a confirmation — without filling the chat with
// messages nobody wants to scroll past.
func (r *Repository) SetReplyMarkup(ctx context.Context, botID, messageID uuid.UUID, keyboard *Keyboard) error {
	var encoded []byte
	if keyboard != nil {
		var err error
		if encoded, err = json.Marshal(keyboard); err != nil {
			return fmt.Errorf("bots: encode keyboard: %w", err)
		}
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE messages SET reply_markup = $3, edited_at = now()
		 WHERE id = $1 AND sender_id = $2 AND deleted_at IS NULL`,
		messageID, botID, encoded)
	if err != nil {
		return fmt.Errorf("bots: set reply markup: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// A bot may only change its own message, which is also what stops one
		// bot rewriting another's buttons.
		return ErrNotFound
	}
	return nil
}

// PurgeExpiredCallbacks drops taps nobody answered.
func (r *Repository) PurgeExpiredCallbacks(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM bot_callback_queries WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("bots: purge callbacks: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------- service

// Tap records a button press and tells the bot about it.
func (s *Service) Tap(ctx context.Context, userID, messageID uuid.UUID, data string) (*CallbackQuery, error) {
	query, err := s.repo.RecordCallback(ctx, messageID, userID, data)
	if err != nil {
		if errors.Is(err, ErrCallbackNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "That button is not available")
		}
		return nil, httpx.Internal(err)
	}

	if err := s.repo.Enqueue(ctx, query.BotID, UpdateCallbackQuery, map[string]any{
		"id":         query.ID,
		"from":       query.UserID,
		"chat_id":    query.ChatID,
		"message_id": query.MessageID,
		"data":       query.Data,
	}); err != nil {
		// The tap is recorded either way; the bot can still find it by polling
		// its pending queries.
		s.logger.Warn("could not enqueue a callback query", errAttr(err))
	}
	return query, nil
}

// AnswerCallback is what a bot calls to close a tap.
func (s *Service) AnswerCallback(ctx context.Context, botID, queryID uuid.UUID, text string, showAlert bool) error {
	if len([]rune(text)) > 200 {
		return httpx.Validation("That answer is too long").
			WithField("text", "at most 200 characters")
	}

	if err := s.repo.AnswerCallback(ctx, botID, queryID, text, showAlert); err != nil {
		if errors.Is(err, ErrAlreadyAnswered) {
			return httpx.Conflict(httpx.CodeConflict,
				"That query has already been answered, or is not yours")
		}
		return httpx.Internal(err)
	}
	return nil
}

// SetReplyMarkup edits the keyboard under one of the bot's own messages.
func (s *Service) SetReplyMarkup(ctx context.Context, botID, messageID uuid.UUID, keyboard *Keyboard) error {
	if keyboard != nil {
		if err := keyboard.Validate(); err != nil {
			return err
		}
	}

	if err := s.repo.SetReplyMarkup(ctx, botID, messageID, keyboard); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound,
				"That message is not one this bot can edit")
		}
		return httpx.Internal(err)
	}
	return nil
}
