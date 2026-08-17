package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
)

var (
	ErrNotFound  = errors.New("messaging: not found")
	ErrNotMember = errors.New("messaging: caller is not a member of this chat")
	// ErrAlreadyPublished is what rescheduling a post that has already gone
	// out returns: it is a message now, and messages are edited, not rescheduled.
	ErrAlreadyPublished = errors.New("messaging: that message has already been published")
)

// FanoutThreshold bounds how many per-user event rows one message may create.
//
// Private chats and ordinary groups sit far below it, so their members get a
// push-style event log and resume by cursor. Above the threshold — a channel
// with tens of thousands of subscribers — writing a row per member per post
// would be untenable, so those chats are pull-based: the post is broadcast on
// the chat subject for live subscribers, and clients reconcile by comparing
// their per-chat cursor against chats.last_seq. See docs/architecture/sync.md.
const FanoutThreshold = 1000

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ---------------------------------------------------------------- chats

// ChatContext is the membership and permission snapshot the service authorises
// against. One query answers "may this user do X here".
type ChatContext struct {
	ChatID      uuid.UUID
	ChatType    string
	MemberCount int
	LastSeq     int64
	Role        string
	Permissions Permissions
	SlowMode    int
	AutoDelete  int
	IsMember    bool
	MutedUntil  *time.Time
}

func (r *Repository) ChatContextFor(ctx context.Context, chatID, userID uuid.UUID) (*ChatContext, error) {
	var (
		result     ChatContext
		role       *string
		rawPerms   []byte
		slowMode   *int
		autoDelete *int
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT c.id, c.type, c.member_count, c.last_seq,
		       m.role, m.permissions, m.muted_until,
		       s.slow_mode_seconds, s.auto_delete_seconds
		FROM chats c
		LEFT JOIN chat_members m ON m.chat_id = c.id AND m.user_id = $2 AND m.left_at IS NULL
		LEFT JOIN chat_settings s ON s.chat_id = c.id
		WHERE c.id = $1 AND c.deleted_at IS NULL`,
		chatID, userID,
	).Scan(&result.ChatID, &result.ChatType, &result.MemberCount, &result.LastSeq,
		&role, &rawPerms, &result.MutedUntil, &slowMode, &autoDelete)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: chat context: %w", err)
	}

	if role != nil {
		result.IsMember = true
		result.Role = *role
		result.Permissions = applyOverrides(PermissionsForRole(*role, result.ChatType), rawPerms)
	}
	if slowMode != nil {
		result.SlowMode = *slowMode
	}
	if autoDelete != nil {
		result.AutoDelete = *autoDelete
	}
	return &result, nil
}

// EnsurePrivateChat returns the existing private chat between two users or
// creates it. The (user_a, user_b) unique key makes this safe under a race:
// the loser of the insert re-reads the winner's row.
func (r *Repository) EnsurePrivateChat(ctx context.Context, a, b uuid.UUID) (uuid.UUID, bool, error) {
	if a == b {
		// Saved Messages: a private chat with a single member.
		return r.ensureSelfChat(ctx, a)
	}

	low, high := a, b
	if high.String() < low.String() {
		low, high = high, low
	}

	var chatID uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT chat_id FROM private_chat_keys WHERE user_a_id = $1 AND user_b_id = $2`,
		low, high).Scan(&chatID)
	if err == nil {
		return chatID, false, nil
	}
	if !database.IsNoRows(err) {
		return uuid.Nil, false, fmt.Errorf("messaging: lookup private chat: %w", err)
	}

	created := false
	err = r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO chats (type, creator_id, member_count) VALUES ('private', $1, 2) RETURNING id`,
			a).Scan(&chatID); err != nil {
			return fmt.Errorf("messaging: create private chat: %w", err)
		}

		_, err := tx.Exec(ctx,
			`INSERT INTO private_chat_keys (chat_id, user_a_id, user_b_id) VALUES ($1, $2, $3)`,
			chatID, low, high)
		if database.IsUniqueViolation(err) {
			// Someone else created it first; adopt their chat.
			return tx.QueryRow(ctx,
				`SELECT chat_id FROM private_chat_keys WHERE user_a_id = $1 AND user_b_id = $2`,
				low, high).Scan(&chatID)
		}
		if err != nil {
			return fmt.Errorf("messaging: link private chat: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_members (chat_id, user_id, role)
			VALUES ($1, $2, 'member'), ($1, $3, 'member')`, chatID, a, b); err != nil {
			return fmt.Errorf("messaging: add private chat members: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_settings (chat_id, max_members) VALUES ($1, 2)`, chatID); err != nil {
			return fmt.Errorf("messaging: create chat settings: %w", err)
		}
		created = true
		return nil
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return chatID, created, nil
}

