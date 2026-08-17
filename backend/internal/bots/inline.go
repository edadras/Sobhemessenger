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
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Inline mode (§13).
//
// Typing `@somebot pizza` in any chat asks that bot for results without adding
// it to the chat. The bot is told the query and who asked; it is deliberately
// **not** told where they are typing. It has no claim to know that, and saying
// so would leak the existence of a conversation it is not part of. Nothing is
// sent until the person picks a result, and what is then sent is sent by them.

var (
	ErrInlineNotEnabled = errors.New("bots: that bot does not answer inline queries")
	ErrQueryNotFound    = errors.New("bots: inline query not found")
)

// inlineQueryTTL is how long a query waits for its answer.
//
// Typing produces a query every few keystrokes, and one from thirty seconds
// ago is about a word the person has already finished.
const inlineQueryTTL = 5 * time.Minute

// MaxInlineResults bounds one answer. A picker shows a handful; the rest is
// data nobody scrolls to and every client has to hold.
const MaxInlineResults = 50

// InlineResult is one thing a bot offers.
type InlineResult struct {
	// ID is the bot's own identifier for this result, echoed back when it is
	// chosen so the bot can tell which one was picked.
	ID    string `json:"id"`
	Type  string `json:"type"`
	Title string `json:"title"`
	// Description is the second line in the picker.
	Description string `json:"description,omitempty"`
	// Content is what gets sent when the result is chosen.
	Content string `json:"content,omitempty"`
	// MediaID attaches an uploaded object, for the media result types.
	MediaID *uuid.UUID `json:"media_id,omitempty"`
	// ThumbnailMediaID is what the picker draws; without it a media result is
	// a row of text.
	ThumbnailMediaID *uuid.UUID `json:"thumbnail_media_id,omitempty"`
	Keyboard         *Keyboard  `json:"reply_markup,omitempty"`
}

// validInlineResultTypes are the shapes a result may take. Each maps onto a
// message type, because choosing a result sends a message.
var validInlineResultTypes = map[string]string{
	"article":  messaging.TypeText,
	"photo":    messaging.TypeImage,
	"video":    messaging.TypeVideo,
	"audio":    messaging.TypeAudio,
	"document": messaging.TypeFile,
	"sticker":  messaging.TypeSticker,
	"gif":      messaging.TypeGIF,
}

// InlineQuery is one ask, waiting for its bot.
type InlineQuery struct {
	ID        uuid.UUID `json:"id"`
	BotID     uuid.UUID `json:"bot_id"`
	UserID    uuid.UUID `json:"user_id"`
	Query     string    `json:"query"`
	Offset    string    `json:"offset,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// OpenInlineQuery records an ask and returns it.
//
// The bot must have inline mode on: without that check, any bot could be made
// to answer queries it never advertised.
func (r *Repository) OpenInlineQuery(ctx context.Context, botID, userID uuid.UUID, query, offset string) (*InlineQuery, error) {
	result := &InlineQuery{BotID: botID, UserID: userID, Query: query, Offset: offset}

	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO bot_inline_queries (bot_id, user_id, query, offset_key, expires_at)
		SELECT $1, $2, $3, $4, now() + $5::interval
		  FROM bots b
		 WHERE b.user_id = $1 AND b.is_active AND b.inline_enabled
		RETURNING id, created_at`,
		botID, userID, query, offset, inlineQueryTTL.String(),
	).Scan(&result.ID, &result.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrInlineNotEnabled
	}
	if err != nil {
		return nil, fmt.Errorf("bots: open inline query: %w", err)
	}
	return result, nil
}

