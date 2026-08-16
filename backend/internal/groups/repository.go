// Package groups implements group and channel administration (§14, §15, §16).
//
// Groups and channels share the `chats` container and the `chat_members`
// membership table with private chats, so posting, sync and moderation reuse
// the messaging module unchanged. What lives here is everything *around* the
// conversation: creation, roles, invite links, join requests and per-post
// statistics.
package groups

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/security"
)

var (
	ErrNotFound      = errors.New("groups: not found")
	ErrNotMember     = errors.New("groups: not a member")
	ErrAlreadyMember = errors.New("groups: already a member")
	ErrChatFull      = errors.New("groups: member limit reached")
)

// Member is one participant, with the profile fields the member list renders.
type Member struct {
	UserID      uuid.UUID  `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Username    *string    `json:"username,omitempty"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	Role        string     `json:"role"`
	CustomTitle string     `json:"custom_title,omitempty"`
	JoinedAt    time.Time  `json:"joined_at"`
	InvitedBy   *uuid.UUID `json:"invited_by,omitempty"`
}

// InviteLink is a shareable join link (§14).
type InviteLink struct {
	ID          uuid.UUID  `json:"id"`
	Slug        string     `json:"slug"`
	URL         string     `json:"url"`
	Name        string     `json:"name,omitempty"`
	MemberLimit *int       `json:"member_limit,omitempty"`
	UsageCount  int        `json:"usage_count"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// JoinRequest is a pending application to a chat that requires approval.
type JoinRequest struct {
	UserID      uuid.UUID `json:"user_id"`
	DisplayName string    `json:"display_name"`
	Username    *string   `json:"username,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Settings are the administrative knobs on a chat.
type Settings struct {
	SlowModeSeconds      int  `json:"slow_mode_seconds"`
	HistoryVisibleToNew  bool `json:"history_visible_to_new"`
	JoinRequiresApproval bool `json:"join_requires_approval"`
	MaxMembers           int  `json:"max_members"`
	AutoDeleteSeconds    int  `json:"auto_delete_seconds"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateChat creates a group or channel with its owner, settings and extension
// row in one transaction.
func (r *Repository) CreateChat(ctx context.Context, chatType, title, description string, ownerID uuid.UUID, isPublic bool, username *string) (uuid.UUID, error) {
	var chatID uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO chats (type, title, description, username, creator_id, is_public, member_count)
			VALUES ($1, $2, $3, $4, $5, $6, 1)
			RETURNING id`,
			chatType, title, description, username, ownerID, isPublic).Scan(&chatID)
		if err != nil {
			if database.IsUniqueViolation(err, "chats_username_key") {
				return ErrUsernameTaken
			}
			return fmt.Errorf("groups: create chat: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'owner')`,
			chatID, ownerID); err != nil {
			return fmt.Errorf("groups: add owner: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO chat_settings (chat_id) VALUES ($1)`, chatID); err != nil {
			return fmt.Errorf("groups: create settings: %w", err)
		}

		// The extension row carries type-specific metadata that would
		// otherwise clutter `chats` with mostly-null columns.
		switch chatType {
		case messaging.ChatGroup:
			_, err = tx.Exec(ctx, `INSERT INTO groups (chat_id) VALUES ($1)`, chatID)
		case messaging.ChatChannel:
			_, err = tx.Exec(ctx,
				`INSERT INTO channels (chat_id, subscriber_count) VALUES ($1, 1)`, chatID)
		}
		if err != nil {
			return fmt.Errorf("groups: create extension row: %w", err)
		}
		return nil
	})

	return chatID, err
}

var ErrUsernameTaken = errors.New("groups: username is already taken")