func (r *Repository) ensureSelfChat(ctx context.Context, userID uuid.UUID) (uuid.UUID, bool, error) {
	var chatID uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		SELECT c.id FROM chats c
		JOIN chat_members m ON m.chat_id = c.id
		WHERE c.type = 'private' AND c.creator_id = $1 AND c.member_count = 1
		  AND c.deleted_at IS NULL
		LIMIT 1`, userID).Scan(&chatID)
	if err == nil {
		return chatID, false, nil
	}
	if !database.IsNoRows(err) {
		return uuid.Nil, false, fmt.Errorf("messaging: lookup saved messages: %w", err)
	}

	err = r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO chats (type, creator_id, member_count) VALUES ('private', $1, 1) RETURNING id`,
			userID).Scan(&chatID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'owner')`,
			chatID, userID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO chat_settings (chat_id, max_members) VALUES ($1, 1)`, chatID)
		return err
	})
	return chatID, true, err
}

// ListChats returns the caller's chat list, most recently active first.
func (r *Repository) ListChats(ctx context.Context, userID uuid.UUID, limit int, before *time.Time) ([]Chat, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.type, c.community_id, c.title, c.description, c.username,
		       c.photo_media_id, c.creator_id, c.last_seq, c.last_message_at,
		       c.member_count, c.is_public, c.created_at,
		       m.role, m.permissions, m.last_read_seq, m.unread_count, m.mention_count,
		       m.is_pinned, m.is_archived, m.muted_until, m.draft, m.joined_at
		FROM chat_members m
		JOIN chats c ON c.id = m.chat_id
		WHERE m.user_id = $1 AND m.left_at IS NULL AND c.deleted_at IS NULL
		  AND ($3::timestamptz IS NULL OR c.last_message_at < $3)
		ORDER BY m.is_pinned DESC, c.last_message_at DESC NULLS LAST, c.created_at DESC
		LIMIT $2`, userID, limit, before)
	if err != nil {
		return nil, fmt.Errorf("messaging: list chats: %w", err)
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var (
			chat     Chat
			member   Membership
			rawPerms []byte
		)
		if err := rows.Scan(&chat.ID, &chat.Type, &chat.CommunityID, &chat.Title, &chat.Description,
			&chat.Username, &chat.PhotoMediaID, &chat.CreatorID, &chat.LastSeq, &chat.LastMessageAt,
			&chat.MemberCount, &chat.IsPublic, &chat.CreatedAt,
			&member.Role, &rawPerms, &member.LastReadSeq, &member.UnreadCount, &member.MentionCount,
			&member.IsPinned, &member.IsArchived, &member.MutedUntil, &member.Draft, &member.JoinedAt); err != nil {
			return nil, err
		}
		member.Permissions = applyOverrides(PermissionsForRole(member.Role, chat.Type), rawPerms)
		chat.Membership = &member
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}

// ---------------------------------------------------------------- send

// SendParams is one accepted send request.
type SendParams struct {
	ChatID          uuid.UUID
	SenderID        uuid.UUID
	ClientMessageID uuid.UUID
	Type            string
	Content         string
	Entities        json.RawMessage
	Payload         json.RawMessage
	ReplyToID       *uuid.UUID
	Attachments     []Attachment
	MentionUserIDs  []uuid.UUID
	Forward         *ForwardInfo
	IsSilent        bool
}

// SendResult carries the stored message plus who must be notified.
type SendResult struct {
	Message    *Message
	Recipients []uuid.UUID
	// EventSeqs maps recipient -> the sync sequence assigned to their event,
	// so a connected device can be told exactly where it now stands.
	EventSeqs map[uuid.UUID]int64
	// Duplicate is true when the client's idempotency key matched an existing
	// message, in which case nothing new was written.
	Duplicate bool
	FannedOut bool
}

// Send persists a message and advances every affected cursor in one
// transaction, so a message is never visible without its sequence number,
// unread counts or sync events.
func (r *Repository) Send(ctx context.Context, p SendParams) (*SendResult, error) {
	// Idempotency fast path: a retried send returns the original message
	// rather than a second copy (§7).
	if existing, err := r.messageByClientID(ctx, p.ChatID, p.SenderID, p.ClientMessageID); err == nil {
		return &SendResult{Message: existing, Duplicate: true}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	result := &SendResult{EventSeqs: make(map[uuid.UUID]int64)}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// Locking the chat row serialises sequence allocation for this chat.
		var lastSeq int64
		var memberCount int
		var chatType string
		if err := tx.QueryRow(ctx,
			`SELECT last_seq, member_count, type FROM chats WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			p.ChatID).Scan(&lastSeq, &memberCount, &chatType); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: lock chat: %w", err)
		}

		seq := lastSeq + 1
		message := &Message{}
		err := tx.QueryRow(ctx, `
			INSERT INTO messages (
				chat_id, seq, sender_id, client_message_id, type, content, entities, payload,
				reply_to_id, forward_from_chat_id, forward_from_message_id, forward_from_user_id,
				forward_signature, is_silent
			) VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, '[]'::jsonb), COALESCE($8, '{}'::jsonb),
			          $9, $10, $11, $12, $13, $14)
			RETURNING id, chat_id, seq, sender_id, client_message_id, type, content,
			          entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at`,
			p.ChatID, seq, p.SenderID, p.ClientMessageID, p.Type, p.Content,
			nullableJSON(p.Entities), nullableJSON(p.Payload), p.ReplyToID,
			forwardChat(p.Forward), forwardMessage(p.Forward), forwardUser(p.Forward),
			forwardSignature(p.Forward), p.IsSilent,
		).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID, &message.ClientMessageID,
			&message.Type, &message.Content, &message.Entities, &message.Payload, &message.ReplyToID,
			&message.IsPinned, &message.CreatedAt, &message.EditedAt, &message.DeletedAt)
		if err != nil {
			return fmt.Errorf("messaging: insert message: %w", err)
		}
		message.ForwardFrom = p.Forward
		message.Attachments = p.Attachments
		result.Message = message

		for _, attachment := range p.Attachments {
			if _, err := tx.Exec(ctx, `
				INSERT INTO message_attachments (message_id, media_id, position, caption)
				VALUES ($1, $2, $3, $4)`,
				message.ID, attachment.MediaID, attachment.Position, attachment.Caption); err != nil {
				return fmt.Errorf("messaging: insert attachment: %w", err)
			}
		}

		if len(p.MentionUserIDs) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO message_mentions (message_id, user_id)
				SELECT $1, unnest($2::uuid[])
				ON CONFLICT DO NOTHING`, message.ID, p.MentionUserIDs); err != nil {
				return fmt.Errorf("messaging: insert mentions: %w", err)
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE chats
			SET last_seq = $2, last_message_id = $3, last_message_at = $4, updated_at = now()
			WHERE id = $1`, p.ChatID, seq, message.ID, message.CreatedAt); err != nil {
			return fmt.Errorf("messaging: advance chat: %w", err)
		}

		// The sender's own cursor moves with the message: their device already
		// has it, so it must never count as unread.
		if _, err := tx.Exec(ctx, `
			UPDATE chat_members
			SET last_read_seq = $3, last_delivered_seq = $3
			WHERE chat_id = $1 AND user_id = $2`, p.ChatID, p.SenderID, seq); err != nil {
			return fmt.Errorf("messaging: advance sender cursor: %w", err)
		}

		if memberCount > FanoutThreshold {
			// Pull-based chat: recipients reconcile from chats.last_seq.
			return nil
		}
		result.FannedOut = true

		if _, err := tx.Exec(ctx, `
			UPDATE chat_members m
			SET unread_count = m.unread_count + 1,
			    mention_count = m.mention_count + CASE
			        WHEN m.user_id = ANY($3::uuid[]) THEN 1 ELSE 0 END
			WHERE m.chat_id = $1 AND m.user_id <> $2 AND m.left_at IS NULL`,
			p.ChatID, p.SenderID, p.MentionUserIDs); err != nil {
			return fmt.Errorf("messaging: bump unread counts: %w", err)
		}

		payload, err := json.Marshal(map[string]any{
			"chat_id": p.ChatID,
			"message": message,
		})
		if err != nil {
			return fmt.Errorf("messaging: marshal event: %w", err)
		}

		recipients, seqs, err := appendUserEvents(ctx, tx, p.ChatID, p.SenderID, EventMessageNew, payload)
		if err != nil {
			return err
		}
		result.Recipients = recipients
		result.EventSeqs = seqs
		return nil
	})
	if err != nil {
		// A concurrent retry of the same client_message_id lost the race on
		// the idempotency index; return the winner's message.
		if database.IsUniqueViolation(err, "messages_idempotency_key") {
			existing, lookupErr := r.messageByClientID(ctx, p.ChatID, p.SenderID, p.ClientMessageID)
			if lookupErr == nil {
				return &SendResult{Message: existing, Duplicate: true}, nil
			}
		}
		return nil, err
	}
	return result, nil
}