// AnswerInlineQuery stores the results a bot offers.
func (r *Repository) AnswerInlineQuery(ctx context.Context, botID, queryID uuid.UUID, results []InlineResult) error {
	encoded, err := json.Marshal(results)
	if err != nil {
		return fmt.Errorf("bots: encode inline results: %w", err)
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE bot_inline_queries
		   SET results = $3::jsonb, answered_at = now()
		 WHERE id = $1 AND bot_id = $2 AND expires_at > now()`,
		queryID, botID, encoded)
	if err != nil {
		return fmt.Errorf("bots: answer inline query: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrQueryNotFound
	}
	return nil
}

// InlineResults reads back what a bot offered, for the person who asked.
//
// The asker is part of the lookup: an inline query is a private exchange
// between one person and one bot, and another user must not be able to read
// what was offered by naming the query id.
func (r *Repository) InlineResults(ctx context.Context, queryID, userID uuid.UUID) ([]InlineResult, uuid.UUID, string, error) {
	var (
		raw   []byte
		botID uuid.UUID
		asked string
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT results, bot_id, query FROM bot_inline_queries
		 WHERE id = $1 AND user_id = $2 AND expires_at > now()`,
		queryID, userID).Scan(&raw, &botID, &asked)
	if database.IsNoRows(err) {
		return nil, uuid.Nil, "", ErrQueryNotFound
	}
	if err != nil {
		return nil, uuid.Nil, "", fmt.Errorf("bots: read inline results: %w", err)
	}

	var results []InlineResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &results); err != nil {
			return nil, uuid.Nil, "", fmt.Errorf("bots: decode inline results: %w", err)
		}
	}
	return results, botID, asked, nil
}

// RecordChosenResult notes which result a person picked.
func (r *Repository) RecordChosenResult(ctx context.Context, botID, userID, queryID uuid.UUID, resultID, query string) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO bot_chosen_inline_results (bot_id, user_id, query_id, result_id, query)
		VALUES ($1, $2, $3, $4, $5)`,
		botID, userID, queryID, resultID, query)
	if err != nil {
		return fmt.Errorf("bots: record chosen result: %w", err)
	}
	return nil
}

// PurgeExpiredInlineQueries drops asks nobody answered or picked from.
func (r *Repository) PurgeExpiredInlineQueries(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM bot_inline_queries WHERE expires_at < now()`)
	if err != nil {
		return 0, fmt.Errorf("bots: purge inline queries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------- service

// StartInlineQuery is what a client calls as someone types `@bot …`.
func (s *Service) StartInlineQuery(ctx context.Context, userID uuid.UUID, botUsername, query, offset string) (*InlineQuery, error) {
	handle := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(botUsername), "@"))
	botID, err := s.repo.BotIDByUsername(ctx, handle)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if botID == nil {
		return nil, httpx.NotFound(httpx.CodeNotFound, "No bot has that username")
	}

	if len([]rune(query)) > 256 {
		return nil, httpx.Validation("That query is too long").
			WithField("query", "at most 256 characters")
	}

	opened, err := s.repo.OpenInlineQuery(ctx, *botID, userID, query, offset)
	if err != nil {
		if errors.Is(err, ErrInlineNotEnabled) {
			return nil, httpx.Forbidden(httpx.CodeForbidden,
				"That bot does not answer inline queries")
		}
		return nil, httpx.Internal(err)
	}

	// The bot is told the query and who asked. It is not told where they are
	// typing, and there is nothing in this payload that would let it work that
	// out.
	if err := s.repo.Enqueue(ctx, *botID, UpdateInlineQuery, map[string]any{
		"id":     opened.ID,
		"from":   userID,
		"query":  query,
		"offset": offset,
	}); err != nil {
		s.logger.Warn("could not enqueue an inline query", errAttr(err))
	}
	return opened, nil
}

