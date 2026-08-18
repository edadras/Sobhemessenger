package messaging

// Forum topics (§14).
//
// A forum is a group whose messages are filed under topics. Every message in
// such a group belongs to exactly one, including the General topic created
// when the group is converted — which is what lets an existing group become a
// forum without orphaning a word of its history.
//
// A topic is not a chat. Membership, permissions, slow mode, moderation and
// the chat list all stay at the chat level; what a topic adds is a partition
// of the messages and a read cursor per partition, because a forum member
// follows some topics and ignores others.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

const (
	maxTopicTitle = 128
	// generalTopicTitle is what the converted group's existing history is
	// filed under. It is a name, not an identity: is_general is.
	generalTopicTitle = "General"
)

var (
	ErrTopicNotFound = errors.New("messaging: no such topic")
	ErrNotAForum     = errors.New("messaging: that chat is not a forum")
	ErrTopicClosed   = errors.New("messaging: that topic is closed")
	ErrGeneralTopic  = errors.New("messaging: the General topic cannot be removed")
)

// Topic is one thread inside a forum.
type Topic struct {
	ID           uuid.UUID  `json:"id"`
	ChatID       uuid.UUID  `json:"chat_id"`
	Title        string     `json:"title"`
	IconColor    int        `json:"icon_color"`
	IconEmoji    string     `json:"icon_emoji,omitempty"`
	CreatedBy    *uuid.UUID `json:"created_by,omitempty"`
	IsGeneral    bool       `json:"is_general"`
	IsClosed     bool       `json:"is_closed"`
	IsHidden     bool       `json:"is_hidden"`
	IsPinned     bool       `json:"is_pinned"`
	MessageCount int        `json:"message_count"`
	// UnreadCount is the caller's own, from their per-topic cursor.
	UnreadCount   int       `json:"unread_count"`
	LastMessageAt time.Time `json:"last_message_at"`
	CreatedAt     time.Time `json:"created_at"`
}

// TopicUpdate carries only what is being changed. Unlike a folder, a topic is
// edited a field at a time — renaming one and closing one are different acts
// by different people, and a whole-object PUT would let a rename silently
// reopen a topic somebody had closed.
type TopicUpdate struct {
	Title     *string
	IconColor *int
	IconEmoji *string
	IsClosed  *bool
	IsHidden  *bool
	IsPinned  *bool
}

// ------------------------------------------------------------- repository