// appendUserEvents writes one sync-log entry per recipient and returns the
// sequence each was given.
//
// The counters are locked in user_id order first: two chats that share members
// would otherwise be able to grab the same rows in opposite orders and
// deadlock.
func appendUserEvents(ctx context.Context, tx pgx.Tx, chatID, excludeUserID uuid.UUID, eventType string, payload []byte) ([]uuid.UUID, map[uuid.UUID]int64, error) {
	rows, err := tx.Query(ctx, `
		WITH recipients AS (
			SELECT m.user_id
			FROM chat_members m
			WHERE m.chat_id = $1 AND m.user_id <> $2 AND m.left_at IS NULL
			ORDER BY m.user_id
		),
		locked AS (
			SELECT c.user_id
			FROM user_event_counters c
			WHERE c.user_id IN (SELECT user_id FROM recipients)
			ORDER BY c.user_id
			FOR UPDATE
		),
		bumped AS (
			UPDATE user_event_counters c
			SET last_seq = c.last_seq + 1
			WHERE c.user_id IN (SELECT user_id FROM locked)
			RETURNING c.user_id, c.last_seq
		)
		INSERT INTO user_events (user_id, seq, type, payload)
		SELECT user_id, last_seq, $3, $4::jsonb FROM bumped
		RETURNING user_id, seq`,
		chatID, excludeUserID, eventType, payload)
	if err != nil {
		return nil, nil, fmt.Errorf("messaging: append user events: %w", err)
	}
	defer rows.Close()

	var recipients []uuid.UUID
	seqs := make(map[uuid.UUID]int64)
	for rows.Next() {
		var userID uuid.UUID
		var seq int64
		if err := rows.Scan(&userID, &seq); err != nil {
			return nil, nil, err
		}
		recipients = append(recipients, userID)
		seqs[userID] = seq
	}
	return recipients, seqs, rows.Err()
}

