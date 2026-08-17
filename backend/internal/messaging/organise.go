package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// Forwarding, pinning, scheduling and chat organisation (§12).
//
// These share a file because they all move a message or a conversation around
// rather than creating one, and they lean on the same membership checks.

// EventMessagePinned tells every member to redraw the pin bar.
const EventMessagePinned = "message.pinned"

// maxForwardBatch bounds one forward request. Forwarding is a fan-out — each
// message becomes a new row in the destination — so an unbounded batch is a
// cheap way to ask for an expensive write.
const maxForwardBatch = 100

// ForwardParams is one forward operation.
type ForwardParams struct {
	FromChatID uuid.UUID
	ToChatID   uuid.UUID
	MessageIDs []uuid.UUID
	SenderID   uuid.UUID
	// DropAuthor forwards without naming the original sender, which is what
	// "forward without quoting" means to a user.
	DropAuthor bool
}

// ForwardSource is the part of an original that a forward has to carry.
type ForwardSource struct {
	MessageID   uuid.UUID
	OriginChat  uuid.UUID
	OriginUser  *uuid.UUID
	Type        string
	Content     string
	Attachments []Attachment
	// Origin is set when the source was itself a forward, so a chain keeps
	// crediting whoever actually wrote the message.
	Origin *ForwardInfo
}

// ForwardSources reads the messages to forward, refusing any the caller cannot
// see.
//
// Membership is joined in the query rather than checked afterwards: a message
// id is guessable, and a separate check would leave a window in which the row
// has been read but the permission has not been decided.
func (r *Repository) ForwardSources(ctx context.Context, chatID, viewerID uuid.UUID, messageIDs []uuid.UUID) ([]ForwardSource, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.id, m.chat_id, m.sender_id, m.type, m.content,
		       m.forward_from_chat_id, m.forward_from_message_id,
		       m.forward_from_user_id, m.forward_signature
		  FROM messages m
		  JOIN chat_members cm
		    ON cm.chat_id = m.chat_id AND cm.user_id = $2 AND cm.left_at IS NULL
		 WHERE m.chat_id = $1
		   AND m.id = ANY($3::uuid[])
		   AND m.deleted_at IS NULL
		   AND m.seq IS NOT NULL
		 ORDER BY m.seq`,
		chatID, viewerID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("messaging: read forward sources: %w", err)
	}
	defer rows.Close()

	var sources []ForwardSource
	for rows.Next() {
		var (
			source          ForwardSource
			originChat      *uuid.UUID
			originMessage   *uuid.UUID
			originUser      *uuid.UUID
			originSignature string
		)
		if err := rows.Scan(&source.MessageID, &source.OriginChat, &source.OriginUser,
			&source.Type, &source.Content,
			&originChat, &originMessage, &originUser, &originSignature); err != nil {
			return nil, err
		}
		if originChat != nil || originUser != nil {
			source.Origin = &ForwardInfo{
				ChatID:    originChat,
				MessageID: originMessage,
				UserID:    originUser,
				Signature: originSignature,
			}
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Attachments live in their own table, so they are read in one further
	// query rather than one per message.
	return r.attachForwardSources(ctx, sources)
}

func (r *Repository) attachForwardSources(ctx context.Context, sources []ForwardSource) ([]ForwardSource, error) {
	if len(sources) == 0 {
		return sources, nil
	}

	ids := make([]uuid.UUID, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.MessageID)
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT message_id, media_id, position, caption
		  FROM message_attachments
		 WHERE message_id = ANY($1::uuid[])
		 ORDER BY message_id, position`, ids)
	if err != nil {
		return nil, fmt.Errorf("messaging: read forward attachments: %w", err)
	}
	defer rows.Close()

	byMessage := make(map[uuid.UUID][]Attachment, len(sources))
	for rows.Next() {
		var (
			messageID  uuid.UUID
			attachment Attachment
		)
		if err := rows.Scan(&messageID, &attachment.MediaID,
			&attachment.Position, &attachment.Caption); err != nil {
			return nil, err
		}
		byMessage[messageID] = append(byMessage[messageID], attachment)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range sources {
		sources[i].Attachments = byMessage[sources[i].MessageID]
	}
	return sources, nil
}