// AddMember joins a user, enforcing the member limit inside the transaction so
// two concurrent joins cannot both slip past a full chat.
func (r *Repository) AddMember(ctx context.Context, chatID, userID uuid.UUID, role string, invitedBy *uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var memberCount, maxMembers int
		var chatType string
		err := tx.QueryRow(ctx, `
			SELECT c.member_count, COALESCE(s.max_members, 200000), c.type
			FROM chats c
			LEFT JOIN chat_settings s ON s.chat_id = c.id
			WHERE c.id = $1 AND c.deleted_at IS NULL
			FOR UPDATE OF c`, chatID).Scan(&memberCount, &maxMembers, &chatType)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("groups: lock chat: %w", err)
		}
		if memberCount >= maxMembers {
			return ErrChatFull
		}

		// A member who previously left is reinstated rather than duplicated.
		tag, err := tx.Exec(ctx, `
			INSERT INTO chat_members (chat_id, user_id, role, invited_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (chat_id, user_id) DO UPDATE
			SET left_at = NULL, role = EXCLUDED.role, joined_at = now()
			WHERE chat_members.left_at IS NOT NULL`,
			chatID, userID, role, invitedBy)
		if err != nil {
			return fmt.Errorf("groups: insert member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyMember
		}

		return bumpMemberCount(ctx, tx, chatID, chatType, 1)
	})
}

// RemoveMember marks a member as departed. The row is kept so their historical
// messages still resolve to a membership record.
func (r *Repository) RemoveMember(ctx context.Context, chatID, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var chatType string
		if err := tx.QueryRow(ctx,
			`SELECT type FROM chats WHERE id = $1 FOR UPDATE`, chatID).Scan(&chatType); err != nil {
			if database.IsNoRows(err) {
				return ErrNotFound
			}
			return err
		}

		tag, err := tx.Exec(ctx, `
			UPDATE chat_members SET left_at = now()
			WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`, chatID, userID)
		if err != nil {
			return fmt.Errorf("groups: remove member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotMember
		}

		return bumpMemberCount(ctx, tx, chatID, chatType, -1)
	})
}

// bumpMemberCount keeps the denormalised counters on `chats` and `channels`
// consistent with the membership table.
func bumpMemberCount(ctx context.Context, tx pgx.Tx, chatID uuid.UUID, chatType string, delta int) error {
	if _, err := tx.Exec(ctx,
		`UPDATE chats SET member_count = GREATEST(0, member_count + $2), updated_at = now() WHERE id = $1`,
		chatID, delta); err != nil {
		return fmt.Errorf("groups: update member count: %w", err)
	}
	if chatType == messaging.ChatChannel {
		if _, err := tx.Exec(ctx,
			`UPDATE channels SET subscriber_count = GREATEST(0, subscriber_count + $2) WHERE chat_id = $1`,
			chatID, delta); err != nil {
			return fmt.Errorf("groups: update subscriber count: %w", err)
		}
	}
	return nil
}

// SetRole promotes or demotes a member. Ownership is transferred separately.
func (r *Repository) SetRole(ctx context.Context, chatID, userID uuid.UUID, role string, permissions map[string]bool, customTitle string) error {
	var raw []byte
	if len(permissions) > 0 {
		encoded, err := json.Marshal(permissions)
		if err != nil {
			return fmt.Errorf("groups: encode permissions: %w", err)
		}
		raw = encoded
	}

	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_members
		SET role = $3, permissions = $4, custom_title = $5
		WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`,
		chatID, userID, role, raw, customTitle)
	if err != nil {
		return fmt.Errorf("groups: set role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotMember
	}
	return nil
}

// TransferOwnership moves the owner role atomically, so a chat never has zero
// or two owners.
func (r *Repository) TransferOwnership(ctx context.Context, chatID, fromUserID, toUserID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE chat_members SET role = 'admin'
			WHERE chat_id = $1 AND user_id = $2 AND role = 'owner' AND left_at IS NULL`,
			chatID, fromUserID)
		if err != nil {
			return fmt.Errorf("groups: demote previous owner: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}

		tag, err = tx.Exec(ctx, `
			UPDATE chat_members SET role = 'owner', permissions = NULL
			WHERE chat_id = $1 AND user_id = $2 AND left_at IS NULL`, chatID, toUserID)
		if err != nil {
			return fmt.Errorf("groups: promote new owner: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotMember
		}
		return nil
	})
}