func (r *Repository) messageByClientID(ctx context.Context, chatID, senderID, clientID uuid.UUID) (*Message, error) {
	message := &Message{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, chat_id, seq, sender_id, client_message_id, type, content,
		       entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at
		FROM messages
		WHERE chat_id = $1 AND sender_id = $2 AND client_message_id = $3`,
		chatID, senderID, clientID,
	).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID, &message.ClientMessageID,
		&message.Type, &message.Content, &message.Entities, &message.Payload, &message.ReplyToID,
		&message.IsPinned, &message.CreatedAt, &message.EditedAt, &message.DeletedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: lookup message by client id: %w", err)
	}
	return message, nil
}

// ---------------------------------------------------------------- history

// History returns messages in a chat, newest first, seeking by sequence.
func (r *Repository) History(ctx context.Context, chatID, viewerID uuid.UUID, beforeSeq, afterSeq *int64, limit int) ([]Message, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.id, m.chat_id, m.seq, m.sender_id, m.client_message_id, m.type,
		       CASE WHEN m.deleted_at IS NULL THEN m.content ELSE '' END,
		       m.entities, m.payload, m.reply_to_id,
		       m.forward_from_chat_id, m.forward_from_message_id, m.forward_from_user_id,
		       m.forward_signature, m.is_pinned, m.view_count,
		       m.created_at, m.edited_at, m.deleted_at,
		       COALESCE((
		           SELECT jsonb_agg(jsonb_build_object(
		               'media_id', a.media_id, 'position', a.position, 'caption', a.caption)
		               ORDER BY a.position)
		           FROM message_attachments a WHERE a.message_id = m.id
		       ), '[]'::jsonb),
		       COALESCE((
		           SELECT jsonb_agg(r.reaction)
		           FROM (
		               SELECT jsonb_build_object(
		                   'emoji', emoji,
		                   'count', count(*),
		                   'by_me', bool_or(user_id = $2)
		               ) AS reaction
		               FROM message_reactions
		               WHERE message_id = m.id
		               GROUP BY emoji
		           ) r
		       ), '[]'::jsonb)
		FROM messages m
		WHERE m.chat_id = $1
		  AND ($3::bigint IS NULL OR m.seq < $3)
		  AND ($4::bigint IS NULL OR m.seq > $4)
		ORDER BY m.seq DESC
		LIMIT $5`, chatID, viewerID, beforeSeq, afterSeq, limit)
	if err != nil {
		return nil, fmt.Errorf("messaging: history: %w", err)
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var (
			message       Message
			forwardChatID *uuid.UUID
			forwardMsgID  *uuid.UUID
			forwardUserID *uuid.UUID
			forwardSig    string
			rawAttach     []byte
			rawReactions  []byte
		)
		if err := rows.Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID,
			&message.ClientMessageID, &message.Type, &message.Content, &message.Entities,
			&message.Payload, &message.ReplyToID, &forwardChatID, &forwardMsgID, &forwardUserID,
			&forwardSig, &message.IsPinned, &message.ViewCount,
			&message.CreatedAt, &message.EditedAt, &message.DeletedAt,
			&rawAttach, &rawReactions); err != nil {
			return nil, err
		}

		if forwardChatID != nil || forwardUserID != nil {
			message.ForwardFrom = &ForwardInfo{
				ChatID: forwardChatID, MessageID: forwardMsgID,
				UserID: forwardUserID, Signature: forwardSig,
			}
		}
		_ = json.Unmarshal(rawAttach, &message.Attachments)
		_ = json.Unmarshal(rawReactions, &message.Reactions)
		messages = append(messages, message)
	}
	return messages, rows.Err()
}