// SetChatFlags updates how the caller has filed a conversation.
//
// Pinning and archiving are per member, not per chat: one person archiving a
// group must not archive it for everyone else in it.
func (r *Repository) SetChatFlags(ctx context.Context, chatID, userID uuid.UUID, pinned, archived *bool) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_members
		   SET is_pinned   = COALESCE($3, is_pinned),
		       is_archived = COALESCE($4, is_archived)
		 WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
		chatID, userID, pinned, archived)
	if err != nil {
		return fmt.Errorf("messaging: set chat flags: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// SetMuted silences a conversation until a time; nil unmutes it.
func (r *Repository) SetMuted(ctx context.Context, chatID, userID uuid.UUID, until *time.Time) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE chat_members SET muted_until = $3
		  WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
		chatID, userID, until)
	if err != nil {
		return fmt.Errorf("messaging: set muted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// PinnedMessages lists what is pinned in a chat, newest first.
func (r *Repository) PinnedMessages(ctx context.Context, chatID, viewerID uuid.UUID, limit int) ([]Message, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.id, m.chat_id, m.seq, m.sender_id, m.type, m.content,
		       m.created_at, m.edited_at
		  FROM messages m
		  JOIN chat_members cm
		    ON cm.chat_id = m.chat_id AND cm.user_id = $2 AND cm.left_at IS NULL
		 WHERE m.chat_id = $1 AND m.is_pinned AND m.deleted_at IS NULL
		 ORDER BY m.seq DESC
		 LIMIT $3`, chatID, viewerID, limit)
	if err != nil {
		return nil, fmt.Errorf("messaging: read pinned: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var message Message
		if err := rows.Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID,
			&message.Type, &message.Content, &message.CreatedAt, &message.EditedAt); err != nil {
			return nil, err
		}
		message.IsPinned = true
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// ---------------------------------------------------------------- scheduling

// ScheduleParams is a message to publish later.
type ScheduleParams struct {
	ChatID          uuid.UUID
	SenderID        uuid.UUID
	ClientMessageID uuid.UUID
	Type            string
	Content         string
	Attachments     []Attachment
	PublishAt       time.Time
}

// ScheduledMessage is one queued post.
type ScheduledMessage struct {
	ID          uuid.UUID    `json:"id"`
	ChatID      uuid.UUID    `json:"chat_id"`
	Type        string       `json:"type"`
	Content     string       `json:"content"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ScheduledAt time.Time    `json:"scheduled_at"`
	CreatedAt   time.Time    `json:"created_at"`
}

// Schedule stores a message with no sequence number.
//
// A scheduled message deliberately gets no `seq` and no recipient events: it
// is not in the conversation yet. Publishing assigns the sequence, so ordering
// reflects when a message became visible rather than when it was written.
func (r *Repository) Schedule(ctx context.Context, in ScheduleParams) (*ScheduledMessage, error) {
	scheduled := &ScheduledMessage{
		ChatID:      in.ChatID,
		Type:        in.Type,
		Content:     in.Content,
		Attachments: in.Attachments,
		ScheduledAt: in.PublishAt,
	}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO messages (
				chat_id, client_message_id, sender_id, type, content, scheduled_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (chat_id, client_message_id) DO UPDATE
			SET scheduled_at = EXCLUDED.scheduled_at
			RETURNING id, created_at`,
			in.ChatID, in.ClientMessageID, in.SenderID, in.Type, in.Content, in.PublishAt,
		).Scan(&scheduled.ID, &scheduled.CreatedAt); err != nil {
			return fmt.Errorf("messaging: schedule: %w", err)
		}

		for _, attachment := range in.Attachments {
			if _, err := tx.Exec(ctx, `
				INSERT INTO message_attachments (message_id, media_id, position, caption)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT DO NOTHING`,
				scheduled.ID, attachment.MediaID, attachment.Position, attachment.Caption); err != nil {
				return fmt.Errorf("messaging: schedule attachment: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return scheduled, nil
}

// ScheduledFor lists what the caller has queued in a chat.
func (r *Repository) ScheduledFor(ctx context.Context, chatID, senderID uuid.UUID) ([]ScheduledMessage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, chat_id, type, content, scheduled_at, created_at
		  FROM messages
		 WHERE chat_id = $1 AND sender_id = $2
		   AND scheduled_at IS NOT NULL AND published_at IS NULL
		   AND deleted_at IS NULL
		 ORDER BY scheduled_at`, chatID, senderID)
	if err != nil {
		return nil, fmt.Errorf("messaging: read scheduled: %w", err)
	}
	defer rows.Close()

	var messages []ScheduledMessage
	for rows.Next() {
		var message ScheduledMessage
		if err := rows.Scan(&message.ID, &message.ChatID, &message.Type, &message.Content,
			&message.ScheduledAt, &message.CreatedAt); err != nil {
			return nil, err
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// CancelScheduled removes a queued message before it publishes.
func (r *Repository) CancelScheduled(ctx context.Context, messageID, senderID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		DELETE FROM messages
		 WHERE id = $1 AND sender_id = $2
		   AND scheduled_at IS NOT NULL AND published_at IS NULL`,
		messageID, senderID)
	if err != nil {
		return fmt.Errorf("messaging: cancel scheduled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ClaimDueScheduled takes ownership of messages whose time has come.
//
// The rows are selected FOR UPDATE SKIP LOCKED and stamped published in the
// same statement, so two publisher workers cannot both take one message.
func (r *Repository) ClaimDueScheduled(ctx context.Context, limit int) ([]uuid.UUID, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE messages SET published_at = now()
		 WHERE id IN (
		     SELECT id FROM messages
		      WHERE scheduled_at IS NOT NULL
		        AND published_at IS NULL
		        AND deleted_at IS NULL
		        AND scheduled_at <= now()
		      ORDER BY scheduled_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 RETURNING id`, limit)
	if err != nil {
		return nil, fmt.Errorf("messaging: claim due scheduled: %w", err)
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

// PublishScheduled gives a claimed message its sequence and recipient events,
// which is what puts it into the conversation.
func (r *Repository) PublishScheduled(ctx context.Context, messageID uuid.UUID) (*SendResult, error) {
	result := &SendResult{}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var (
			chatID      uuid.UUID
			senderID    uuid.UUID
			lastSeq     int64
			memberCount int
		)
		if err := tx.QueryRow(ctx,
			`SELECT chat_id, sender_id FROM messages WHERE id = $1`, messageID,
		).Scan(&chatID, &senderID); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: read scheduled message: %w", err)
		}

		// The same row lock the live send path takes, for the same reason:
		// two publishers must not hand out one sequence number twice.
		if err := tx.QueryRow(ctx,
			`SELECT last_seq, member_count FROM chats WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			chatID).Scan(&lastSeq, &memberCount); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: lock chat: %w", err)
		}
		seq := lastSeq + 1

		message := &Message{}
		if err := tx.QueryRow(ctx, `
			UPDATE messages SET seq = $2, created_at = now()
			 WHERE id = $1
			 RETURNING id, chat_id, seq, sender_id, client_message_id, type, content,
			           entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at`,
			messageID, seq,
		).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID,
			&message.ClientMessageID, &message.Type, &message.Content, &message.Entities,
			&message.Payload, &message.ReplyToID, &message.IsPinned, &message.CreatedAt,
			&message.EditedAt, &message.DeletedAt); err != nil {
			return fmt.Errorf("messaging: publish scheduled: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE chats
			SET last_seq = $2, last_message_id = $3, last_message_at = $4, updated_at = now()
			WHERE id = $1`, chatID, seq, message.ID, message.CreatedAt); err != nil {
			return fmt.Errorf("messaging: advance chat: %w", err)
		}

		// The author's own cursor moves with the message, exactly as on the
		// live path: it must never come back to them as unread.
		if _, err := tx.Exec(ctx, `
			UPDATE chat_members
			SET last_read_seq = $3, last_delivered_seq = $3
			WHERE chat_id = $1 AND user_id = $2`, chatID, senderID, seq); err != nil {
			return fmt.Errorf("messaging: advance sender cursor: %w", err)
		}

		result.Message = message

		// Above the threshold the chat is pull-based, so there is no
		// per-recipient log to write — the same split the live send makes.
		if memberCount > FanoutThreshold {
			return nil
		}
		result.FannedOut = true

		if _, err := tx.Exec(ctx, `
			UPDATE chat_members m
			SET unread_count = m.unread_count + 1
			WHERE m.chat_id = $1 AND m.user_id <> $2 AND m.left_at IS NULL`,
			chatID, senderID); err != nil {
			return fmt.Errorf("messaging: bump unread counts: %w", err)
		}

		payload, err := json.Marshal(map[string]any{
			"chat_id": chatID,
			"message": message,
		})
		if err != nil {
			return fmt.Errorf("messaging: marshal event: %w", err)
		}

		recipients, seqs, err := appendUserEvents(ctx, tx, chatID, senderID,
			EventMessageNew, payload)
		if err != nil {
			return err
		}
		result.Recipients = recipients
		result.EventSeqs = seqs
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------- service

// Forward copies messages into another chat.
func (s *Service) Forward(ctx context.Context, in ForwardParams) ([]*Message, error) {
	if len(in.MessageIDs) == 0 {
		return nil, httpx.Validation("Nothing to forward").
			WithField("message_ids", "at least one is required")
	}
	if len(in.MessageIDs) > maxForwardBatch {
		return nil, httpx.Validation("Too many messages in one forward").
			WithField("message_ids", fmt.Sprintf("at most %d", maxForwardBatch))
	}

	// The destination is checked first: being able to read the source says
	// nothing about being allowed to post into the target.
	target, err := s.repo.ChatContextFor(ctx, in.ToChatID, in.SenderID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !target.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	sources, err := s.repo.ForwardSources(ctx, in.FromChatID, in.SenderID, in.MessageIDs)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(sources) == 0 {
		return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Those messages are not available")
	}

	forwarded := make([]*Message, 0, len(sources))
	for _, source := range sources {
		// The first origin wins: forwarding a forward still credits whoever
		// wrote the message, not the person who passed it on.
		origin := source.Origin
		if origin == nil {
			originChat := source.OriginChat
			originMessage := source.MessageID
			origin = &ForwardInfo{
				ChatID:    &originChat,
				MessageID: &originMessage,
				UserID:    source.OriginUser,
			}
		}
		if in.DropAuthor {
			origin = &ForwardInfo{ChatID: origin.ChatID, MessageID: origin.MessageID}
		}

		message, err := s.Send(ctx, SendInput{
			ChatID:          in.ToChatID,
			SenderID:        in.SenderID,
			ClientMessageID: uuid.New(),
			Type:            source.Type,
			Content:         source.Content,
			Attachments:     source.Attachments,
			Forward:         origin,
		})
		if err != nil {
			// A rejection on the first message is the caller's answer; after
			// that, the ones already delivered are reported rather than lost.
			if len(forwarded) == 0 {
				return nil, err
			}
			break
		}
		forwarded = append(forwarded, message)
	}
	return forwarded, nil
}

// SetPinned pins or unpins a message, which needs the pin permission.
func (s *Service) SetPinned(ctx context.Context, messageID, userID uuid.UUID, pinned bool) error {
	message, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return httpx.Internal(err)
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, message.ChatID, userID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if !chatCtx.Permissions.PinMessages {
		return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot pin messages in this chat")
	}

	if _, err := s.repo.SetPinned(ctx, messageID, pinned); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return httpx.Internal(err)
	}

	// Everyone in the chat sees the pin bar, so the chat subject is the right
	// channel rather than a per-recipient event.
	s.publishChat(message.ChatID, EventMessagePinned, map[string]any{
		"message_id": messageID,
		"chat_id":    message.ChatID,
		"pinned":     pinned,
	})
	return nil
}

func (s *Service) PinnedMessages(ctx context.Context, chatID, viewerID uuid.UUID) ([]Message, error) {
	messages, err := s.repo.PinnedMessages(ctx, chatID, viewerID, 50)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return messages, nil
}

// SetChatFlags files a conversation for the caller alone.
func (s *Service) SetChatFlags(ctx context.Context, chatID, userID uuid.UUID, pinned, archived *bool) error {
	if err := s.repo.SetChatFlags(ctx, chatID, userID, pinned, archived); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) SetMuted(ctx context.Context, chatID, userID uuid.UUID, until *time.Time) error {
	if err := s.repo.SetMuted(ctx, chatID, userID, until); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		}
		return httpx.Internal(err)
	}
	return nil
}

// Schedule queues a message for later.
func (s *Service) Schedule(ctx context.Context, in ScheduleParams) (*ScheduledMessage, error) {
	if in.PublishAt.Before(time.Now().Add(10 * time.Second)) {
		return nil, httpx.Validation("That time has already passed").
			WithField("scheduled_at", "must be at least 10 seconds from now")
	}
	// A year is beyond any real use and stops a typo parking a row for ever.
	if in.PublishAt.After(time.Now().AddDate(1, 0, 0)) {
		return nil, httpx.Validation("That is too far in the future").
			WithField("scheduled_at", "at most one year ahead")
	}
	if in.ClientMessageID == uuid.Nil {
		return nil, httpx.Validation("client_message_id is required").
			WithField("client_message_id", "required so a retry is idempotent")
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, in.ChatID, in.SenderID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if !chatCtx.Permissions.SendMessages {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied, "You cannot post in this chat")
	}

	scheduled, err := s.repo.Schedule(ctx, in)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return scheduled, nil
}

func (s *Service) ScheduledFor(ctx context.Context, chatID, senderID uuid.UUID) ([]ScheduledMessage, error) {
	messages, err := s.repo.ScheduledFor(ctx, chatID, senderID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return messages, nil
}

func (s *Service) CancelScheduled(ctx context.Context, messageID, senderID uuid.UUID) error {
	if err := s.repo.CancelScheduled(ctx, messageID, senderID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound, "That message is not scheduled")
		}
		return httpx.Internal(err)
	}
	return nil
}

// PublishDue publishes everything whose time has come; the scheduler worker
// calls it. It returns how many were published.
func (s *Service) PublishDue(ctx context.Context, limit int) (int, error) {
	ids, err := s.repo.ClaimDueScheduled(ctx, limit)
	if err != nil {
		return 0, err
	}

	published := 0
	for _, id := range ids {
		result, err := s.repo.PublishScheduled(ctx, id)
		if err != nil {
			// One bad message must not stall the queue behind it.
			s.logger.Warn("could not publish a scheduled message",
				slog.String("message_id", id.String()), slog.Any("error", err))
			continue
		}

		chatCtx := &ChatContext{ChatID: result.Message.ChatID}
		s.broadcast(ctx, chatCtx, EventMessageNew, result, map[string]any{
			"message": result.Message,
		})
		published++
	}
	return published, nil
}

// ---------------------------------------------------------------- handler

// RegisterOrganiseRoutes adds the routes that move messages and chats around.
//
// They register into the same /chats router as the rest of messaging rather
// than mounting a second one, because chi permits only one Mount per pattern.
func (h *Handler) RegisterOrganiseRoutes(r chi.Router) {
	r.Post("/{chatID}/forward", h.forward)
	r.Get("/{chatID}/pinned", h.pinnedMessages)
	r.Put("/{chatID}/flags", h.setChatFlags)
	r.Put("/{chatID}/mute", h.setMuted)
	r.Get("/{chatID}/scheduled", h.scheduled)
	r.Post("/{chatID}/scheduled", h.schedule)
	r.Delete("/{chatID}/scheduled/{messageID}", h.cancelScheduled)
}

// RegisterPinRoute adds pinning to the /messages router, where the message id
// is already the path parameter.
func (h *Handler) RegisterPinRoute(r chi.Router) {
	r.Put("/{messageID}/pin", h.setPinned)
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ToChatID   uuid.UUID   `json:"to_chat_id"`
		MessageIDs []uuid.UUID `json:"message_ids"`
		DropAuthor bool        `json:"drop_author"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	messages, err := h.service.Forward(r.Context(), ForwardParams{
		FromChatID: chatID,
		ToChatID:   body.ToChatID,
		MessageIDs: body.MessageIDs,
		SenderID:   principal.UserID,
		DropAuthor: body.DropAuthor,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"messages": messages})
}

func (h *Handler) setPinned(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	messageID, err := pathUUID(r, "messageID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetPinned(r.Context(), messageID, principal.UserID, body.Pinned); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) pinnedMessages(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	messages, err := h.service.PinnedMessages(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"messages": messages})
}

func (h *Handler) setChatFlags(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Pinned   *bool `json:"pinned"`
		Archived *bool `json:"archived"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetChatFlags(r.Context(), chatID, principal.UserID,
		body.Pinned, body.Archived); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setMuted(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		// Null unmutes; a far-future time is how a client says "for ever".
		MutedUntil *time.Time `json:"muted_until"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetMuted(r.Context(), chatID, principal.UserID, body.MutedUntil); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) scheduled(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	messages, err := h.service.ScheduledFor(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"messages": messages})
}

func (h *Handler) schedule(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ClientMessageID uuid.UUID    `json:"client_message_id"`
		Type            string       `json:"type"`
		Content         string       `json:"content"`
		Attachments     []Attachment `json:"attachments"`
		ScheduledAt     time.Time    `json:"scheduled_at"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Type == "" {
		body.Type = TypeText
	}

	scheduled, err := h.service.Schedule(r.Context(), ScheduleParams{
		ChatID:          chatID,
		SenderID:        principal.UserID,
		ClientMessageID: body.ClientMessageID,
		Type:            body.Type,
		Content:         body.Content,
		Attachments:     body.Attachments,
		PublishAt:       body.ScheduledAt,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, scheduled)
}

func (h *Handler) cancelScheduled(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	messageID, err := pathUUID(r, "messageID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.CancelScheduled(r.Context(), messageID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}
