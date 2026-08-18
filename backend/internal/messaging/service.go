package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
)

// Limits on message content.
const (
	MaxContentRunes    = 8000
	MaxCaptionRunes    = 2000
	MaxAttachments     = 10
	MaxMentions        = 50
	EditWindow         = 48 * time.Hour
	MaxHistoryPageSize = 200
)

// MessageObserver is told about a message once it has been delivered.
//
// It exists so the bot platform can see traffic without messaging importing
// it: bots already depends on messaging to send, and a dependency the other
// way would be a cycle. An observer is told after delivery and its failures
// are its own — nothing it does can fail the send that triggered it.
type MessageObserver interface {
	ObserveMessage(ctx context.Context, chatCtx *ChatContext, message *Message)
}

// Service applies the messaging rules — permissions, slow mode, edit windows,
// rate limits — on top of the repository.
type Service struct {
	repo    *Repository
	bus     *bus.Bus
	limiter *ratelimit.Limiter
	rules   ratelimit.Rules
	metrics *observability.Metrics
	logger  *slog.Logger

	// observers is written once during assembly and only read afterwards, so
	// it needs no lock.
	observers []MessageObserver
}

// AddObserver registers an observer. Called during assembly, before the server
// accepts a request.
func (s *Service) AddObserver(observer MessageObserver) {
	s.observers = append(s.observers, observer)
}

func NewService(repo *Repository, messageBus *bus.Bus, limiter *ratelimit.Limiter, rules ratelimit.Rules, metrics *observability.Metrics, logger *slog.Logger) *Service {
	return &Service{repo: repo, bus: messageBus, limiter: limiter, rules: rules, metrics: metrics, logger: logger}
}

// SendInput is a validated send request from the API or WebSocket.
type SendInput struct {
	ChatID          uuid.UUID
	SenderID        uuid.UUID
	ClientMessageID uuid.UUID
	Type            string
	Content         string
	Entities        json.RawMessage
	Payload         json.RawMessage
	ReplyToID       *uuid.UUID
	Attachments     []Attachment
	Mentions        []uuid.UUID
	// Forward is set when this send is a forward, and names the original
	// author rather than the person passing it on.
	Forward *ForwardInfo
	// ReplyMarkup is an inline keyboard. The REST handler never reads it from a
	// request body, so a person cannot set one: only the bot service does, and
	// only for a bot's own message. A keyboard from an arbitrary user would be
	// a way to make everyone else's client render a callback target of their
	// choosing.
	ReplyMarkup json.RawMessage
	IsSilent    bool
}