// EnableForum switches a group into forum mode and files its history under a
// General topic.
//
// Both in one transaction. A group marked as a forum whose existing messages
// belong to no topic would render as an empty forum with its history
// unreachable — the conversion has to be all or nothing.
func (r *Repository) EnableForum(ctx context.Context, chatID uuid.UUID, actorID uuid.UUID) (*Topic, error) {
	topic := &Topic{ChatID: chatID}
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var chatType string
		if err := tx.QueryRow(ctx,
			`SELECT type FROM chats WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			chatID).Scan(&chatType); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: lock chat: %w", err)
		}
		if chatType != ChatGroup {
			return ErrNotAForum
		}

		// Already a forum: return the General topic rather than making a
		// second one. Enabling twice is what a retried request looks like.
		err := tx.QueryRow(ctx, `
			SELECT id, title, icon_color, icon_emoji, created_by, is_general,
			       is_closed, is_hidden, is_pinned, message_count, last_message_at, created_at
			FROM forum_topics
			WHERE chat_id = $1 AND is_general AND deleted_at IS NULL`, chatID,
		).Scan(&topic.ID, &topic.Title, &topic.IconColor, &topic.IconEmoji, &topic.CreatedBy,
			&topic.IsGeneral, &topic.IsClosed, &topic.IsHidden, &topic.IsPinned,
			&topic.MessageCount, &topic.LastMessageAt, &topic.CreatedAt)
		if err == nil {
			return markForum(ctx, tx, chatID, true)
		}
		if !database.IsNoRows(err) {
			return fmt.Errorf("messaging: read general topic: %w", err)
		}

		err = tx.QueryRow(ctx, `
			INSERT INTO forum_topics (chat_id, title, created_by, is_general, is_pinned)
			VALUES ($1, $2, $3, TRUE, TRUE)
			RETURNING id, title, icon_color, icon_emoji, created_by, is_general,
			          is_closed, is_hidden, is_pinned, message_count, last_message_at, created_at`,
			chatID, generalTopicTitle, actorID,
		).Scan(&topic.ID, &topic.Title, &topic.IconColor, &topic.IconEmoji, &topic.CreatedBy,
			&topic.IsGeneral, &topic.IsClosed, &topic.IsHidden, &topic.IsPinned,
			&topic.MessageCount, &topic.LastMessageAt, &topic.CreatedAt)
		if err != nil {
			return fmt.Errorf("messaging: create general topic: %w", err)
		}

		tag, err := tx.Exec(ctx,
			`UPDATE messages SET topic_id = $2 WHERE chat_id = $1 AND topic_id IS NULL`,
			chatID, topic.ID)
		if err != nil {
			return fmt.Errorf("messaging: file history under general: %w", err)
		}
		topic.MessageCount = int(tag.RowsAffected())
		if _, err := tx.Exec(ctx,
			`UPDATE forum_topics SET message_count = $2 WHERE id = $1`,
			topic.ID, topic.MessageCount); err != nil {
			return fmt.Errorf("messaging: count general topic: %w", err)
		}

		if err := markForum(ctx, tx, chatID, true); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return topic, nil
}

// markForum sets the flag, creating the extension row if it is somehow
// missing.
//
// An UPDATE alone would report success while changing nothing, and the chat
// would come back from the conversion with a General topic it does not use —
// which is exactly what happened until a test caught it.
func markForum(ctx context.Context, tx pgx.Tx, chatID uuid.UUID, isForum bool) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO groups (chat_id, is_forum) VALUES ($1, $2)
		ON CONFLICT (chat_id) DO UPDATE SET is_forum = EXCLUDED.is_forum`,
		chatID, isForum); err != nil {
		return fmt.Errorf("messaging: mark forum: %w", err)
	}
	return nil
}

// DisableForum turns forum mode off.
//
// The topics and the messages' topic_id are left in place. Clearing them would
// destroy the filing somebody did, and switching back on would then find an
// empty forum — whereas leaving them means the group reads as an ordinary
// group now and returns to its topics if it is ever a forum again.
func (r *Repository) DisableForum(ctx context.Context, chatID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		// The chat's type is what decides whether this is meaningful, not
		// whether an extension row happens to exist: reporting "not a forum"
		// because a row is missing would be a confusing answer to a request
		// that is otherwise perfectly sensible.
		var chatType string
		if err := tx.QueryRow(ctx,
			`SELECT type FROM chats WHERE id = $1 AND deleted_at IS NULL`,
			chatID).Scan(&chatType); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: read chat type: %w", err)
		}
		if chatType != ChatGroup {
			return ErrNotAForum
		}
		return markForum(ctx, tx, chatID, false)
	})
}