// Members lists participants, administrators first.
func (r *Repository) Members(ctx context.Context, chatID uuid.UUID, limit, offset int) ([]Member, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT m.user_id, COALESCE(p.display_name, ''), u.username, p.avatar_media_id,
		       m.role, m.custom_title, m.joined_at, m.invited_by
		FROM chat_members m
		JOIN users u ON u.id = m.user_id
		LEFT JOIN user_profiles p ON p.user_id = m.user_id
		WHERE m.chat_id = $1 AND m.left_at IS NULL
		ORDER BY
			CASE m.role
				WHEN 'owner' THEN 0 WHEN 'admin' THEN 1
				WHEN 'moderator' THEN 2 ELSE 3
			END,
			m.joined_at
		LIMIT $2 OFFSET $3`, chatID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("groups: list members: %w", err)
	}
	defer rows.Close()

	var members []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.DisplayName, &m.Username, &m.AvatarID,
			&m.Role, &m.CustomTitle, &m.JoinedAt, &m.InvitedBy); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// UpdateChat changes the presentation fields of a group or channel.
func (r *Repository) UpdateChat(ctx context.Context, chatID uuid.UUID, title, description *string, photoMediaID *uuid.UUID, username *string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE chats
		SET title = COALESCE($2, title),
		    description = COALESCE($3, description),
		    photo_media_id = COALESCE($4, photo_media_id),
		    username = COALESCE($5, username),
		    updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		chatID, title, description, photoMediaID, username)
	if err != nil {
		if database.IsUniqueViolation(err, "chats_username_key") {
			return ErrUsernameTaken
		}
		return fmt.Errorf("groups: update chat: %w", err)
	}
	return nil
}

func (r *Repository) UpdateSettings(ctx context.Context, chatID uuid.UUID, s Settings) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE chat_settings
		SET slow_mode_seconds = $2, history_visible_to_new = $3,
		    join_requires_approval = $4, max_members = $5,
		    auto_delete_seconds = $6, updated_at = now()
		WHERE chat_id = $1`,
		chatID, s.SlowModeSeconds, s.HistoryVisibleToNew,
		s.JoinRequiresApproval, s.MaxMembers, s.AutoDeleteSeconds)
	if err != nil {
		return fmt.Errorf("groups: update settings: %w", err)
	}
	return nil
}