func (r *Repository) MessageByID(ctx context.Context, messageID uuid.UUID) (*Message, error) {
	message := &Message{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, chat_id, seq, sender_id, client_message_id, type, content,
		       entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at
		FROM messages WHERE id = $1`, messageID,
	).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID, &message.ClientMessageID,
		&message.Type, &message.Content, &message.Entities, &message.Payload, &message.ReplyToID,
		&message.IsPinned, &message.CreatedAt, &message.EditedAt, &message.DeletedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: read message: %w", err)
	}
	return message, nil
}

// ---------------------------------------------------------------- mutations

// Edit rewrites a message body, keeping the previous text in message_edits.
func (r *Repository) Edit(ctx context.Context, messageID, editorID uuid.UUID, content string, entities json.RawMessage) (*Message, []uuid.UUID, error) {
	var message *Message
	var recipients []uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var previous string
		var chatID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT content, chat_id FROM messages WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			messageID).Scan(&previous, &chatID); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: lock message: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO message_edits (message_id, previous_content, edited_by)
			VALUES ($1, $2, $3)`, messageID, previous, editorID); err != nil {
			return fmt.Errorf("messaging: record edit: %w", err)
		}

		updated := &Message{}
		if err := tx.QueryRow(ctx, `
			UPDATE messages
			SET content = $2, entities = COALESCE($3, entities), edited_at = now()
			WHERE id = $1
			RETURNING id, chat_id, seq, sender_id, client_message_id, type, content,
			          entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at`,
			messageID, content, nullableJSON(entities),
		).Scan(&updated.ID, &updated.ChatID, &updated.Seq, &updated.SenderID, &updated.ClientMessageID,
			&updated.Type, &updated.Content, &updated.Entities, &updated.Payload, &updated.ReplyToID,
			&updated.IsPinned, &updated.CreatedAt, &updated.EditedAt, &updated.DeletedAt); err != nil {
			return fmt.Errorf("messaging: update message: %w", err)
		}
		message = updated

		payload, err := json.Marshal(map[string]any{"chat_id": chatID, "message": updated})
		if err != nil {
			return err
		}
		recipients, _, err = appendUserEvents(ctx, tx, chatID, editorID, EventMessageEdited, payload)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return message, recipients, nil
}