// IsForum reports whether a chat files its messages under topics.
func (r *Repository) IsForum(ctx context.Context, chatID uuid.UUID) (bool, error) {
	var isForum bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT COALESCE(is_forum, FALSE) FROM groups WHERE chat_id = $1`, chatID).Scan(&isForum)
	if database.IsNoRows(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("messaging: read forum flag: %w", err)
	}
	return isForum, nil
}

// Topics lists a forum's topics with the caller's own unread counts.
func (r *Repository) Topics(ctx context.Context, chatID, viewerID uuid.UUID, includeHidden bool, limit int) ([]Topic, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT t.id, t.chat_id, t.title, t.icon_color, t.icon_emoji, t.created_by,
		       t.is_general, t.is_closed, t.is_hidden, t.is_pinned,
		       t.message_count, t.last_message_at, t.created_at,
		       COALESCE(rd.unread_count, 0)
		FROM forum_topics t
		LEFT JOIN forum_topic_reads rd ON rd.topic_id = t.id AND rd.user_id = $2
		WHERE t.chat_id = $1 AND t.deleted_at IS NULL
		  AND ($3::boolean OR NOT t.is_hidden)
		ORDER BY t.is_pinned DESC, t.last_message_at DESC
		LIMIT $4`, chatID, viewerID, includeHidden, limit)
	if err != nil {
		return nil, fmt.Errorf("messaging: list topics: %w", err)
	}
	defer rows.Close()

	topics := []Topic{}
	for rows.Next() {
		var topic Topic
		if err := rows.Scan(&topic.ID, &topic.ChatID, &topic.Title, &topic.IconColor,
			&topic.IconEmoji, &topic.CreatedBy, &topic.IsGeneral, &topic.IsClosed,
			&topic.IsHidden, &topic.IsPinned, &topic.MessageCount,
			&topic.LastMessageAt, &topic.CreatedAt, &topic.UnreadCount); err != nil {
			return nil, err
		}
		topics = append(topics, topic)
	}
	return topics, rows.Err()
}

// TopicByID reads one topic, scoped to its chat so an id from elsewhere
// cannot be used to read across chats.
func (r *Repository) TopicByID(ctx context.Context, chatID, topicID uuid.UUID) (*Topic, error) {
	topic := &Topic{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, chat_id, title, icon_color, icon_emoji, created_by,
		       is_general, is_closed, is_hidden, is_pinned,
		       message_count, last_message_at, created_at
		FROM forum_topics
		WHERE id = $1 AND chat_id = $2 AND deleted_at IS NULL`, topicID, chatID,
	).Scan(&topic.ID, &topic.ChatID, &topic.Title, &topic.IconColor, &topic.IconEmoji,
		&topic.CreatedBy, &topic.IsGeneral, &topic.IsClosed, &topic.IsHidden,
		&topic.IsPinned, &topic.MessageCount, &topic.LastMessageAt, &topic.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrTopicNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: read topic: %w", err)
	}
	return topic, nil
}

// GeneralTopic returns a forum's General topic id, which is where a message
// that names no topic is filed.
func (r *Repository) GeneralTopic(ctx context.Context, chatID uuid.UUID) (*uuid.UUID, error) {
	var topicID uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT id FROM forum_topics WHERE chat_id = $1 AND is_general AND deleted_at IS NULL`,
		chatID).Scan(&topicID)
	if database.IsNoRows(err) {
		// A forum without a General topic should not exist — the conversion
		// creates one in the same transaction as the flag — but a message is
		// not worth refusing over it.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: read general topic: %w", err)
	}
	return &topicID, nil
}