func (r *Repository) Settings(ctx context.Context, chatID uuid.UUID) (*Settings, error) {
	s := &Settings{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT slow_mode_seconds, history_visible_to_new, join_requires_approval,
		       max_members, auto_delete_seconds
		FROM chat_settings WHERE chat_id = $1`, chatID,
	).Scan(&s.SlowModeSeconds, &s.HistoryVisibleToNew, &s.JoinRequiresApproval,
		&s.MaxMembers, &s.AutoDeleteSeconds)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("groups: read settings: %w", err)
	}
	return s, nil
}

// Delete soft-deletes a chat, hiding it from every member at once.
func (r *Repository) Delete(ctx context.Context, chatID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE chats SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, chatID)
	return err
}

// ---------------------------------------------------------------- invites

// CreateInviteLink mints a link with an unguessable slug.
func (r *Repository) CreateInviteLink(ctx context.Context, chatID, creatorID uuid.UUID, name string, memberLimit *int, expiresAt *time.Time) (*InviteLink, error) {
	slug, err := security.RandomToken(16)
	if err != nil {
		return nil, err
	}

	link := &InviteLink{}
	err = r.db.Pool.QueryRow(ctx, `
		INSERT INTO chat_invite_links (chat_id, slug, created_by, name, member_limit, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, slug, name, member_limit, usage_count, expires_at, created_at`,
		chatID, slug, creatorID, name, memberLimit, expiresAt,
	).Scan(&link.ID, &link.Slug, &link.Name, &link.MemberLimit,
		&link.UsageCount, &link.ExpiresAt, &link.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("groups: create invite link: %w", err)
	}
	return link, nil
}

func (r *Repository) InviteLinks(ctx context.Context, chatID uuid.UUID) ([]InviteLink, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, slug, name, member_limit, usage_count, expires_at, created_at
		FROM chat_invite_links
		WHERE chat_id = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC`, chatID)
	if err != nil {
		return nil, fmt.Errorf("groups: list invite links: %w", err)
	}
	defer rows.Close()

	var links []InviteLink
	for rows.Next() {
		var link InviteLink
		if err := rows.Scan(&link.ID, &link.Slug, &link.Name, &link.MemberLimit,
			&link.UsageCount, &link.ExpiresAt, &link.CreatedAt); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}

func (r *Repository) RevokeInviteLink(ctx context.Context, chatID, linkID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE chat_invite_links SET revoked_at = now()
		 WHERE id = $1 AND chat_id = $2 AND revoked_at IS NULL`, linkID, chatID)
	if err != nil {
		return fmt.Errorf("groups: revoke invite link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ResolvedInvite is a validated link, ready to be used.
type ResolvedInvite struct {
	LinkID           uuid.UUID
	ChatID           uuid.UUID
	ChatType         string
	Title            string
	MemberCount      int
	RequiresApproval bool
}

// ResolveInvite validates a slug and reports what it leads to.
func (r *Repository) ResolveInvite(ctx context.Context, slug string) (*ResolvedInvite, error) {
	invite := &ResolvedInvite{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT l.id, c.id, c.type, c.title, c.member_count,
		       COALESCE(s.join_requires_approval, FALSE)
		FROM chat_invite_links l
		JOIN chats c ON c.id = l.chat_id AND c.deleted_at IS NULL
		LEFT JOIN chat_settings s ON s.chat_id = c.id
		WHERE l.slug = $1
		  AND l.revoked_at IS NULL
		  AND (l.expires_at IS NULL OR l.expires_at > now())
		  AND (l.member_limit IS NULL OR l.usage_count < l.member_limit)`, slug,
	).Scan(&invite.LinkID, &invite.ChatID, &invite.ChatType, &invite.Title,
		&invite.MemberCount, &invite.RequiresApproval)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("groups: resolve invite: %w", err)
	}
	return invite, nil
}

func (r *Repository) CountInviteUse(ctx context.Context, linkID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE chat_invite_links SET usage_count = usage_count + 1 WHERE id = $1`, linkID)
	return err
}

// ---------------------------------------------------------------- join requests

func (r *Repository) CreateJoinRequest(ctx context.Context, chatID, userID uuid.UUID, linkID *uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO chat_join_requests (chat_id, user_id, invite_link_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (chat_id, user_id) DO UPDATE
		SET status = 'pending', created_at = now(), resolved_at = NULL, resolved_by = NULL
		WHERE chat_join_requests.status <> 'pending'`,
		chatID, userID, linkID)
	if err != nil {
		return fmt.Errorf("groups: create join request: %w", err)
	}
	return nil
}