// Delete soft-deletes a message so the sequence stays contiguous and other
// devices can reconcile the tombstone.
func (r *Repository) Delete(ctx context.Context, messageID, actorID uuid.UUID) (uuid.UUID, int64, []uuid.UUID, error) {
	var chatID uuid.UUID
	var seq int64
	var recipients []uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			UPDATE messages
			SET deleted_at = now(), deleted_by = $2, content = '', entities = '[]'::jsonb
			WHERE id = $1 AND deleted_at IS NULL
			RETURNING chat_id, seq`, messageID, actorID).Scan(&chatID, &seq); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: delete message: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`DELETE FROM message_attachments WHERE message_id = $1`, messageID); err != nil {
			return err
		}

		payload, err := json.Marshal(map[string]any{
			"chat_id": chatID, "message_id": messageID, "seq": seq,
		})
		if err != nil {
			return err
		}
		recipients, _, err = appendUserEvents(ctx, tx, chatID, actorID, EventMessageDeleted, payload)
		return err
	})
	return chatID, seq, recipients, err
}

// MarkRead advances the caller's read cursor and clears the unread counters.
// The cursor never moves backwards.
func (r *Repository) MarkRead(ctx context.Context, chatID, userID uuid.UUID, uptoSeq int64) (int64, error) {
	var newSeq int64
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE chat_members
		SET last_read_seq = GREATEST(last_read_seq, $3),
		    last_delivered_seq = GREATEST(last_delivered_seq, $3),
		    unread_count = (
		        SELECT count(*) FROM messages
		        WHERE chat_id = $1 AND seq > GREATEST(chat_members.last_read_seq, $3)
		          AND deleted_at IS NULL
		    ),
		    mention_count = (
		        SELECT count(*) FROM message_mentions mm
		        JOIN messages m ON m.id = mm.message_id
		        WHERE mm.user_id = $2 AND m.chat_id = $1
		          AND m.seq > GREATEST(chat_members.last_read_seq, $3)
		    )
		WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL
		RETURNING last_read_seq`, chatID, userID, uptoSeq).Scan(&newSeq)
	if database.IsNoRows(err) {
		return 0, ErrNotMember
	}
	if err != nil {
		return 0, fmt.Errorf("messaging: mark read: %w", err)
	}
	return newSeq, nil
}