// Send validates, authorises, persists and broadcasts a message.
func (s *Service) Send(ctx context.Context, in SendInput) (*Message, error) {
	start := time.Now()

	if err := s.validateSend(&in); err != nil {
		return nil, err
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
	if err := s.checkSendPermission(chatCtx, in); err != nil {
		return nil, err
	}

	allowed, err := s.limiter.Allow(ctx, s.rules.MessagesPerUser, in.SenderID.String())
	if err != nil {
		s.logger.Warn("message rate limiter unavailable", slog.Any("error", err))
	}
	if !allowed.Allowed {
		return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
	}

	// Slow mode: members wait between posts; staff are exempt (§14).
	if chatCtx.SlowMode > 0 && chatCtx.Role == RoleMember {
		lastAt, err := s.repo.LastSendAt(ctx, in.ChatID, in.SenderID)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		if !lastAt.IsZero() {
			wait := time.Duration(chatCtx.SlowMode)*time.Second - time.Since(lastAt)
			if wait > 0 {
				e := httpx.Forbidden(httpx.CodeSlowModeActive, "Slow mode is active in this chat")
				e.RetryAfter = int(wait.Seconds()) + 1
				return nil, e
			}
		}
	}

	result, err := s.repo.Send(ctx, SendParams{
		ChatID:          in.ChatID,
		SenderID:        in.SenderID,
		ClientMessageID: in.ClientMessageID,
		Type:            in.Type,
		Content:         in.Content,
		Entities:        in.Entities,
		Payload:         in.Payload,
		ReplyToID:       in.ReplyToID,
		Attachments:     in.Attachments,
		MentionUserIDs:  in.Mentions,
		Forward:         in.Forward,
		ReplyMarkup:     in.ReplyMarkup,
		IsSilent:        in.IsSilent,
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}

	if result.Duplicate {
		// The client retried; return the original so its outbox settles.
		return result.Message, nil
	}

	s.broadcast(ctx, chatCtx, EventMessageNew, result, map[string]any{
		"chat_id": in.ChatID,
		"message": result.Message,
	})

	s.notifyObservers(ctx, chatCtx, result.Message)
	s.indexForSearch(ctx, chatCtx, result.Message)

	s.metrics.MessagesSent.WithLabelValues(chatCtx.ChatType, in.Type).Inc()
	s.metrics.MessageSendLatency.Observe(time.Since(start).Seconds())
	return result.Message, nil
}

// indexForSearch queues a message for the search index (§26).
//
// Queued rather than written here: indexing is not part of delivering a
// message, and a search cluster that is slow or down must not slow down or fail
// a send. The job is durable, so an index that was unreachable catches up
// rather than losing the message from search for ever.
//
// Nothing from an encrypted chat is ever indexed — the server has not seen the
// text and never will (§24, §61). The chat-type guard on sending already makes
// this unreachable; it is repeated because "the other check covers it" is how a
// leak survives a refactor.
func (s *Service) indexForSearch(ctx context.Context, chatCtx *ChatContext, message *Message) {
	if chatCtx.ChatType == ChatSecret || message.Content == "" {
		return
	}
	// The deduplication key names the operation and the version of the text.
	//
	// PublishJob suppresses a repeated key inside JetStream's duplicate window,
	// which is what makes a retried publish harmless — but it also means two
	// different jobs sharing a key silently become one. An edit must not be
	// mistaken for a repeat of the original send, so the edit timestamp is part
	// of the key.
	version := int64(0)
	if message.EditedAt != nil {
		version = message.EditedAt.UnixNano()
	}
	s.enqueueIndex(ctx, fmt.Sprintf("message-index-%s-%d", message.ID, version), message.ID,
		map[string]any{
			"index":       "messages",
			"document_id": message.ID.String(),
			"document": map[string]any{
				"id":         message.ID,
				"chat_id":    message.ChatID,
				"sender_id":  message.SenderID,
				"content":    message.Content,
				"seq":        message.Seq,
				"created_at": message.CreatedAt,
			},
		})
}

// removeFromSearch takes a message out of the index.
//
// A deleted message that stays searchable is a deletion that did not happen as
// far as the person who asked for it is concerned.
func (s *Service) removeFromSearch(ctx context.Context, messageID uuid.UUID) {
	// A different key from any index job for the same message: sharing one
	// would let the removal be swallowed as a duplicate of the send, leaving a
	// deleted message searchable for ever.
	s.enqueueIndex(ctx, "message-delete-"+messageID.String(), messageID,
		map[string]any{
			"index":       "messages",
			"document_id": messageID.String(),
			"delete":      true,
		})
}

func (s *Service) enqueueIndex(ctx context.Context, dedupeID string, messageID uuid.UUID, payload map[string]any) {
	if err := s.bus.PublishJob(ctx, bus.SubjectJobSearchIndex, dedupeID, payload); err != nil {
		// Logged, never returned: the message itself is already delivered and
		// durable, and failing the caller now would be reporting a problem they
		// cannot act on about work that already succeeded.
		s.logger.Error("could not enqueue message indexing",
			slog.String("message_id", messageID.String()), slog.Any("error", err))
	}
}

// notifyObservers tells each observer about a delivered message.
//
// A panic in an observer is contained here. A bot platform fault must not turn
// into a failed send for the person who wrote the message — they have already
// been told it went.
func (s *Service) notifyObservers(ctx context.Context, chatCtx *ChatContext, message *Message) {
	for _, observer := range s.observers {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					s.logger.Error("a message observer panicked",
						slog.Any("panic", recovered),
						slog.String("message_id", message.ID.String()))
				}
			}()
			observer.ObserveMessage(ctx, chatCtx, message)
		}()
	}
}