func (r *Repository) JoinRequests(ctx context.Context, chatID uuid.UUID, limit int) ([]JoinRequest, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT j.user_id, COALESCE(p.display_name, ''), u.username, j.created_at
		FROM chat_join_requests j
		JOIN users u ON u.id = j.user_id
		LEFT JOIN user_profiles p ON p.user_id = j.user_id
		WHERE j.chat_id = $1 AND j.status = 'pending'
		ORDER BY j.created_at
		LIMIT $2`, chatID, limit)
	if err != nil {
		return nil, fmt.Errorf("groups: list join requests: %w", err)
	}
	defer rows.Close()

	var requests []JoinRequest
	for rows.Next() {
		var request JoinRequest
		if err := rows.Scan(&request.UserID, &request.DisplayName,
			&request.Username, &request.CreatedAt); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// ResolveJoinRequest approves or rejects an application; approval also adds the
// member, in the same transaction.
func (r *Repository) ResolveJoinRequest(ctx context.Context, chatID, userID, resolverID uuid.UUID, approve bool) error {
	status := "rejected"
	if approve {
		status = "approved"
	}

	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE chat_join_requests
			SET status = $4, resolved_at = now(), resolved_by = $3
			WHERE chat_id = $1 AND user_id = $2 AND status = 'pending'`,
			chatID, userID, resolverID, status)
		if err != nil {
			return fmt.Errorf("groups: resolve join request: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if !approve {
			return nil
		}

		var chatType string
		if err := tx.QueryRow(ctx,
			`SELECT type FROM chats WHERE id = $1 FOR UPDATE`, chatID).Scan(&chatType); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO chat_members (chat_id, user_id, role) VALUES ($1, $2, 'member')
			ON CONFLICT (chat_id, user_id) DO UPDATE SET left_at = NULL, joined_at = now()`,
			chatID, userID); err != nil {
			return fmt.Errorf("groups: add approved member: %w", err)
		}
		return bumpMemberCount(ctx, tx, chatID, chatType, 1)
	})
}

// ---------------------------------------------------------------- channels

// RecordPostView counts a distinct viewer and refreshes the aggregate (§15).
func (r *Repository) RecordPostView(ctx context.Context, messageID, chatID, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO channel_post_views (message_id, user_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, messageID, userID)
		if err != nil {
			return fmt.Errorf("groups: record view: %w", err)
		}
		if tag.RowsAffected() == 0 {
			// Already counted; views are distinct viewers, not impressions.
			return nil
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO channel_post_stats (message_id, chat_id, view_count)
			VALUES ($1, $2, 1)
			ON CONFLICT (message_id) DO UPDATE
			SET view_count = channel_post_stats.view_count + 1, updated_at = now()`,
			messageID, chatID)
		if err != nil {
			return fmt.Errorf("groups: update post stats: %w", err)
		}

		_, err = tx.Exec(ctx,
			`UPDATE messages SET view_count = view_count + 1 WHERE id = $1`, messageID)
		return err
	})
}

// PostStats is the analytics summary for one channel post.
type PostStats struct {
	MessageID     uuid.UUID `json:"message_id"`
	ViewCount     int       `json:"view_count"`
	ForwardCount  int       `json:"forward_count"`
	ReactionCount int       `json:"reaction_count"`
	CommentCount  int       `json:"comment_count"`
}

func (r *Repository) PostStats(ctx context.Context, chatID uuid.UUID, messageIDs []uuid.UUID) ([]PostStats, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.message_id, s.view_count, s.forward_count, s.reaction_count, s.comment_count
		FROM channel_post_stats s
		WHERE s.chat_id = $1 AND s.message_id = ANY($2::uuid[])`, chatID, messageIDs)
	if err != nil {
		return nil, fmt.Errorf("groups: read post stats: %w", err)
	}
	defer rows.Close()

	var stats []PostStats
	for rows.Next() {
		var s PostStats
		if err := rows.Scan(&s.MessageID, &s.ViewCount, &s.ForwardCount,
			&s.ReactionCount, &s.CommentCount); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// Discover lists public chats for the directory, ranked by size.
func (r *Repository) Discover(ctx context.Context, chatType, query string, limit int) ([]messaging.Chat, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, type, title, description, username, photo_media_id,
		       member_count, is_public, created_at
		FROM chats
		WHERE is_public AND deleted_at IS NULL
		  AND ($1 = '' OR type = $1)
		  AND ($2 = '' OR title ILIKE '%' || $2 || '%' OR username::text ILIKE '%' || $2 || '%')
		ORDER BY member_count DESC
		LIMIT $3`, chatType, query, limit)
	if err != nil {
		return nil, fmt.Errorf("groups: discover: %w", err)
	}
	defer rows.Close()

	var chats []messaging.Chat
	for rows.Next() {
		var chat messaging.Chat
		if err := rows.Scan(&chat.ID, &chat.Type, &chat.Title, &chat.Description,
			&chat.Username, &chat.PhotoMediaID, &chat.MemberCount,
			&chat.IsPublic, &chat.CreatedAt); err != nil {
			return nil, err
		}
		chats = append(chats, chat)
	}
	return chats, rows.Err()
}
