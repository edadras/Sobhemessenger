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

// maxScheduledPublishAttempts bounds how often a stuck post is retried.
//
// A post whose chat has been deleted, or whose author has been removed from it,
// can never publish. Retrying it every tick for ever would keep the publisher
// busy and bury real failures in the log, so after this many tries it is
// abandoned — recorded with its error, not deleted, so the author can be told.
const maxScheduledPublishAttempts = 5

// scheduledClaimLease is how long a claimed post stays out of the queue.
//
// Long enough that a publisher working through a batch is never handed its own
// backlog again, short enough that a publisher killed mid-batch releases its
// work within one tick or two rather than leaving posts stranded.
const scheduledClaimLease = 2 * time.Minute

// ScheduleParams is a message to publish later.
type ScheduleParams struct {
	ChatID          uuid.UUID
	SenderID        uuid.UUID
	ClientMessageID uuid.UUID
	Type            string
	Content         string
	Attachments     []Attachment
	ReplyToID       *uuid.UUID
	IsSilent        bool
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
	// Attempts and LastError are how the compose screen explains a post that
	// did not go out.
	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// Schedule stores a post in the queue, not in the conversation.
//
// It deliberately does not touch `messages`: a queued post has no sequence
// number, and a seq-less row in that table would sort to the top of history.
// It becomes a message at publication, through the ordinary send path.
func (r *Repository) Schedule(ctx context.Context, in ScheduleParams) (*ScheduledMessage, error) {
	attachments, err := json.Marshal(in.Attachments)
	if err != nil {
		return nil, fmt.Errorf("messaging: encode scheduled attachments: %w", err)
	}
	if in.Attachments == nil {
		attachments = []byte("[]")
	}

	scheduled := &ScheduledMessage{
		ChatID:      in.ChatID,
		Type:        in.Type,
		Content:     in.Content,
		Attachments: in.Attachments,
		ScheduledAt: in.PublishAt,
	}

	// Re-sending the same compose request reschedules rather than queueing a
	// second copy, which is what makes a retry over a flaky link safe. A row
	// that has already gone out is not resurrected: the WHERE on the DO UPDATE
	// leaves it alone, and the caller is told it is too late.
	err = r.db.Pool.QueryRow(ctx, `
		INSERT INTO scheduled_messages (
			chat_id, sender_id, client_message_id, type, content,
			attachments, reply_to_id, is_silent, scheduled_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9)
		ON CONFLICT (chat_id, sender_id, client_message_id) DO UPDATE
		SET type         = EXCLUDED.type,
		    content      = EXCLUDED.content,
		    attachments  = EXCLUDED.attachments,
		    reply_to_id  = EXCLUDED.reply_to_id,
		    is_silent    = EXCLUDED.is_silent,
		    scheduled_at = EXCLUDED.scheduled_at,
		    attempts     = 0,
		    last_error   = '',
		    abandoned_at = NULL,
		    -- Editing a queued post gives it a fresh start, including its
		    -- place in the queue: a lease from an earlier attempt must not
		    -- delay the revised version.
		    claimed_until = NULL,
		    updated_at   = now()
		WHERE scheduled_messages.published_at IS NULL
		RETURNING id, created_at`,
		in.ChatID, in.SenderID, in.ClientMessageID, in.Type, in.Content,
		attachments, in.ReplyToID, in.IsSilent, in.PublishAt,
	).Scan(&scheduled.ID, &scheduled.CreatedAt)
	if database.IsNoRows(err) {
		// The DO UPDATE matched nothing, so the row exists and has published.
		return nil, ErrAlreadyPublished
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: schedule: %w", err)
	}
	return scheduled, nil
}

// ScheduledFor lists what the caller has queued in a chat.
func (r *Repository) ScheduledFor(ctx context.Context, chatID, senderID uuid.UUID) ([]ScheduledMessage, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, chat_id, type, content, attachments, scheduled_at, created_at,
		       attempts, last_error
		  FROM scheduled_messages
		 WHERE chat_id = $1 AND sender_id = $2 AND published_at IS NULL
		 ORDER BY scheduled_at`, chatID, senderID)
	if err != nil {
		return nil, fmt.Errorf("messaging: read scheduled: %w", err)
	}
	defer rows.Close()

	var messages []ScheduledMessage
	for rows.Next() {
		var (
			message   ScheduledMessage
			rawAttach []byte
		)
		if err := rows.Scan(&message.ID, &message.ChatID, &message.Type, &message.Content,
			&rawAttach, &message.ScheduledAt, &message.CreatedAt,
			&message.Attempts, &message.LastError); err != nil {
			return nil, err
		}
		if len(rawAttach) > 0 {
			if err := json.Unmarshal(rawAttach, &message.Attachments); err != nil {
				return nil, fmt.Errorf("messaging: decode scheduled attachments: %w", err)
			}
		}
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

// CancelScheduled removes a queued post before it publishes.
func (r *Repository) CancelScheduled(ctx context.Context, scheduledID, senderID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		DELETE FROM scheduled_messages
		 WHERE id = $1 AND sender_id = $2 AND published_at IS NULL`,
		scheduledID, senderID)
	if err != nil {
		return fmt.Errorf("messaging: cancel scheduled: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// DueScheduled is a claimed post, ready to be sent.
type DueScheduled struct {
	ID              uuid.UUID
	ChatID          uuid.UUID
	SenderID        uuid.UUID
	ClientMessageID uuid.UUID
	Type            string
	Content         string
	Attachments     []Attachment
	ReplyToID       *uuid.UUID
	IsSilent        bool
	Attempts        int
}

// ClaimDueScheduled takes ownership of posts whose time has come.
//
// Two things separate two publishers here, and they do different jobs.
// FOR UPDATE SKIP LOCKED separates workers claiming at the same instant: each
// gets a different set and neither waits. The lease separates them over time —
// a claimed post is invisible to the queue until the lease expires, so a
// publisher part-way through a batch is never handed its own backlog again.
// Without the lease every tick would re-offer the same posts and spend one of
// each post's attempts, and a publisher that was merely slow would abandon
// perfectly healthy posts.
//
// Neither is what makes publication exactly-once. The claim is not publication:
// the row is marked published only once a message exists. A worker that dies in
// between leaves a lease that expires and the post is claimed again — which is
// safe because the send carries the same client_message_id, so the retry
// resolves to the message already created rather than making a second one.
func (r *Repository) ClaimDueScheduled(ctx context.Context, limit int) ([]DueScheduled, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE scheduled_messages s
		   SET attempts      = s.attempts + 1,
		       claimed_until = now() + $2::interval,
		       updated_at    = now()
		 WHERE s.id IN (
		     SELECT id FROM scheduled_messages
		      WHERE published_at IS NULL
		        AND abandoned_at IS NULL
		        AND scheduled_at <= now()
		        AND (claimed_until IS NULL OR claimed_until <= now())
		      ORDER BY scheduled_at
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		 RETURNING s.id, s.chat_id, s.sender_id, s.client_message_id, s.type,
		           s.content, s.attachments, s.reply_to_id, s.is_silent, s.attempts`,
		limit, scheduledClaimLease.String())
	if err != nil {
		return nil, fmt.Errorf("messaging: claim due scheduled: %w", err)
	}
	defer rows.Close()

	var due []DueScheduled
	for rows.Next() {
		var (
			item      DueScheduled
			rawAttach []byte
		)
		if err := rows.Scan(&item.ID, &item.ChatID, &item.SenderID, &item.ClientMessageID,
			&item.Type, &item.Content, &rawAttach, &item.ReplyToID, &item.IsSilent,
			&item.Attempts); err != nil {
			return nil, err
		}
		if len(rawAttach) > 0 {
			if err := json.Unmarshal(rawAttach, &item.Attachments); err != nil {
				return nil, fmt.Errorf("messaging: decode scheduled attachments: %w", err)
			}
		}
		due = append(due, item)
	}
	return due, rows.Err()
}

// MarkScheduledPublished records which message a queued post became.
func (r *Repository) MarkScheduledPublished(ctx context.Context, scheduledID, messageID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE scheduled_messages
		   SET published_at = now(), published_message_id = $2,
		       last_error = '', updated_at = now()
		 WHERE id = $1 AND published_at IS NULL`, scheduledID, messageID)
	if err != nil {
		return fmt.Errorf("messaging: mark scheduled published: %w", err)
	}
	return nil
}

// MarkScheduledFailed records why a post did not go out, and abandons it once
// it has failed too often to be worth retrying.
func (r *Repository) MarkScheduledFailed(ctx context.Context, scheduledID uuid.UUID, reason string) error {
	// The reason is stored bounded: it comes from an error string, which can
	// carry a whole driver message.
	if len([]rune(reason)) > 500 {
		reason = string([]rune(reason)[:500])
	}
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE scheduled_messages
		   SET last_error   = $2,
		       abandoned_at = CASE WHEN attempts >= $3 THEN now() ELSE abandoned_at END,
		       updated_at   = now()
		 WHERE id = $1 AND published_at IS NULL`,
		scheduledID, reason, maxScheduledPublishAttempts)
	if err != nil {
		return fmt.Errorf("messaging: mark scheduled failed: %w", err)
	}
	return nil
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
		if errors.Is(err, ErrAlreadyPublished) {
			return nil, httpx.Conflict(httpx.CodeConflict,
				"That message has already been sent")
		}
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

func (s *Service) CancelScheduled(ctx context.Context, scheduledID, senderID uuid.UUID) error {
	if err := s.repo.CancelScheduled(ctx, scheduledID, senderID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound, "That message is not scheduled")
		}
		return httpx.Internal(err)
	}
	return nil
}

// PublishDue publishes everything whose time has come; the scheduler worker
// calls it. It returns how many were published.
//
// Each post goes out through Send, the same path a live message takes. That is
// deliberate: sequence allocation, recipient events, unread counts, fan-out and
// the permission re-check are all one implementation, so a scheduled message
// cannot drift into behaving differently from a typed one. It also makes
// publication exactly-once for free — the send carries the queued row's
// client_message_id, so a worker that dies before recording the result re-sends
// and the idempotency key returns the message already created.
func (s *Service) PublishDue(ctx context.Context, limit int) (int, error) {
	due, err := s.repo.ClaimDueScheduled(ctx, limit)
	if err != nil {
		return 0, err
	}

	published := 0
	for _, item := range due {
		// The author's right to post is re-checked at publication, not trusted
		// from when they queued it: they may have been muted, restricted or
		// removed from the chat in between.
		message, err := s.Send(ctx, SendInput{
			ChatID:          item.ChatID,
			SenderID:        item.SenderID,
			ClientMessageID: item.ClientMessageID,
			Type:            item.Type,
			Content:         item.Content,
			Attachments:     item.Attachments,
			ReplyToID:       item.ReplyToID,
			IsSilent:        item.IsSilent,
		})
		if err != nil {
			// One bad post must not stall the queue behind it. The reason is
			// recorded on the row so the author can be shown why.
			s.logger.Warn("could not publish a scheduled message",
				slog.String("scheduled_id", item.ID.String()),
				slog.Int("attempts", item.Attempts),
				slog.Any("error", err))
			if markErr := s.repo.MarkScheduledFailed(ctx, item.ID, err.Error()); markErr != nil {
				s.logger.Error("could not record a scheduled publish failure",
					slog.String("scheduled_id", item.ID.String()), slog.Any("error", markErr))
			}
			continue
		}

		if err := s.repo.MarkScheduledPublished(ctx, item.ID, message.ID); err != nil {
			// The message is out; failing to record that only risks a repeat
			// attempt, which the idempotency key makes harmless.
			s.logger.Error("could not record a scheduled publication",
				slog.String("scheduled_id", item.ID.String()), slog.Any("error", err))
		}
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
	r.Delete("/{chatID}/scheduled/{scheduledID}", h.cancelScheduled)
}

// RegisterPinRoute adds pinning to the /messages router, where the message id
// is already the path parameter.
func (h *Handler) RegisterPinRoute(r chi.Router) {
	r.Put("/{messageID}/pin", h.setPinned)
	r.Put("/{messageID}/live-location", h.updateLiveLocation)
	r.Delete("/{messageID}/live-location", h.stopLiveLocation)
}

func (h *Handler) updateLiveLocation(w http.ResponseWriter, r *http.Request) {
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
		Latitude           float64 `json:"latitude"`
		Longitude          float64 `json:"longitude"`
		HorizontalAccuracy float64 `json:"horizontal_accuracy"`
		Heading            float64 `json:"heading"`
		Speed              float64 `json:"speed"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	message, err := h.service.UpdateLiveLocation(r.Context(), messageID, principal.UserID,
		body.Latitude, body.Longitude, body.HorizontalAccuracy, body.Heading, body.Speed)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, message)
}

func (h *Handler) stopLiveLocation(w http.ResponseWriter, r *http.Request) {
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

	if err := h.service.StopLiveLocation(r.Context(), messageID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
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
		ReplyToID       *uuid.UUID   `json:"reply_to_id"`
		IsSilent        bool         `json:"is_silent"`
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
		ReplyToID:       body.ReplyToID,
		IsSilent:        body.IsSilent,
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
	scheduledID, err := pathUUID(r, "scheduledID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.CancelScheduled(r.Context(), scheduledID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}