func (s *Service) validateSend(in *SendInput) error {
	if !ValidMessageTypes[in.Type] {
		return httpx.Validation("Unsupported message type").WithField("type", "unsupported")
	}
	if in.Type == TypeSystem {
		return httpx.Forbidden(httpx.CodeForbidden, "System messages cannot be sent by clients")
	}
	if in.ClientMessageID == uuid.Nil {
		return httpx.Validation("client_message_id is required").
			WithField("client_message_id", "required for idempotent delivery")
	}

	in.Content = strings.TrimSpace(in.Content)
	if utf8.RuneCountInString(in.Content) > MaxContentRunes {
		return httpx.Validation("Message is too long").
			WithField("content", "exceeds the maximum length")
	}
	if len(in.Attachments) > MaxAttachments {
		return httpx.Validation("Too many attachments").
			WithField("attachments", "at most 10 per message")
	}
	if len(in.Mentions) > MaxMentions {
		return httpx.Validation("Too many mentions").WithField("mentions", "at most 50 per message")
	}
	for _, attachment := range in.Attachments {
		if utf8.RuneCountInString(attachment.Caption) > MaxCaptionRunes {
			return httpx.Validation("Caption is too long").WithField("caption", "exceeds the maximum length")
		}
	}

	// A message must carry something: text, media, or a typed payload.
	needsBody := in.Type == TypeText
	if needsBody && in.Content == "" && len(in.Attachments) == 0 {
		return httpx.Validation("Message is empty").WithField("content", "must not be empty")
	}
	// A location and a contact are structured, and a client draws a map pin or
	// a save-number button from them. Arbitrary JSON in that column is a crash
	// in somebody else's renderer, so it is checked here rather than hoped for.
	normalized, err := validateTypedPayload(in.Type, in.Payload)
	if err != nil {
		return err
	}
	in.Payload = normalized
	if in.Entities != nil && !json.Valid(in.Entities) {
		return httpx.Validation("entities is not valid JSON").WithField("entities", "invalid JSON")
	}
	if in.Payload != nil && !json.Valid(in.Payload) {
		return httpx.Validation("payload is not valid JSON").WithField("payload", "invalid JSON")
	}
	if in.ReplyMarkup != nil && !json.Valid(in.ReplyMarkup) {
		return httpx.Validation("reply_markup is not valid JSON").
			WithField("reply_markup", "invalid JSON")
	}
	return nil
}

func (s *Service) checkSendPermission(chatCtx *ChatContext, in SendInput) error {
	// An encrypted chat is not reachable through this path, ever (§23, §24).
	//
	// Nothing else here would have stopped it: the sender is a member, has the
	// send permission, and the row would have been written to `messages` in
	// cleartext — the conversation would silently stop being encrypted, with no
	// error and nothing on either screen to say so. That is the worst possible
	// failure for this feature, so it is refused at the server rather than left
	// to the client to avoid. A client is a thing an attacker gets to rewrite.
	//
	// Forward routes every message through Send, so it is covered here too.
	if chatCtx.ChatType == ChatSecret {
		return httpx.Validation("That chat is encrypted, so messages must be sent through the encrypted path").
			WithField("chat_id", "is a secret chat")
	}

	permissions := chatCtx.Permissions
	if !permissions.SendMessages {
		return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot post in this chat")
	}

	switch in.Type {
	case TypeImage, TypeVideo, TypeAudio, TypeVoice:
		if !permissions.SendMedia {
			return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot send media in this chat")
		}
	case TypeFile:
		if !permissions.SendFiles {
			return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot send files in this chat")
		}
	case TypePoll:
		if !permissions.SendPolls {
			return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot create polls in this chat")
		}
	case TypeSticker, TypeGIF:
		if !permissions.SendStickers {
			return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot send stickers in this chat")
		}
	}
	return nil
}

// ListChats returns the caller's conversation list.
func (s *Service) ListChats(ctx context.Context, userID uuid.UUID, limit int, before *time.Time) ([]Chat, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	chats, err := s.repo.ListChats(ctx, userID, limit, before)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return chats, nil
}

// OpenPrivateChat returns (creating if needed) the private chat with a peer.
func (s *Service) OpenPrivateChat(ctx context.Context, userID, peerID uuid.UUID) (uuid.UUID, error) {
	chatID, _, err := s.repo.EnsurePrivateChat(ctx, userID, peerID)
	if err != nil {
		return uuid.Nil, httpx.Internal(err)
	}
	return chatID, nil
}