// CreateTopic opens a new thread.
func (r *Repository) CreateTopic(ctx context.Context, chatID, creatorID uuid.UUID, title, emoji string, color int) (*Topic, error) {
	topic := &Topic{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO forum_topics (chat_id, title, icon_color, icon_emoji, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, chat_id, title, icon_color, icon_emoji, created_by,
		          is_general, is_closed, is_hidden, is_pinned,
		          message_count, last_message_at, created_at`,
		chatID, title, color, emoji, creatorID,
	).Scan(&topic.ID, &topic.ChatID, &topic.Title, &topic.IconColor, &topic.IconEmoji,
		&topic.CreatedBy, &topic.IsGeneral, &topic.IsClosed, &topic.IsHidden,
		&topic.IsPinned, &topic.MessageCount, &topic.LastMessageAt, &topic.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("messaging: create topic: %w", err)
	}
	return topic, nil
}

// UpdateTopic applies the fields that were supplied and leaves the rest.
func (r *Repository) UpdateTopic(ctx context.Context, chatID, topicID uuid.UUID, in TopicUpdate) (*Topic, error) {
	topic := &Topic{}
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE forum_topics
		SET title = COALESCE($3, title),
		    icon_color = COALESCE($4, icon_color),
		    icon_emoji = COALESCE($5, icon_emoji),
		    is_closed = COALESCE($6, is_closed),
		    is_hidden = COALESCE($7, is_hidden),
		    is_pinned = COALESCE($8, is_pinned)
		WHERE id = $1 AND chat_id = $2 AND deleted_at IS NULL
		RETURNING id, chat_id, title, icon_color, icon_emoji, created_by,
		          is_general, is_closed, is_hidden, is_pinned,
		          message_count, last_message_at, created_at`,
		topicID, chatID, in.Title, in.IconColor, in.IconEmoji,
		in.IsClosed, in.IsHidden, in.IsPinned,
	).Scan(&topic.ID, &topic.ChatID, &topic.Title, &topic.IconColor, &topic.IconEmoji,
		&topic.CreatedBy, &topic.IsGeneral, &topic.IsClosed, &topic.IsHidden,
		&topic.IsPinned, &topic.MessageCount, &topic.LastMessageAt, &topic.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrTopicNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: update topic: %w", err)
	}
	return topic, nil
}

// DeleteTopic removes a topic and tombstones what was said in it.
//
// The messages go with it because a topic is where they live: leaving them
// behind with a dangling topic_id would put them in a forum with no thread to
// open them from. The General topic is exempt — deleting it would strand the
// whole converted history.
func (r *Repository) DeleteTopic(ctx context.Context, chatID, topicID, actorID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var isGeneral bool
		err := tx.QueryRow(ctx,
			`SELECT is_general FROM forum_topics
			 WHERE id = $1 AND chat_id = $2 AND deleted_at IS NULL FOR UPDATE`,
			topicID, chatID).Scan(&isGeneral)
		if database.IsNoRows(err) {
			return ErrTopicNotFound
		}
		if err != nil {
			return fmt.Errorf("messaging: lock topic: %w", err)
		}
		if isGeneral {
			return ErrGeneralTopic
		}

		if _, err := tx.Exec(ctx, `
			UPDATE messages
			SET deleted_at = now(), deleted_by = $2, content = '',
			    entities = '[]'::jsonb, payload = '{}'::jsonb, reply_markup = NULL
			WHERE topic_id = $1 AND deleted_at IS NULL`, topicID, actorID); err != nil {
			return fmt.Errorf("messaging: tombstone topic messages: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE forum_topics SET deleted_at = now() WHERE id = $1`, topicID); err != nil {
			return fmt.Errorf("messaging: delete topic: %w", err)
		}
		return nil
	})
}