// React toggles a reaction: sending the same emoji twice removes it (§52).
func (r *Repository) React(ctx context.Context, messageID, userID uuid.UUID, emoji string) (added bool, chatID uuid.UUID, err error) {
	err = r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT chat_id FROM messages WHERE id = $1 AND deleted_at IS NULL`,
			messageID).Scan(&chatID); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return err
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM message_reactions WHERE message_id = $1 AND user_id = $2 AND emoji = $3`,
			messageID, userID, emoji)
		if err != nil {
			return fmt.Errorf("messaging: remove reaction: %w", err)
		}
		if tag.RowsAffected() > 0 {
			added = false
			return nil
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO message_reactions (message_id, user_id, emoji) VALUES ($1, $2, $3)`,
			messageID, userID, emoji); err != nil {
			return fmt.Errorf("messaging: add reaction: %w", err)
		}
		added = true
		return nil
	})
	return added, chatID, err
}

// SetPinned pins or unpins a message.
func (r *Repository) SetPinned(ctx context.Context, messageID uuid.UUID, pinned bool) (uuid.UUID, error) {
	var chatID uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`UPDATE messages SET is_pinned = $2 WHERE id = $1 AND deleted_at IS NULL RETURNING chat_id`,
		messageID, pinned).Scan(&chatID)
	if database.IsNoRows(err) {
		return uuid.Nil, ErrNotFound
	}
	return chatID, err
}

// SetDraft stores the caller's unsent text so it follows them across devices.
func (r *Repository) SetDraft(ctx context.Context, chatID, userID uuid.UUID, draft string) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE chat_members SET draft = $3 WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
		chatID, userID, draft)
	if err != nil {
		return fmt.Errorf("messaging: set draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// ---------------------------------------------------------------- sync

// EventsSince returns the caller's sync log after a cursor (§9).
func (r *Repository) EventsSince(ctx context.Context, userID uuid.UUID, cursor int64, limit int) ([]Event, int64, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT seq, type, payload, created_at
		FROM user_events
		WHERE user_id = $1 AND seq > $2
		ORDER BY seq
		LIMIT $3`, userID, cursor, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("messaging: events since: %w", err)
	}
	defer rows.Close()

	var events []Event
	for rows.Next() {
		var event Event
		if err := rows.Scan(&event.Seq, &event.Type, &event.Payload, &event.CreatedAt); err != nil {
			return nil, 0, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var latest int64
	if err := r.db.Pool.QueryRow(ctx,
		`SELECT last_seq FROM user_event_counters WHERE user_id = $1`, userID).Scan(&latest); err != nil {
		if !database.IsNoRows(err) {
			return nil, 0, err
		}
	}
	return events, latest, nil
}

// PublishUserEvent appends a single event for one user, used by domains
// outside messaging (calls, news, moderation) that need to reach a device.
func (r *Repository) PublishUserEvent(ctx context.Context, userID uuid.UUID, eventType string, payload any) (int64, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("messaging: marshal event: %w", err)
	}

	var seq int64
	err = r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO user_event_counters (user_id, last_seq) VALUES ($1, 1)
			ON CONFLICT (user_id) DO UPDATE SET last_seq = user_event_counters.last_seq + 1
			RETURNING last_seq`, userID).Scan(&seq); err != nil {
			return fmt.Errorf("messaging: bump event counter: %w", err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO user_events (user_id, seq, type, payload) VALUES ($1, $2, $3, $4)`,
			userID, seq, eventType, body)
		return err
	})
	return seq, err
}

// PruneEvents drops sync-log entries every device has already consumed.
func (r *Repository) PruneEvents(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		DELETE FROM user_events e
		WHERE e.created_at < now() - $1::interval
		  AND e.seq <= COALESCE((
		      SELECT min(d.sync_cursor) FROM devices d
		      WHERE d.user_id = e.user_id AND d.revoked_at IS NULL
		  ), e.seq)`, olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("messaging: prune events: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ChatMemberIDs lists the current members, used for realtime fan-out.
func (r *Repository) ChatMemberIDs(ctx context.Context, chatID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT user_id FROM chat_members WHERE chat_id = $1 AND left_at IS NULL`, chatID)
	if err != nil {
		return nil, fmt.Errorf("messaging: chat members: %w", err)
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

// LastSendAt backs slow-mode enforcement.
func (r *Repository) LastSendAt(ctx context.Context, chatID, userID uuid.UUID) (time.Time, error) {
	var at time.Time
	err := r.db.Pool.QueryRow(ctx, `
		SELECT created_at FROM messages
		WHERE chat_id = $1 AND sender_id = $2
		ORDER BY seq DESC LIMIT 1`, chatID, userID).Scan(&at)
	if database.IsNoRows(err) {
		return time.Time{}, nil
	}
	return at, err
}

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

func forwardChat(f *ForwardInfo) any {
	if f == nil {
		return nil
	}
	return f.ChatID
}

func forwardMessage(f *ForwardInfo) any {
	if f == nil {
		return nil
	}
	return f.MessageID
}

func forwardUser(f *ForwardInfo) any {
	if f == nil {
		return nil
	}
	return f.UserID
}

func forwardSignature(f *ForwardInfo) string {
	if f == nil {
		return ""
	}
	return f.Signature
}