// History returns a page of messages, oldest-to-newest within the page.
func (s *Service) History(ctx context.Context, chatID, userID uuid.UUID, beforeSeq, afterSeq *int64, limit int) ([]Message, error) {
	if limit <= 0 || limit > MaxHistoryPageSize {
		limit = 50
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	messages, err := s.repo.History(ctx, HistoryQuery{
		ChatID: chatID, ViewerID: userID,
		BeforeSeq: beforeSeq, AfterSeq: afterSeq, Limit: limit,
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return messages, nil
}

// ClearHistory empties a conversation without leaving it.
//
// The default is one-sided and always permitted: what the caller keeps in
// their own copy of a chat is theirs to discard. Clearing it for everyone is
// destructive to someone else's copy, so it is allowed only where deleting
// their messages one by one would also have been — a one-to-one chat, or a
// group where the caller may delete other people's messages.
func (s *Service) ClearHistory(ctx context.Context, chatID, actorID uuid.UUID, forEveryone bool) (int64, error) {
	chatCtx, err := s.repo.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return 0, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return 0, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return 0, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if forEveryone {
		switch chatCtx.ChatType {
		case ChatPrivate, ChatSecret:
			// Two people, one conversation: either may end it for both.
		default:
			if !chatCtx.Permissions.DeleteMessages {
				return 0, httpx.Forbidden(httpx.CodeForbidden,
					"You cannot clear this chat for everyone")
			}
		}
	}

	watermark, recipients, err := s.repo.ClearHistory(ctx, chatID, actorID, forEveryone)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return 0, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		case errors.Is(err, ErrNotMember):
			return 0, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		}
		return 0, httpx.Internal(err)
	}

	s.broadcast(ctx, chatCtx, EventChatHistoryCleared,
		&SendResult{Recipients: recipients, FannedOut: true},
		map[string]any{"chat_id": chatID, "upto_seq": watermark, "for_everyone": forEveryone})
	return watermark, nil
}

// Edit updates a message the caller sent, inside the edit window.
func (s *Service) Edit(ctx context.Context, messageID, editorID uuid.UUID, content string, entities json.RawMessage) (*Message, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, httpx.Validation("Message is empty").WithField("content", "must not be empty")
	}
	if utf8.RuneCountInString(content) > MaxContentRunes {
		return nil, httpx.Validation("Message is too long").WithField("content", "exceeds the maximum length")
	}

	existing, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return nil, httpx.Internal(err)
	}
	if existing.DeletedAt != nil {
		return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
	}
	if existing.SenderID == nil || *existing.SenderID != editorID {
		return nil, httpx.Forbidden(httpx.CodeForbidden, "You can only edit your own messages")
	}
	if time.Since(existing.CreatedAt) > EditWindow {
		return nil, httpx.Forbidden(httpx.CodeMessageTooOld, "This message is too old to edit")
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, existing.ChatID, editorID)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	updated, recipients, err := s.repo.Edit(ctx, messageID, editorID, content, entities)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return nil, httpx.Internal(err)
	}

	s.broadcast(ctx, chatCtx, EventMessageEdited, &SendResult{Recipients: recipients},
		map[string]any{"chat_id": existing.ChatID, "message": updated})
	// Re-indexed under the same document id, so the edit replaces the old text
	// rather than leaving the original findable alongside it.
	s.indexForSearch(ctx, chatCtx, updated)
	return updated, nil
}

// Delete removes a message. Senders may delete their own; moderators with
// delete_messages may delete anyone's.
func (s *Service) Delete(ctx context.Context, messageID, actorID uuid.UUID) error {
	existing, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return httpx.Internal(err)
	}
	if existing.DeletedAt != nil {
		return nil // Deleting twice is a no-op.
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, existing.ChatID, actorID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	isOwn := existing.SenderID != nil && *existing.SenderID == actorID
	if !isOwn && !chatCtx.Permissions.DeleteMessages {
		return httpx.Forbidden(httpx.CodeForbidden, "You cannot delete this message")
	}

	chatID, seq, recipients, err := s.repo.Delete(ctx, messageID, actorID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return httpx.Internal(err)
	}

	s.broadcast(ctx, chatCtx, EventMessageDeleted, &SendResult{Recipients: recipients},
		map[string]any{"chat_id": chatID, "message_id": messageID, "seq": seq})
	s.removeFromSearch(ctx, messageID)
	return nil
}

