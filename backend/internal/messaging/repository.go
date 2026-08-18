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
	// IsBroadcast marks a group where only staff may post.
	IsBroadcast bool
	// CustomRole is the name of the named permission bundle this member
	// holds, if any. It does not change the hierarchy — who may act on whom
	// is still decided by the built-in role — it changes what they may do.
	CustomRole string
}

func (r *Repository) ChatContextFor(ctx context.Context, chatID, userID uuid.UUID) (*ChatContext, error) {
	var (
		result       ChatContext
		role         *string
		rawPerms     []byte
		slowMode     *int
		autoDelete   *int
		chatDefaults []byte
		isBroadcast  *bool
		roleName     *string
		rolePerms    []byte
	)
	err := r.db.Pool.QueryRow(ctx, `
		SELECT c.id, c.type, c.member_count, c.last_seq,
		       m.role, m.permissions, m.muted_until,
		       s.slow_mode_seconds, s.auto_delete_seconds, s.default_permissions,
		       g.is_broadcast, r.name, r.permissions
		FROM chats c
		LEFT JOIN chat_members m ON m.chat_id = c.id AND m.user_id = $2 AND m.left_at IS NULL
		LEFT JOIN chat_settings s ON s.chat_id = c.id
		LEFT JOIN groups g ON g.chat_id = c.id
		LEFT JOIN group_roles r ON r.id = m.custom_role_id
		WHERE c.id = $1 AND c.deleted_at IS NULL`,
		chatID, userID,
	).Scan(&result.ChatID, &result.ChatType, &result.MemberCount, &result.LastSeq,
		&role, &rawPerms, &result.MutedUntil, &slowMode, &autoDelete, &chatDefaults,
		&isBroadcast, &roleName, &rolePerms)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("messaging: chat context: %w", err)
	}

	if role != nil {
		result.IsMember = true
		result.Role = *role
		// Three layers, narrowest last: what the role grants, what the chat
		// restricts for everyone, and what this one member has been given or
		// denied personally.
		//
		// The chat-wide layer applies to ordinary members only. It is how an
		// admin says "nobody may post media here" — and if it applied to staff
		// too, the same switch would lock the admins out of moderating the chat
		// they had just restricted.
		permissions := PermissionsForRole(*role, result.ChatType)
		if *role == RoleMember || *role == RoleRestricted {
			permissions = applyOverrides(permissions, chatDefaults)
			// A broadcast group reads like a channel: staff post, everyone
			// else listens. It sits under the personal overrides so one
			// member can still be granted a voice.
			if isBroadcast != nil && *isBroadcast {
				permissions.SendMessages = false
				permissions.SendMedia = false
				permissions.SendFiles = false
				permissions.SendPolls = false
				permissions.SendStickers = false
			}
		}
		// A named role sits between the chat-wide defaults and the member's
		// own overrides: it is a bundle handed to several people at once, and
		// an exception granted to one of them individually still wins.
		permissions = applyOverrides(permissions, rolePerms)
		result.Permissions = applyOverrides(permissions, rawPerms)
		if roleName != nil {
			result.CustomRole = *roleName
		}
	}
	if slowMode != nil {
		result.SlowMode = *slowMode
	}
	if autoDelete != nil {
		result.AutoDelete = *autoDelete
	}
	if isBroadcast != nil {
		result.IsBroadcast = *isBroadcast
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

		// A unique violation is returned untouched so the caller below can
		// recognise it. It cannot be handled here: PostgreSQL aborts the whole
		// transaction on the error, and every further statement in it —
		// including a SELECT to find the winner's row — fails with 25P02 until
		// it is rolled back. The re-read has to happen after.
		if _, err := tx.Exec(ctx,
			`INSERT INTO private_chat_keys (chat_id, user_a_id, user_b_id) VALUES ($1, $2, $3)`,
			chatID, low, high); err != nil {
			if database.IsUniqueViolation(err) {
				return err
			}
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

	switch {
	case err == nil:
		return chatID, created, nil

	case database.IsUniqueViolation(err):
		// Someone else created it first. Their row is committed and ours is
		// rolled back — including the `chats` row this transaction inserted —
		// so adopt theirs.
		if err := r.db.Pool.QueryRow(ctx,
			`SELECT chat_id FROM private_chat_keys WHERE user_a_id = $1 AND user_b_id = $2`,
			low, high).Scan(&chatID); err != nil {
			return uuid.Nil, false, fmt.Errorf("messaging: adopt racing private chat: %w", err)
		}
		return chatID, false, nil

	default:
		return uuid.Nil, false, err
	}
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

// ChatListQuery narrows the chat list.
//
// FolderID is a field rather than a fourth positional argument because
// the folder filter is the sort of thing that grows — and a call site reading
// `ListChats(ctx, id, 50, nil, nil)` says nothing about which nil is which.
type ChatListQuery struct {
	UserID uuid.UUID
	Limit  int
	Before *time.Time
	// FolderID narrows the list to one saved filter. Nil is the main list,
	// which shows everything including chats no folder matches.
	FolderID *uuid.UUID
}

// ListChats returns the caller's chat list, most recently active first.
func (r *Repository) ListChats(ctx context.Context, q ChatListQuery) ([]Chat, error) {
	// The LATERAL resolves the other person in a one-to-one chat. Such a chat
	// has no title of its own, so without this every private and secret
	// conversation arrives nameless and each client has to work out which
	// member is not the viewer. It is restricted to those two types and to one
	// row, so it costs an index lookup on chats that have a peer and nothing at
	// all on those that do not.
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.type, c.community_id, c.title, c.description, c.username,
		       c.photo_media_id, c.creator_id, c.last_seq, c.last_message_at,
		       c.member_count, c.is_public, c.created_at,
		       m.role, m.permissions, m.last_read_seq, m.unread_count, m.mention_count,
		       m.is_pinned, m.is_archived, m.muted_until, m.draft, m.joined_at,
		       peer.user_id, peer.display_name, peer.username, peer.avatar_media_id, peer.is_bot
		FROM chat_members m
		JOIN chats c ON c.id = m.chat_id
		LEFT JOIN LATERAL (
			SELECT u.id AS user_id,
			       COALESCE(NULLIF(p.display_name, ''), u.username, '') AS display_name,
			       u.username, p.avatar_media_id, u.is_bot
			FROM chat_members pm
			JOIN users u ON u.id = pm.user_id AND u.deleted_at IS NULL
			LEFT JOIN user_profiles p ON p.user_id = u.id
			WHERE pm.chat_id = c.id AND pm.user_id <> $1 AND pm.left_at IS NULL
			LIMIT 1
		) peer ON c.type IN ('private', 'secret')
		WHERE m.user_id = $1 AND m.left_at IS NULL AND c.deleted_at IS NULL
		  AND ($3::timestamptz IS NULL OR c.last_message_at < $3)
		  -- A folder is a saved filter, so narrowing by one is a WHERE clause
		  -- rather than a different query. The predicate is shared with the
		  -- badge count, because a folder's list and its badge disagreeing
		  -- about what it contains is exactly the bug that would follow from
		  -- writing it twice.
		  AND ($4::uuid IS NULL OR EXISTS (
		      SELECT 1 FROM chat_folders f
		      WHERE f.id = $4 AND f.owner_id = $1 AND (`+folderPredicate+`)
		  ))
		ORDER BY m.is_pinned DESC, c.last_message_at DESC NULLS LAST, c.created_at DESC
		LIMIT $2`, q.UserID, q.Limit, q.Before, q.FolderID)
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
			peer     ChatPeer
			peerID   *uuid.UUID
			peerName *string
			peerBot  *bool
		)
		if err := rows.Scan(&chat.ID, &chat.Type, &chat.CommunityID, &chat.Title, &chat.Description,
			&chat.Username, &chat.PhotoMediaID, &chat.CreatorID, &chat.LastSeq, &chat.LastMessageAt,
			&chat.MemberCount, &chat.IsPublic, &chat.CreatedAt,
			&member.Role, &rawPerms, &member.LastReadSeq, &member.UnreadCount, &member.MentionCount,
			&member.IsPinned, &member.IsArchived, &member.MutedUntil, &member.Draft, &member.JoinedAt,
			&peerID, &peerName, &peer.Username, &peer.AvatarID, &peerBot); err != nil {
			return nil, err
		}
		member.Permissions = applyOverrides(PermissionsForRole(member.Role, chat.Type), rawPerms)
		chat.Membership = &member

		// Absent for a group or channel, and for a one-to-one chat whose other
		// member has deleted their account — which is a real state, not an
		// error, and leaves the conversation readable but nameless.
		if peerID != nil {
			peer.UserID = *peerID
			if peerName != nil {
				peer.DisplayName = *peerName
			}
			if peerBot != nil {
				peer.IsBot = *peerBot
			}
			chat.Peer = &peer
		}
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
	// ReplyMarkup is a bot's inline keyboard; nil for everything else.
	ReplyMarkup json.RawMessage
	IsSilent    bool
	// AsChat posts on behalf of the chat itself rather than a person, storing
	// no sender at all. It is how a channel post is mirrored into its
	// discussion group: naming the admin who wrote it there would leak the
	// authorship that `signature_enabled` exists to control.
	//
	// Such a message has no idempotency key to be checked against — the
	// partial unique index skips NULL senders — so a caller using it is
	// responsible for not asking twice. The one caller that does holds a
	// primary key on the post it is mirroring, which is a stronger guarantee.
	AsChat bool
	// TopicID files the message under a forum topic. Nil in every chat that
	// is not a forum, and never nil in one that is.
	TopicID *uuid.UUID
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
	if !p.AsChat {
		if existing, err := r.messageByClientID(ctx, p.ChatID, p.SenderID, p.ClientMessageID); err == nil {
			return &SendResult{Message: existing, Duplicate: true}, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
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
				forward_signature, is_silent, reply_markup, topic_id, author_signature
			) VALUES ($1, $2, CASE WHEN $16::boolean THEN NULL ELSE $3::uuid END,
			          $4, $5, $6, COALESCE($7, '[]'::jsonb), COALESCE($8, '{}'::jsonb),
			          $9, $10, $11, $12, $13, $14, $15, $17,
			          -- Resolved here rather than in Go so the name is taken
			          -- inside the same transaction that assigns the sequence:
			          -- a separate read could catch the admin mid-rename and
			          -- sign the post with a name that was never theirs.
			          -- The subquery yields nothing at all unless this is a
			          -- channel with signatures on and the sender is staff, so
			          -- an ordinary chat pays one index probe that misses.
			          --
			          -- An admin with no custom title, no display name and no
			          -- username signs with nothing. That is the honest
			          -- answer: there is no name to sign with, and reaching
			          -- for the phone number to fill the gap would publish it.
			          COALESCE((
			              SELECT COALESCE(NULLIF(cm.custom_title, ''),
			                              NULLIF(p.display_name, ''), u.username, '')
			              FROM channels ch
			              JOIN chat_members cm
			                ON cm.chat_id = ch.chat_id AND cm.user_id = $3
			              JOIN users u ON u.id = $3
			              LEFT JOIN user_profiles p ON p.user_id = u.id
			              WHERE ch.chat_id = $1 AND ch.signature_enabled
			                AND cm.role IN ('owner', 'admin', 'moderator')
			          ), ''))
			RETURNING id, chat_id, seq, sender_id, client_message_id, type, content,
			          entities, payload, reply_to_id, is_pinned, created_at, edited_at, deleted_at,
			          reply_markup, topic_id, author_signature`,
			p.ChatID, seq, p.SenderID, p.ClientMessageID, p.Type, p.Content,
			nullableJSON(p.Entities), nullableJSON(p.Payload), p.ReplyToID,
			forwardChat(p.Forward), forwardMessage(p.Forward), forwardUser(p.Forward),
			forwardSignature(p.Forward), p.IsSilent, nullableJSON(p.ReplyMarkup), p.AsChat,
			p.TopicID,
		).Scan(&message.ID, &message.ChatID, &message.Seq, &message.SenderID, &message.ClientMessageID,
			&message.Type, &message.Content, &message.Entities, &message.Payload, &message.ReplyToID,
			&message.IsPinned, &message.CreatedAt, &message.EditedAt, &message.DeletedAt,
			&message.ReplyMarkup, &message.TopicID, &message.AuthorSignature)
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

		if p.TopicID != nil {
			// The topic's counters move with the message rather than being
			// recomputed later: a topic list that lags the messages in it is
			// the same defect as a chat list that lags its chats.
			if _, err := tx.Exec(ctx, `
				UPDATE forum_topics
				SET message_count = message_count + 1, last_message_at = $2
				WHERE id = $1`, *p.TopicID, message.CreatedAt); err != nil {
				return fmt.Errorf("messaging: advance topic: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				UPDATE forum_topic_reads
				SET unread_count = unread_count + 1
				WHERE topic_id = $1 AND user_id <> $2`, *p.TopicID, p.SenderID); err != nil {
				return fmt.Errorf("messaging: bump topic unread: %w", err)
			}
		}

		// The sender's own cursor moves with the message: their device already
		// has it, so it must never count as unread. A message posted by the
		// chat has no sender whose cursor could move.
		if !p.AsChat {
			if _, err := tx.Exec(ctx, `
				UPDATE chat_members
				SET last_read_seq = $3, last_delivered_seq = $3
				WHERE chat_id = $1 AND user_id = $2`, p.ChatID, p.SenderID, seq); err != nil {
				return fmt.Errorf("messaging: advance sender cursor: %w", err)
			}
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
			WHERE m.chat_id = $1 AND ($4::boolean OR m.user_id <> $2) AND m.left_at IS NULL`,
			p.ChatID, p.SenderID, p.MentionUserIDs, p.AsChat); err != nil {
			return fmt.Errorf("messaging: bump unread counts: %w", err)
		}

		payload, err := json.Marshal(map[string]any{
			"chat_id": p.ChatID,
			"message": message,
		})
		if err != nil {
			return fmt.Errorf("messaging: marshal event: %w", err)
		}

		recipients, seqs, err := appendUserEvents(ctx, tx, p.ChatID, EventMessageNew, payload)
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
// appendUserEvents writes one event to every member's log, the actor included.
//
// Including the person who caused the event is the point. The log is per user,
// not per device, and it is the only thing a second device reads to catch up
// (§9) — so excluding the actor meant their tablet never learned what their
// phone had just sent, edited or deleted. It would find the message only by
// refetching the whole conversation, and the chat list would go on showing a
// stale last message until it did.
//
// The device that caused the event receives its own echo. That costs one frame
// and is harmless: every client already reconciles on `client_message_id` and
// upserts by message id, because a retried send has always been able to arrive
// twice.
//
// This is deliberately not the same question as the unread counter, which does
// still skip the actor — a message you sent is not unread for you. That
// exclusion lives in its own statement.
func appendUserEvents(ctx context.Context, tx pgx.Tx, chatID uuid.UUID, eventType string, payload []byte) ([]uuid.UUID, map[uuid.UUID]int64, error) {
	rows, err := tx.Query(ctx, `
		WITH recipients AS (
			SELECT m.user_id
			FROM chat_members m
			WHERE m.chat_id = $1 AND m.left_at IS NULL
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
		SELECT user_id, last_seq, $2, $3::jsonb FROM bumped
		RETURNING user_id, seq`,
		chatID, eventType, payload)
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

// HistoryQuery narrows a page of a chat's history.
//
// It is a struct rather than a growing parameter list, because the same query
// now serves three readers with different filters — the conversation itself,
// one forum topic within it, and the comment thread under a channel post —
// and a fourth positional *int64 would have been unreadable at every call
// site.
type HistoryQuery struct {
	ChatID    uuid.UUID
	ViewerID  uuid.UUID
	BeforeSeq *int64
	AfterSeq  *int64
	Limit     int
	// ReplyToID narrows to the direct replies to one message, which is what a
	// comment thread under a mirrored channel post is.
	ReplyToID *uuid.UUID
	// TopicID narrows to one forum topic.
	TopicID *uuid.UUID
}

// History returns messages in a chat, newest first, seeking by sequence.
func (r *Repository) History(ctx context.Context, q HistoryQuery) ([]Message, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.id, m.chat_id, m.seq, m.sender_id, m.client_message_id, m.type,
		       CASE WHEN m.deleted_at IS NULL THEN m.content ELSE '' END,
		       m.entities, m.payload, m.reply_to_id,
		       m.forward_from_chat_id, m.forward_from_message_id, m.forward_from_user_id,
		       m.forward_signature, m.author_signature, m.topic_id, m.is_pinned, m.view_count,
		       m.created_at, m.edited_at, m.deleted_at, m.reply_markup,
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
		  -- Everything at or below the viewer's watermark was cleared by them
		  -- and must stay invisible to them alone; the rows are untouched and
		  -- every other member still reads them.
		  AND m.seq > COALESCE((
		      SELECT cm.history_cleared_seq FROM chat_members cm
		      WHERE cm.chat_id = $1 AND cm.user_id = $2
		  ), 0)
		  AND ($6::uuid IS NULL OR m.reply_to_id = $6)
		  AND ($7::uuid IS NULL OR m.topic_id = $7)
		ORDER BY m.seq DESC
		LIMIT $5`, q.ChatID, q.ViewerID, q.BeforeSeq, q.AfterSeq, q.Limit, q.ReplyToID, q.TopicID)
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
			&forwardSig, &message.AuthorSignature, &message.TopicID, &message.IsPinned, &message.ViewCount,
			&message.CreatedAt, &message.EditedAt, &message.DeletedAt,
			&message.ReplyMarkup, &rawAttach, &rawReactions); err != nil {
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
		recipients, _, err = appendUserEvents(ctx, tx, chatID, EventMessageEdited, payload)
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
		recipients, _, err = appendUserEvents(ctx, tx, chatID, EventMessageDeleted, payload)
		return err
	})
	return chatID, seq, recipients, err
}

// ClearHistory empties a chat for the caller, and optionally for everyone.
//
// The one-sided form writes no rows to `messages` at all: it moves a
// per-member watermark up to the chat's current sequence, and every read path
// filters below it. That makes the operation O(1) whatever the history is
// worth, survives the other member continuing to post, and cannot corrupt a
// conversation the caller only wanted out of their own view.
//
// `forEveryone` is the destructive form. It tombstones the messages the way
// deleting each one individually would, so the other side sees them go too —
// the caller must be entitled to that, which the service checks before
// calling.
func (r *Repository) ClearHistory(ctx context.Context, chatID, actorID uuid.UUID, forEveryone bool) (int64, []uuid.UUID, error) {
	var watermark int64
	var recipients []uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// The chat row is locked so the watermark cannot land above a message
		// that a concurrent send is still writing — which would hide it from
		// the caller for good.
		if err := tx.QueryRow(ctx,
			`SELECT last_seq FROM chats WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`,
			chatID).Scan(&watermark); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return fmt.Errorf("messaging: lock chat: %w", err)
		}

		if forEveryone {
			if _, err := tx.Exec(ctx, `
				UPDATE messages
				SET deleted_at = now(), deleted_by = $2, content = '',
				    entities = '[]'::jsonb, payload = '{}'::jsonb, reply_markup = NULL
				WHERE chat_id = $1 AND seq <= $3 AND deleted_at IS NULL`,
				chatID, actorID, watermark); err != nil {
				return fmt.Errorf("messaging: tombstone history: %w", err)
			}
			if _, err := tx.Exec(ctx, `
				DELETE FROM message_attachments a
				USING messages m
				WHERE a.message_id = m.id AND m.chat_id = $1 AND m.seq <= $2`,
				chatID, watermark); err != nil {
				return fmt.Errorf("messaging: drop attachments: %w", err)
			}
			// Every member is watermarked, not just the caller. The tombstones
			// would otherwise stay in everyone else's history as a row of
			// "message deleted" placeholders, which is not what deleting a
			// conversation for both sides is supposed to leave behind.
			if _, err := tx.Exec(ctx, `
				UPDATE chat_members
				SET history_cleared_seq = GREATEST(history_cleared_seq, $2),
				    last_read_seq = GREATEST(last_read_seq, $2),
				    last_delivered_seq = GREATEST(last_delivered_seq, $2),
				    unread_count = 0, mention_count = 0
				WHERE chat_id = $1 AND left_at IS NULL`, chatID, watermark); err != nil {
				return fmt.Errorf("messaging: reset counters: %w", err)
			}
		}

		tag, err := tx.Exec(ctx, `
			UPDATE chat_members
			SET history_cleared_seq = GREATEST(history_cleared_seq, $3),
			    last_read_seq = GREATEST(last_read_seq, $3),
			    last_delivered_seq = GREATEST(last_delivered_seq, $3),
			    unread_count = 0, mention_count = 0
			WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
			chatID, actorID, watermark)
		if err != nil {
			return fmt.Errorf("messaging: clear history: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotMember
		}

		payload, err := json.Marshal(map[string]any{
			"chat_id": chatID, "upto_seq": watermark, "for_everyone": forEveryone,
		})
		if err != nil {
			return err
		}
		if forEveryone {
			recipients, _, err = appendUserEvents(ctx, tx, chatID, EventChatHistoryCleared, payload)
			return err
		}

		// A one-sided clear is nobody else's business, but the caller's other
		// devices still have to be told, or they keep showing the history
		// this one just dropped.
		var seq int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO user_event_counters (user_id, last_seq) VALUES ($1, 1)
			ON CONFLICT (user_id) DO UPDATE SET last_seq = user_event_counters.last_seq + 1
			RETURNING last_seq`, actorID).Scan(&seq); err != nil {
			return fmt.Errorf("messaging: bump event counter: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO user_events (user_id, seq, type, payload) VALUES ($1, $2, $3, $4)`,
			actorID, seq, EventChatHistoryCleared, payload); err != nil {
			return fmt.Errorf("messaging: append clear event: %w", err)
		}
		recipients = []uuid.UUID{actorID}
		return nil
	})
	return watermark, recipients, err
}

// MarkRead advances the caller's read cursor and clears the unread counters.
// The cursor never moves backwards.
func (r *Repository) MarkRead(ctx context.Context, chatID, userID uuid.UUID, uptoSeq int64) (int64, int64, error) {
	var newSeq, previousSeq int64
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE chat_members
		SET last_read_seq = GREATEST(chat_members.last_read_seq, $3),
		    last_delivered_seq = GREATEST(chat_members.last_delivered_seq, $3),
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
		-- Joining the table to itself is how the pre-update value is read: the
		-- joined copy sees the snapshot the statement started from, so the old
		-- cursor comes back alongside the new one. The caller then knows
		-- exactly which messages this call passed and records a receipt for
		-- those and no others.
		FROM chat_members old
		WHERE old.chat_id = chat_members.chat_id AND old.user_id = chat_members.user_id
		  AND chat_members.chat_id = $1 AND chat_members.user_id = $2
		  AND chat_members.left_at IS NULL
		RETURNING chat_members.last_read_seq, old.last_read_seq`,
		chatID, userID, uptoSeq).Scan(&newSeq, &previousSeq)
	if database.IsNoRows(err) {
		return 0, 0, ErrNotMember
	}
	if err != nil {
		return 0, 0, fmt.Errorf("messaging: mark read: %w", err)
	}
	return newSeq, previousSeq, nil
}

// ReadReceipt is one person who has read a message.
type ReadReceipt struct {
	UserID      uuid.UUID  `json:"user_id"`
	DisplayName string     `json:"display_name"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	ReadAt      time.Time  `json:"read_at"`
}

// RecordReads writes a row per message the cursor has just passed (§7).
//
// The cursor answers "how far has this person read", which is all a chat list
// needs. It cannot answer "who has read this message", which is what a sender
// looks for in a group — so `message_reads` holds that, written from the same
// call that moves the cursor.
//
// It is bounded on purpose. Somebody returning to a chat with two thousand
// unread messages would otherwise write two thousand rows per member, and the
// answer people actually want is about recent messages. Older ones fall back
// to the cursor, which is exact for "read up to here" and always available.
func (r *Repository) RecordReads(ctx context.Context, chatID, userID uuid.UUID, fromSeq, toSeq int64, limit int) error {
	if toSeq <= fromSeq {
		return nil
	}
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO message_reads (message_id, user_id)
		SELECT m.id, $2
		FROM messages m
		WHERE m.chat_id = $1 AND m.seq > $3 AND m.seq <= $4
		  AND m.deleted_at IS NULL
		  -- A sender has read their own message by definition; a row saying so
		  -- is noise in every receipt list.
		  AND (m.sender_id IS NULL OR m.sender_id <> $2)
		ORDER BY m.seq DESC
		LIMIT $5
		ON CONFLICT DO NOTHING`, chatID, userID, fromSeq, toSeq, limit)
	if err != nil {
		return fmt.Errorf("messaging: record reads: %w", err)
	}
	return nil
}

// ReadReceipts lists who has read one message.
//
// Ordered by when they read it, so the list reads as it happened rather than
// in whatever order the join produced.
func (r *Repository) ReadReceipts(ctx context.Context, messageID uuid.UUID, limit int) ([]ReadReceipt, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT rd.user_id,
		       COALESCE(NULLIF(p.display_name, ''), u.username, '') AS display_name,
		       p.avatar_media_id, rd.read_at
		FROM message_reads rd
		JOIN users u ON u.id = rd.user_id AND u.deleted_at IS NULL
		LEFT JOIN user_profiles p ON p.user_id = rd.user_id
		WHERE rd.message_id = $1
		ORDER BY rd.read_at
		LIMIT $2`, messageID, limit)
	if err != nil {
		return nil, fmt.Errorf("messaging: read receipts: %w", err)
	}
	defer rows.Close()

	receipts := []ReadReceipt{}
	for rows.Next() {
		var receipt ReadReceipt
		if err := rows.Scan(&receipt.UserID, &receipt.DisplayName,
			&receipt.AvatarID, &receipt.ReadAt); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	return receipts, rows.Err()
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
// SetDraft stores a half-written message so it survives closing the app.
//
// It refuses to do so for an encrypted chat. A draft is plaintext, and
// `chat_members.draft` is an ordinary server-side column: storing one would put
// the beginning of a secret message on the server in clear, which is exactly
// what the conversation exists to prevent. The condition is part of the UPDATE
// rather than a lookup beforehand, so enforcing it costs nothing — drafts are
// written on a keystroke timer and a second round trip each time would be felt.
func (r *Repository) SetDraft(ctx context.Context, chatID, userID uuid.UUID, draft string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_members m SET draft = $3
		FROM chats c
		WHERE m.chat_id = $1 AND m.user_id = $2 AND m.left_at IS NULL
		  AND c.id = m.chat_id AND c.type <> 'secret'`,
		chatID, userID, draft)
	if err != nil {
		return fmt.Errorf("messaging: set draft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Not a member, or the chat is encrypted. The caller turns this into a
		// message; both are a refusal to store the draft.
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

// PeerKnowsSender reports whether the other member of a one-to-one chat has
// the sender in their contacts.
//
// It is the test a restricted account is measured against: someone who has
// put you in their address book has invited the conversation, and messaging
// them is not cold outreach. Saved Messages has no other member, so it
// answers true — an account is never restricted from talking to itself.
func (r *Repository) PeerKnowsSender(ctx context.Context, chatID, senderID uuid.UUID) (bool, error) {
	var known bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT NOT EXISTS (
		    SELECT 1
		    FROM chat_members m
		    WHERE m.chat_id = $1 AND m.user_id <> $2 AND m.left_at IS NULL
		      AND NOT EXISTS (
		          SELECT 1 FROM contacts c
		          WHERE c.owner_id = m.user_id AND c.contact_id = $2
		      )
		)`, chatID, senderID).Scan(&known)
	if err != nil {
		return false, fmt.Errorf("messaging: check peer contact: %w", err)
	}
	return known, nil
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