// AnswerInlineQuery is what a bot calls with its results.
func (s *Service) AnswerInlineQuery(ctx context.Context, botID, queryID uuid.UUID, results []InlineResult) error {
	if len(results) > MaxInlineResults {
		return httpx.Validation("Too many results").
			WithField("results", fmt.Sprintf("at most %d", MaxInlineResults))
	}

	seen := make(map[string]bool, len(results))
	for i := range results {
		result := &results[i]

		if strings.TrimSpace(result.ID) == "" {
			return httpx.Validation("Every result needs an id").
				WithField("results", "id is required")
		}
		// Duplicate ids would make a chosen result ambiguous, and the bot would
		// be told the wrong thing was picked.
		if seen[result.ID] {
			return httpx.Validation("Two results share an id").
				WithField("results", "each id must be distinct")
		}
		seen[result.ID] = true

		if _, ok := validInlineResultTypes[result.Type]; !ok {
			return httpx.Validation("Unsupported result type").
				WithField("results",
					"one of article, photo, video, audio, document, sticker, gif")
		}
		if strings.TrimSpace(result.Title) == "" {
			return httpx.Validation("Every result needs a title").
				WithField("results", "title is required")
		}
		if result.Type == "article" && strings.TrimSpace(result.Content) == "" {
			return httpx.Validation("An article result needs content").
				WithField("results", "content is required for article results")
		}
		if result.Type != "article" && result.MediaID == nil {
			return httpx.Validation("A media result needs a media id").
				WithField("results", "media_id is required for media results")
		}
		if result.Keyboard != nil {
			if err := result.Keyboard.Validate(); err != nil {
				return err
			}
		}
	}

	if err := s.repo.AnswerInlineQuery(ctx, botID, queryID, results); err != nil {
		if errors.Is(err, ErrQueryNotFound) {
			return httpx.NotFound(httpx.CodeNotFound,
				"That query is not open, or is not this bot's")
		}
		return httpx.Internal(err)
	}
	return nil
}

// InlineResults is what a client reads to draw the picker.
func (s *Service) InlineResults(ctx context.Context, userID, queryID uuid.UUID) ([]InlineResult, error) {
	results, _, _, err := s.repo.InlineResults(ctx, queryID, userID)
	if err != nil {
		if errors.Is(err, ErrQueryNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "That query is no longer open")
		}
		return nil, httpx.Internal(err)
	}
	if results == nil {
		results = []InlineResult{}
	}
	return results, nil
}

// ChooseInlineResult sends the result a person picked.
//
// The message is sent **by the person**, not by the bot: they chose it and it
// appears in their voice, which is also why the bot needs no membership of the
// chat it lands in. The bot is told afterwards which result was taken, and
// still not where it went.
func (s *Service) ChooseInlineResult(
	ctx context.Context,
	userID, queryID, chatID uuid.UUID,
	resultID string,
) (*messaging.Message, error) {
	results, botID, query, err := s.repo.InlineResults(ctx, queryID, userID)
	if err != nil {
		if errors.Is(err, ErrQueryNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "That query is no longer open")
		}
		return nil, httpx.Internal(err)
	}

	var chosen *InlineResult
	for i := range results {
		if results[i].ID == resultID {
			chosen = &results[i]
			break
		}
	}
	if chosen == nil {
		return nil, httpx.NotFound(httpx.CodeNotFound, "That result is not in this query")
	}

	messageType := validInlineResultTypes[chosen.Type]
	send := messaging.SendInput{
		ChatID:          chatID,
		SenderID:        userID,
		ClientMessageID: uuid.New(),
		Type:            messageType,
		Content:         chosen.Content,
	}
	if chosen.MediaID != nil {
		send.Attachments = []messaging.Attachment{{MediaID: *chosen.MediaID}}
	}

	// Sent through the ordinary path, so the person's own membership and
	// permissions decide whether it lands — picking a bot's result is not a way
	// to post somewhere you cannot post.
	message, err := s.messaging.Send(ctx, send)
	if err != nil {
		return nil, err
	}

	if err := s.repo.RecordChosenResult(ctx, botID, userID, queryID, resultID, query); err != nil {
		s.logger.Warn("could not record a chosen inline result", errAttr(err))
	}

	if err := s.repo.Enqueue(ctx, botID, UpdateChosenInlineResult, map[string]any{
		"result_id": resultID,
		"from":      userID,
		"query":     query,
	}); err != nil {
		s.logger.Warn("could not enqueue a chosen inline result", errAttr(err))
	}

	return message, nil
}