// MarkRead advances the read cursor and tells the other party (§7).
func (s *Service) MarkRead(ctx context.Context, chatID, userID uuid.UUID, uptoSeq int64) (int64, error) {
	newSeq, err := s.repo.MarkRead(ctx, chatID, userID, uptoSeq)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return 0, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		}
		return 0, httpx.Internal(err)
	}

	// Read receipts are a live signal, not history: they go out over the chat
	// subject and are never written to the durable event log.
	s.publishChat(chatID, EventMessageRead, map[string]any{
		"chat_id": chatID, "user_id": userID, "last_read_seq": newSeq,
	})
	return newSeq, nil
}

// React toggles an emoji reaction.
func (s *Service) React(ctx context.Context, messageID, userID uuid.UUID, emoji string) (bool, error) {
	emoji = strings.TrimSpace(emoji)
	if emoji == "" || utf8.RuneCountInString(emoji) > 8 {
		return false, httpx.Validation("Reaction is not valid").WithField("emoji", "must be a single emoji")
	}

	message, err := s.repo.MessageByID(ctx, messageID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return false, httpx.Internal(err)
	}

	chatCtx, err := s.repo.ChatContextFor(ctx, message.ChatID, userID)
	if err != nil {
		return false, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return false, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	added, chatID, err := s.repo.React(ctx, messageID, userID, emoji)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, httpx.NotFound(httpx.CodeMessageNotFound, "Message not found")
		}
		return false, httpx.Internal(err)
	}

	s.publishChat(chatID, EventMessageReaction, map[string]any{
		"chat_id": chatID, "message_id": messageID,
		"user_id": userID, "emoji": emoji, "added": added,
	})
	return added, nil
}

// SetTyping broadcasts a transient typing indicator. It is deliberately
// ephemeral: nothing is persisted and a missed frame costs nothing.
func (s *Service) SetTyping(ctx context.Context, chatID, userID uuid.UUID, typing bool) error {
	chatCtx, err := s.repo.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	event := EventTypingStop
	if typing {
		event = EventTypingStart
	}
	s.publishChat(chatID, event, map[string]any{"chat_id": chatID, "user_id": userID})
	return nil
}

// SetDraft persists an unsent draft so it appears on the user's other devices.
func (s *Service) SetDraft(ctx context.Context, chatID, userID uuid.UUID, draft string) error {
	if utf8.RuneCountInString(draft) > MaxContentRunes {
		return httpx.Validation("Draft is too long").WithField("draft", "exceeds the maximum length")
	}
	if err := s.repo.SetDraft(ctx, chatID, userID, draft); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
		}
		return httpx.Internal(err)
	}
	return nil
}

// Sync returns events after the caller's cursor plus the current head, which
// is what a device asks for on reconnect (§9).
func (s *Service) Sync(ctx context.Context, userID uuid.UUID, cursor int64, limit int) ([]Event, int64, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	events, latest, err := s.repo.EventsSince(ctx, userID, cursor, limit)
	if err != nil {
		return nil, 0, httpx.Internal(err)
	}
	return events, latest, nil
}

// broadcast pushes an event to every recipient's socket and to the chat
// subject. Realtime delivery is best-effort: the durable record is the event
// log, and a device that misses a frame catches up by cursor.
func (s *Service) broadcast(ctx context.Context, chatCtx *ChatContext, eventType string, result *SendResult, payload map[string]any) {
	for _, recipient := range result.Recipients {
		envelope := map[string]any{
			"event":   eventType,
			"payload": payload,
		}
		if seq, ok := result.EventSeqs[recipient]; ok {
			envelope["sync_seq"] = seq
		}
		if err := s.bus.PublishRealtime(bus.UserSubject(recipient.String()), envelope); err != nil {
			s.logger.Warn("realtime publish failed",
				slog.String("event", eventType),
				slog.String("user_id", recipient.String()),
				slog.Any("error", err))
		}
	}

	// Large chats have no per-user fan-out, so the chat subject is the only
	// live path for members who currently have the chat open.
	if !result.FannedOut {
		s.publishChat(chatCtx.ChatID, eventType, payload)
	}
}

func (s *Service) publishChat(chatID uuid.UUID, eventType string, payload map[string]any) {
	envelope := map[string]any{"event": eventType, "payload": payload}
	if err := s.bus.PublishRealtime(bus.ChatSubject(chatID.String()), envelope); err != nil {
		s.logger.Warn("chat broadcast failed",
			slog.String("event", eventType),
			slog.String("chat_id", chatID.String()),
			slog.Any("error", err))
	}
}