// MarkTopicRead advances the caller's cursor inside one topic.
func (r *Repository) MarkTopicRead(ctx context.Context, topicID, userID uuid.UUID, uptoSeq int64) (int64, error) {
	var newSeq int64
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO forum_topic_reads (topic_id, user_id, last_read_seq, unread_count)
		VALUES ($1, $2, $3, 0)
		ON CONFLICT (topic_id, user_id) DO UPDATE
		SET last_read_seq = GREATEST(forum_topic_reads.last_read_seq, EXCLUDED.last_read_seq),
		    unread_count = (
		        SELECT count(*) FROM messages m
		        WHERE m.topic_id = $1 AND m.deleted_at IS NULL
		          AND m.seq > GREATEST(forum_topic_reads.last_read_seq, EXCLUDED.last_read_seq)
		    )
		RETURNING last_read_seq`, topicID, userID, uptoSeq).Scan(&newSeq)
	if err != nil {
		return 0, fmt.Errorf("messaging: mark topic read: %w", err)
	}
	return newSeq, nil
}

// ---------------------------------------------------------------- service

// EnableForum switches a group into forum mode.
func (s *Service) EnableForum(ctx context.Context, chatID, actorID uuid.UUID) (*Topic, error) {
	if _, err := s.authorizeTopics(ctx, chatID, actorID, true); err != nil {
		return nil, err
	}

	topic, err := s.repo.EnableForum(ctx, chatID, actorID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		case errors.Is(err, ErrNotAForum):
			return nil, httpx.Validation("Only a group can be a forum").
				WithField("chat_id", "must be a group")
		}
		return nil, httpx.Internal(err)
	}
	s.publishChat(chatID, EventChatUpdated, map[string]any{
		"chat_id": chatID, "is_forum": true, "general_topic_id": topic.ID,
	})
	return topic, nil
}

// DisableForum turns forum mode off, leaving the filing intact.
func (s *Service) DisableForum(ctx context.Context, chatID, actorID uuid.UUID) error {
	if _, err := s.authorizeTopics(ctx, chatID, actorID, true); err != nil {
		return err
	}
	if err := s.repo.DisableForum(ctx, chatID); err != nil {
		if errors.Is(err, ErrNotAForum) {
			return httpx.Validation("Only a group can be a forum").
				WithField("chat_id", "must be a group")
		}
		return httpx.Internal(err)
	}
	s.publishChat(chatID, EventChatUpdated, map[string]any{"chat_id": chatID, "is_forum": false})
	return nil
}

// Topics lists a forum's topics.
//
// Hidden topics are shown to staff, who need to see what they hid, and not to
// anyone else.
func (s *Service) Topics(ctx context.Context, chatID, viewerID uuid.UUID, limit int) ([]Topic, error) {
	chatCtx, err := s.authorizeTopics(ctx, chatID, viewerID, false)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	topics, err := s.repo.Topics(ctx, chatID, viewerID, chatCtx.Permissions.EditGroup, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return topics, nil
}

// CreateTopic opens a thread. Anyone who may post may open one, which is what
// makes a forum a forum rather than a set of announcements.
func (s *Service) CreateTopic(ctx context.Context, chatID, creatorID uuid.UUID, title, emoji string, color int) (*Topic, error) {
	chatCtx, err := s.authorizeTopics(ctx, chatID, creatorID, false)
	if err != nil {
		return nil, err
	}
	if !chatCtx.Permissions.SendMessages {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot open a topic in this group")
	}

	title = strings.TrimSpace(title)
	if title == "" {
		return nil, httpx.Validation("A topic needs a title").WithField("title", "required")
	}
	if len([]rune(title)) > maxTopicTitle {
		return nil, httpx.Validation("That title is too long").
			WithField("title", fmt.Sprintf("at most %d characters", maxTopicTitle))
	}

	topic, err := s.repo.CreateTopic(ctx, chatID, creatorID, title, emoji, color)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	s.publishChat(chatID, EventTopicCreated, map[string]any{
		"chat_id": chatID, "topic": topic,
	})
	return topic, nil
}

// UpdateTopic renames, closes, hides or pins one.
//
// Renaming a topic you opened is yours to do; everything else is moderation.
// The split matters: closing a topic ends a conversation other people are
// having in it.
func (s *Service) UpdateTopic(ctx context.Context, chatID, topicID, actorID uuid.UUID, in TopicUpdate) (*Topic, error) {
	chatCtx, err := s.authorizeTopics(ctx, chatID, actorID, false)
	if err != nil {
		return nil, err
	}

	existing, err := s.repo.TopicByID(ctx, chatID, topicID)
	if err != nil {
		if errors.Is(err, ErrTopicNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "No such topic")
		}
		return nil, httpx.Internal(err)
	}

	isAuthor := existing.CreatedBy != nil && *existing.CreatedBy == actorID
	moderating := in.IsClosed != nil || in.IsHidden != nil || in.IsPinned != nil
	if moderating && !chatCtx.Permissions.EditGroup {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot moderate topics in this group")
	}
	if in.Title != nil && !isAuthor && !chatCtx.Permissions.EditGroup {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot rename this topic")
	}
	if in.Title != nil {
		trimmed := strings.TrimSpace(*in.Title)
		if trimmed == "" {
			return nil, httpx.Validation("A topic needs a title").WithField("title", "required")
		}
		if len([]rune(trimmed)) > maxTopicTitle {
			return nil, httpx.Validation("That title is too long").
				WithField("title", fmt.Sprintf("at most %d characters", maxTopicTitle))
		}
		in.Title = &trimmed
	}

	topic, err := s.repo.UpdateTopic(ctx, chatID, topicID, in)
	if err != nil {
		if errors.Is(err, ErrTopicNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "No such topic")
		}
		return nil, httpx.Internal(err)
	}
	s.publishChat(chatID, EventTopicUpdated, map[string]any{
		"chat_id": chatID, "topic": topic,
	})
	return topic, nil
}

// DeleteTopic removes a thread and what was said in it.
func (s *Service) DeleteTopic(ctx context.Context, chatID, topicID, actorID uuid.UUID) error {
	chatCtx, err := s.authorizeTopics(ctx, chatID, actorID, false)
	if err != nil {
		return err
	}
	if !chatCtx.Permissions.DeleteMessages && !chatCtx.Permissions.EditGroup {
		return httpx.Forbidden(httpx.CodePermissionDenied, "You cannot delete topics here")
	}

	if err := s.repo.DeleteTopic(ctx, chatID, topicID, actorID); err != nil {
		switch {
		case errors.Is(err, ErrTopicNotFound):
			return httpx.NotFound(httpx.CodeNotFound, "No such topic")
		case errors.Is(err, ErrGeneralTopic):
			return httpx.Conflict(httpx.CodeConflict,
				"The General topic cannot be deleted; it holds the group's history")
		}
		return httpx.Internal(err)
	}
	s.publishChat(chatID, EventTopicDeleted, map[string]any{
		"chat_id": chatID, "topic_id": topicID,
	})
	return nil
}

// TopicHistory returns a page of one topic's messages.
func (s *Service) TopicHistory(ctx context.Context, chatID, topicID, viewerID uuid.UUID, beforeSeq, afterSeq *int64, limit int) ([]Message, error) {
	if _, err := s.authorizeTopics(ctx, chatID, viewerID, false); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxHistoryPageSize {
		limit = 50
	}
	if _, err := s.repo.TopicByID(ctx, chatID, topicID); err != nil {
		if errors.Is(err, ErrTopicNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "No such topic")
		}
		return nil, httpx.Internal(err)
	}

	messages, err := s.repo.History(ctx, HistoryQuery{
		ChatID: chatID, ViewerID: viewerID,
		BeforeSeq: beforeSeq, AfterSeq: afterSeq, Limit: limit, TopicID: &topicID,
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return messages, nil
}

// MarkTopicRead advances the caller's cursor inside one topic.
func (s *Service) MarkTopicRead(ctx context.Context, chatID, topicID, userID uuid.UUID, uptoSeq int64) (int64, error) {
	if _, err := s.authorizeTopics(ctx, chatID, userID, false); err != nil {
		return 0, err
	}
	if _, err := s.repo.TopicByID(ctx, chatID, topicID); err != nil {
		if errors.Is(err, ErrTopicNotFound) {
			return 0, httpx.NotFound(httpx.CodeNotFound, "No such topic")
		}
		return 0, httpx.Internal(err)
	}

	newSeq, err := s.repo.MarkTopicRead(ctx, topicID, userID, uptoSeq)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return newSeq, nil
}

// authorizeTopics is the gate every topic operation passes through: a member
// of the chat, and for the administrative ones the permission to edit it.
func (s *Service) authorizeTopics(ctx context.Context, chatID, actorID uuid.UUID, needsAdmin bool) (*ChatContext, error) {
	chatCtx, err := s.repo.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if needsAdmin && !chatCtx.Permissions.EditGroup {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied, "You cannot change this group")
	}
	return chatCtx, nil
}
