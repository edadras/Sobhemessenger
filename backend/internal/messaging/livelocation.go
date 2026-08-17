package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// Live location (§12).
//
// A moving position updates the message it was shared in rather than posting a
// new one. Sending one message per reading would bury a conversation under a
// position log, and the useful question is "where are they now", not "where
// have they been".

// EventLocationUpdated tells a client to move the pin it is already drawing.
const EventLocationUpdated = "message.location_updated"

// UpdateLiveLocation moves the point on a message the caller shared.
//
// Only the author may move it, and only while the sharing period is still
// running: a stopped share that could be restarted by another update would
// mean "stop sharing" did not mean it.
func (r *Repository) UpdateLiveLocation(
	ctx context.Context,
	messageID, authorID uuid.UUID,
	latitude, longitude, accuracy, heading, speed float64,
) (*Message, uuid.UUID, error) {
	var (
		message  Message
		chatID   uuid.UUID
		rawStore []byte
	)

	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, chat_id, payload FROM messages
		 WHERE id = $1 AND sender_id = $2 AND type = $3 AND deleted_at IS NULL`,
		messageID, authorID, TypeLocation).Scan(&message.ID, &chatID, &rawStore)
	if database.IsNoRows(err) {
		return nil, uuid.Nil, ErrNotFound
	}
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("messaging: read live location: %w", err)
	}

	var location LocationPayload
	if err := json.Unmarshal(rawStore, &location); err != nil {
		return nil, uuid.Nil, fmt.Errorf("messaging: decode live location: %w", err)
	}
	if !location.IsLive() {
		return nil, uuid.Nil, ErrNotLive
	}

	location.Latitude = latitude
	location.Longitude = longitude
	location.HorizontalAccuracy = accuracy
	location.Heading = heading
	location.Speed = speed
	// Validate would recompute LiveUntil from LivePeriodSeconds and so extend
	// the share on every update, which is exactly what must not happen. The
	// bounds are checked without it.
	until := location.LiveUntil
	if err := location.Validate(); err != nil {
		return nil, uuid.Nil, err
	}
	location.LiveUntil = until

	encoded, err := json.Marshal(location)
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("messaging: encode live location: %w", err)
	}

	// edited_at is deliberately not touched: a moving pin is not an edit, and
	// showing "edited" every few seconds would be noise.
	if err := r.db.Pool.QueryRow(ctx, `
		UPDATE messages SET payload = $2::jsonb
		 WHERE id = $1
		 RETURNING id, chat_id, seq, sender_id, client_message_id, type, content,
		           entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at`,
		messageID, encoded,
	).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID,
		&message.ClientMessageID, &message.Type, &message.Content, &message.Entities,
		&message.Payload, &message.ReplyToID, &message.IsPinned, &message.CreatedAt,
		&message.EditedAt, &message.DeletedAt); err != nil {
		return nil, uuid.Nil, fmt.Errorf("messaging: update live location: %w", err)
	}

	return &message, chatID, nil
}

// StopLiveLocation ends a share early.
func (r *Repository) StopLiveLocation(ctx context.Context, messageID, authorID uuid.UUID) (uuid.UUID, error) {
	var (
		chatID   uuid.UUID
		rawStore []byte
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT chat_id, payload FROM messages
		 WHERE id = $1 AND sender_id = $2 AND type = $3 AND deleted_at IS NULL`,
		messageID, authorID, TypeLocation).Scan(&chatID, &rawStore)
	if database.IsNoRows(err) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("messaging: read live location: %w", err)
	}

	var location LocationPayload
	if err := json.Unmarshal(rawStore, &location); err != nil {
		return uuid.Nil, fmt.Errorf("messaging: decode live location: %w", err)
	}
	if !location.IsLive() {
		return uuid.Nil, ErrNotLive
	}

	// Expiring it in the past is what stops it, rather than deleting the
	// period: the message stays as a record that a location was shared, and
	// the last point remains visible.
	stopped := time.Now()
	location.LiveUntil = &stopped

	encoded, err := json.Marshal(location)
	if err != nil {
		return uuid.Nil, fmt.Errorf("messaging: encode live location: %w", err)
	}
	if _, err := r.db.Pool.Exec(ctx,
		`UPDATE messages SET payload = $2::jsonb WHERE id = $1`, messageID, encoded); err != nil {
		return uuid.Nil, fmt.Errorf("messaging: stop live location: %w", err)
	}
	return chatID, nil
}

// ErrNotLive is returned for a location that is not being shared any more.
var ErrNotLive = errors.New("messaging: that location is not live")

// ---------------------------------------------------------------- service

// UpdateLiveLocation moves a shared position and tells the chat.
func (s *Service) UpdateLiveLocation(
	ctx context.Context,
	messageID, authorID uuid.UUID,
	latitude, longitude, accuracy, heading, speed float64,
) (*Message, error) {
	message, chatID, err := s.repo.UpdateLiveLocation(ctx, messageID, authorID,
		latitude, longitude, accuracy, heading, speed)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return nil, httpx.NotFound(httpx.CodeMessageNotFound,
				"That is not a location you shared")
		case errors.Is(err, ErrNotLive):
			return nil, httpx.Conflict(httpx.CodeConflict,
				"That location is no longer being shared")
		default:
			var apiErr *httpx.Error
			if errors.As(err, &apiErr) {
				return nil, apiErr
			}
			return nil, httpx.Internal(err)
		}
	}

	// The chat subject rather than a per-recipient event: a position that moves
	// every few seconds must not write a sync event per member per reading, and
	// a client that was not watching does not need the intermediate points.
	s.publishChat(chatID, EventLocationUpdated, map[string]any{
		"chat_id": chatID,
		"message": message,
	})
	return message, nil
}

// StopLiveLocation ends a share before its period runs out.
func (s *Service) StopLiveLocation(ctx context.Context, messageID, authorID uuid.UUID) error {
	chatID, err := s.repo.StopLiveLocation(ctx, messageID, authorID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return httpx.NotFound(httpx.CodeMessageNotFound,
				"That is not a location you shared")
		case errors.Is(err, ErrNotLive):
			return httpx.Conflict(httpx.CodeConflict,
				"That location is no longer being shared")
		default:
			return httpx.Internal(err)
		}
	}

	s.publishChat(chatID, EventLocationUpdated, map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"stopped":    true,
	})
	return nil
}
