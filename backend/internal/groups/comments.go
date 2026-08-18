package groups

// Channel comments and the linked discussion group (§15).
//
// A comment is not a new kind of message. A channel with a discussion group
// gets each of its posts mirrored into that group, and the comments are
// ordinary replies to the mirrored copy — so permissions, moderation,
// reactions, editing, deletion and search all apply to them without a single
// special case, and a person who opens the discussion group directly sees the
// same conversation the comment sheet shows.
//
// What the schema was missing is the correspondence between the two copies.
// `channel_post_comments` is that correspondence, and everything here is
// either establishing it or reading through it.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var (
	ErrNotAChannel       = errors.New("groups: that chat is not a channel")
	ErrNotAGroup         = errors.New("groups: that chat is not a group")
	ErrAlreadyLinked     = errors.New("groups: one of those chats is already linked")
	ErrNoDiscussionGroup = errors.New("groups: this channel has no discussion group")
	ErrCommentsDisabled  = errors.New("groups: comments are switched off for this channel")
)

// Discussion is the link between a channel and the group carrying its comments.
type Discussion struct {
	ChannelChatID    uuid.UUID `json:"channel_chat_id"`
	DiscussionChatID uuid.UUID `json:"discussion_chat_id"`
	CommentsEnabled  bool      `json:"comments_enabled"`
}

// CommentThread names where the comments on one post live.
type CommentThread struct {
	PostMessageID    uuid.UUID `json:"post_message_id"`
	DiscussionChatID uuid.UUID `json:"discussion_chat_id"`
	// RootMessageID is the mirrored copy of the post inside the discussion
	// group. Comments are its direct replies.
	RootMessageID uuid.UUID `json:"root_message_id"`
	CommentCount  int       `json:"comment_count"`
}

// ------------------------------------------------------------- repository

// LinkDiscussion attaches a group to a channel, both ways, in one transaction.
//
// Both rows are written together because a half-link is worse than none: a
// channel pointing at a group that does not point back would accept comments
// the group's own members could not find.
func (r *Repository) LinkDiscussion(ctx context.Context, channelID, groupID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var existingDiscussion *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT discussion_chat_id FROM channels WHERE chat_id = $1 FOR UPDATE`,
			channelID).Scan(&existingDiscussion)
		if database.IsNoRows(err) {
			return ErrNotAChannel
		}
		if err != nil {
			return fmt.Errorf("groups: lock channel: %w", err)
		}

		var existingChannel *uuid.UUID
		err = tx.QueryRow(ctx,
			`SELECT linked_channel_id FROM groups WHERE chat_id = $1 FOR UPDATE`,
			groupID).Scan(&existingChannel)
		if database.IsNoRows(err) {
			return ErrNotAGroup
		}
		if err != nil {
			return fmt.Errorf("groups: lock group: %w", err)
		}

		// Relinking the same pair is idempotent; anything else has to be
		// unlinked first, or the posts already mirrored into the old group
		// would lose the thread they belong to.
		if existingDiscussion != nil && *existingDiscussion != groupID {
			return ErrAlreadyLinked
		}
		if existingChannel != nil && *existingChannel != channelID {
			return ErrAlreadyLinked
		}

		if _, err := tx.Exec(ctx,
			`UPDATE channels SET discussion_chat_id = $2 WHERE chat_id = $1`,
			channelID, groupID); err != nil {
			return fmt.Errorf("groups: link discussion: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE groups SET linked_channel_id = $2 WHERE chat_id = $1`,
			groupID, channelID); err != nil {
			return fmt.Errorf("groups: link channel: %w", err)
		}
		return nil
	})
}

// UnlinkDiscussion detaches whatever group is linked, both ways.
//
// The threads already mirrored are left alone: they are real messages in a
// real group, and deleting a conversation because its channel moved on would
// destroy other people's replies.
func (r *Repository) UnlinkDiscussion(ctx context.Context, channelID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var groupID *uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT discussion_chat_id FROM channels WHERE chat_id = $1 FOR UPDATE`,
			channelID).Scan(&groupID)
		if database.IsNoRows(err) {
			return ErrNotAChannel
		}
		if err != nil {
			return fmt.Errorf("groups: lock channel: %w", err)
		}
		if groupID == nil {
			return ErrNoDiscussionGroup
		}

		if _, err := tx.Exec(ctx,
			`UPDATE channels SET discussion_chat_id = NULL WHERE chat_id = $1`,
			channelID); err != nil {
			return fmt.Errorf("groups: unlink discussion: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE groups SET linked_channel_id = NULL WHERE chat_id = $1`,
			*groupID); err != nil {
			return fmt.Errorf("groups: unlink channel: %w", err)
		}
		return nil
	})
}

// Discussion reports the link, if there is one.
func (r *Repository) Discussion(ctx context.Context, channelID uuid.UUID) (*Discussion, error) {
	result := &Discussion{ChannelChatID: channelID}
	var discussionID *uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT discussion_chat_id, comments_enabled FROM channels WHERE chat_id = $1`,
		channelID).Scan(&discussionID, &result.CommentsEnabled)
	if database.IsNoRows(err) {
		return nil, ErrNotAChannel
	}
	if err != nil {
		return nil, fmt.Errorf("groups: read discussion link: %w", err)
	}
	if discussionID == nil {
		return nil, ErrNoDiscussionGroup
	}
	result.DiscussionChatID = *discussionID
	return result, nil
}

// CommentThread returns the mirror for a post, or ErrNotFound if there is none
// yet.
func (r *Repository) CommentThread(ctx context.Context, postID uuid.UUID) (*CommentThread, error) {
	thread := &CommentThread{PostMessageID: postID}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT c.discussion_chat_id, c.discussion_message_id,
		       COALESCE(s.comment_count, 0)
		FROM channel_post_comments c
		LEFT JOIN channel_post_stats s ON s.message_id = c.post_message_id
		WHERE c.post_message_id = $1`, postID,
	).Scan(&thread.DiscussionChatID, &thread.RootMessageID, &thread.CommentCount)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("groups: read comment thread: %w", err)
	}
	return thread, nil
}

// RecordCommentThread claims the mirror for a post.
//
// It reports whether this caller's mirror is the one that stuck. Two people
// opening the comments on the same post at the same moment can both reach
// this point having each posted a mirror; only one row can exist, and the
// loser is told so it can withdraw the copy it made.
func (r *Repository) RecordCommentThread(ctx context.Context, postID, channelID, discussionChatID, discussionMessageID uuid.UUID) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		INSERT INTO channel_post_comments
		    (post_message_id, channel_chat_id, discussion_chat_id, discussion_message_id)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (post_message_id) DO NOTHING`,
		postID, channelID, discussionChatID, discussionMessageID)
	if err != nil {
		return false, fmt.Errorf("groups: record comment thread: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// WithdrawMirror tombstones a mirror that lost the race to be the thread root.
//
// It is a tombstone rather than a delete because the copy has already been
// delivered to whoever had the discussion group open. Removing the row would
// leave those clients holding a message that no history fetch ever mentions
// again; a tombstone is a message they are told to stop showing.
func (r *Repository) WithdrawMirror(ctx context.Context, messageID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE messages
		SET deleted_at = now(), content = '', entities = '[]'::jsonb, payload = '{}'::jsonb
		WHERE id = $1 AND deleted_at IS NULL`, messageID)
	if err != nil {
		return fmt.Errorf("groups: withdraw mirror: %w", err)
	}
	return nil
}

// CountComments recomputes a post's comment count from the replies themselves.
//
// Recounted rather than incremented: a comment can be deleted, and a counter
// that only ever went up would drift away from the thread it claims to
// describe.
func (r *Repository) CountComments(ctx context.Context, postID, chatID, rootMessageID uuid.UUID) (int, error) {
	var count int
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM messages
			WHERE reply_to_id = $1 AND deleted_at IS NULL`, rootMessageID).Scan(&count); err != nil {
			return fmt.Errorf("groups: count comments: %w", err)
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO channel_post_stats (message_id, chat_id, comment_count)
			VALUES ($1, $2, $3)
			ON CONFLICT (message_id) DO UPDATE
			SET comment_count = EXCLUDED.comment_count, updated_at = now()`,
			postID, chatID, count)
		return err
	})
	return count, err
}

// ---------------------------------------------------------------- service

// LinkDiscussion attaches a discussion group to a channel.
//
// The caller has to be entitled to edit both: linking hands the channel's
// posts to the group and hands the group's members the channel's audience, so
// authority over one of the two is not enough.
func (s *Service) LinkDiscussion(ctx context.Context, channelID, groupID, actorID uuid.UUID) error {
	channelCtx, err := s.authorize(ctx, channelID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot change this channel")
	if err != nil {
		return err
	}
	if channelCtx.ChatType != messaging.ChatChannel {
		return httpx.Validation("Only a channel can have a discussion group").
			WithField("chat_id", "must be a channel")
	}

	groupCtx, err := s.authorize(ctx, groupID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot change that group")
	if err != nil {
		return err
	}
	if groupCtx.ChatType != messaging.ChatGroup {
		return httpx.Validation("A discussion group has to be a group").
			WithField("group_chat_id", "must be a group")
	}

	if err := s.repo.LinkDiscussion(ctx, channelID, groupID); err != nil {
		switch {
		case errors.Is(err, ErrNotAChannel), errors.Is(err, ErrNotAGroup):
			return httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		case errors.Is(err, ErrAlreadyLinked):
			return httpx.Conflict(httpx.CodeConflict,
				"One of those chats is already linked to another")
		}
		return httpx.Internal(err)
	}

	s.announce(ctx, channelID, messaging.EventChatUpdated, map[string]any{
		"chat_id": channelID, "discussion_chat_id": groupID,
	})
	s.announce(ctx, groupID, messaging.EventChatUpdated, map[string]any{
		"chat_id": groupID, "linked_channel_id": channelID,
	})
	return nil
}

// UnlinkDiscussion detaches the discussion group from a channel.
func (s *Service) UnlinkDiscussion(ctx context.Context, channelID, actorID uuid.UUID) error {
	if _, err := s.authorize(ctx, channelID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot change this channel"); err != nil {
		return err
	}

	if err := s.repo.UnlinkDiscussion(ctx, channelID); err != nil {
		switch {
		case errors.Is(err, ErrNotAChannel):
			return httpx.Validation("Only a channel can have a discussion group").
				WithField("chat_id", "must be a channel")
		case errors.Is(err, ErrNoDiscussionGroup):
			return nil // Unlinking nothing is not an error.
		}
		return httpx.Internal(err)
	}

	s.announce(ctx, channelID, messaging.EventChatUpdated, map[string]any{
		"chat_id": channelID, "discussion_chat_id": nil,
	})
	return nil
}

// CommentThreadFor resolves — creating it if necessary — where the comments on
// one channel post live.
func (s *Service) CommentThreadFor(ctx context.Context, channelID, postID, viewerID uuid.UUID) (*CommentThread, error) {
	if err := s.canSeeChannel(ctx, channelID, viewerID); err != nil {
		return nil, err
	}

	discussion, err := s.repo.Discussion(ctx, channelID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotAChannel):
			return nil, httpx.Validation("Only a channel has comments").
				WithField("chat_id", "must be a channel")
		case errors.Is(err, ErrNoDiscussionGroup):
			return nil, httpx.NotFound(httpx.CodeNotFound,
				"This channel has no discussion group")
		}
		return nil, httpx.Internal(err)
	}
	if !discussion.CommentsEnabled {
		return nil, httpx.Forbidden(httpx.CodeForbidden, "Comments are switched off here")
	}

	thread, err := s.repo.CommentThread(ctx, postID)
	if err == nil {
		return thread, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, httpx.Internal(err)
	}
	return s.mirrorPost(ctx, discussion, postID)
}

// mirrorPost copies a channel post into the discussion group and claims it as
// that post's comment root.
//
// Normally the mirror is made as the post goes out, by the observer below.
// This path covers the two cases that leaves: a post made before the group was
// linked, and one whose mirror failed at the time.
func (s *Service) mirrorPost(ctx context.Context, discussion *Discussion, postID uuid.UUID) (*CommentThread, error) {
	post, err := s.messaging.MessageByID(ctx, postID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Post not found")
		}
		return nil, httpx.Internal(err)
	}
	if post.ChatID != discussion.ChannelChatID {
		return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Post not found")
	}
	if post.DeletedAt != nil {
		return nil, httpx.NotFound(httpx.CodeMessageNotFound, "Post not found")
	}

	mirror, err := s.messaging.Send(ctx, messaging.SendParams{
		ChatID:          discussion.DiscussionChatID,
		ClientMessageID: uuid.New(),
		Type:            post.Type,
		Content:         post.Content,
		Entities:        post.Entities,
		Payload:         post.Payload,
		// Posted by the chat, not by a person: naming the admin who wrote it
		// would leak the authorship that signature_enabled exists to control.
		AsChat: true,
		Forward: &messaging.ForwardInfo{
			ChatID:    &post.ChatID,
			MessageID: &post.ID,
			Signature: post.AuthorSignature,
		},
	})
	if err != nil {
		return nil, httpx.Internal(err)
	}

	won, err := s.repo.RecordCommentThread(ctx, postID, discussion.ChannelChatID,
		discussion.DiscussionChatID, mirror.Message.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !won {
		// Somebody else's mirror got there first. Withdraw ours rather than
		// leave two copies of the same post in the group.
		if err := s.repo.WithdrawMirror(ctx, mirror.Message.ID); err != nil {
			s.logger.Warn("could not withdraw a duplicate mirror",
				slog.String("message_id", mirror.Message.ID.String()),
				slog.Any("error", err))
		}
		thread, err := s.repo.CommentThread(ctx, postID)
		if err != nil {
			return nil, httpx.Internal(err)
		}
		return thread, nil
	}

	return &CommentThread{
		PostMessageID:    postID,
		DiscussionChatID: discussion.DiscussionChatID,
		RootMessageID:    mirror.Message.ID,
	}, nil
}

// Comments lists the replies under a post, newest first.
func (s *Service) Comments(ctx context.Context, channelID, postID, viewerID uuid.UUID, beforeSeq *int64, limit int) (*CommentThread, []messaging.Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	thread, err := s.CommentThreadFor(ctx, channelID, postID, viewerID)
	if err != nil {
		return nil, nil, err
	}

	comments, err := s.messaging.History(ctx, messaging.HistoryQuery{
		ChatID:    thread.DiscussionChatID,
		ViewerID:  viewerID,
		BeforeSeq: beforeSeq,
		Limit:     limit,
		ReplyToID: &thread.RootMessageID,
	})
	if err != nil {
		return nil, nil, httpx.Internal(err)
	}
	return thread, comments, nil
}

// Comment posts a reply under a channel post.
//
// A reader who is not yet in the discussion group is joined by commenting,
// which is the only way the feature can work: the audience for a channel is
// its subscribers, and requiring them to find and join a second chat first
// would leave the comment box on a post they cannot use.
func (s *Service) Comment(ctx context.Context, channelID, postID, authorID uuid.UUID, in messaging.SendInput) (*messaging.Message, error) {
	if s.messagingService == nil {
		return nil, httpx.Internal(errors.New("groups: the messaging service is not wired up"))
	}

	thread, err := s.CommentThreadFor(ctx, channelID, postID, authorID)
	if err != nil {
		return nil, err
	}

	memberCtx, err := s.messaging.ChatContextFor(ctx, thread.DiscussionChatID, authorID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !memberCtx.IsMember {
		if err := s.repo.AddMember(ctx, thread.DiscussionChatID, authorID,
			messaging.RoleMember, nil); err != nil && !errors.Is(err, ErrAlreadyMember) {
			if errors.Is(err, ErrChatFull) {
				return nil, httpx.Conflict(httpx.CodeConflict,
					"The discussion group is full")
			}
			return nil, httpx.Internal(err)
		}
	}

	in.ChatID = thread.DiscussionChatID
	in.SenderID = authorID
	in.ReplyToID = &thread.RootMessageID
	comment, err := s.messagingService.Send(ctx, in)
	if err != nil {
		return nil, err
	}

	if _, err := s.repo.CountComments(ctx, postID, channelID, thread.RootMessageID); err != nil {
		s.logger.Warn("could not refresh the comment count",
			slog.String("post_id", postID.String()), slog.Any("error", err))
	}
	return comment, nil
}

// canSeeChannel admits members, and anyone at all to a public channel.
func (s *Service) canSeeChannel(ctx context.Context, channelID, viewerID uuid.UUID) error {
	chatCtx, err := s.messaging.ChatContextFor(ctx, channelID, viewerID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return httpx.Internal(err)
	}
	if chatCtx.IsMember {
		return nil
	}

	var isPublic bool
	if err := s.repo.db.Pool.QueryRow(ctx,
		`SELECT is_public FROM chats WHERE id = $1 AND deleted_at IS NULL`,
		channelID).Scan(&isPublic); err != nil {
		return httpx.Internal(err)
	}
	if !isPublic {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	return nil
}

// ObserveMessage mirrors a channel post into its discussion group as it is
// posted, so the group sees the post at the moment it goes out rather than
// when somebody first comments.
//
// It is an observer for the same reason the bot platform is one: mirroring
// must never be able to fail a post. If it does fail, the post stands and the
// first person to open its comments creates the mirror instead.
func (s *Service) ObserveMessage(ctx context.Context, chatCtx *messaging.ChatContext, message *messaging.Message) {
	if chatCtx.ChatType != messaging.ChatChannel || message.DeletedAt != nil {
		return
	}

	discussion, err := s.repo.Discussion(ctx, chatCtx.ChatID)
	if err != nil {
		if !errors.Is(err, ErrNoDiscussionGroup) && !errors.Is(err, ErrNotAChannel) {
			s.logger.Warn("could not read the discussion link",
				slog.String("chat_id", chatCtx.ChatID.String()), slog.Any("error", err))
		}
		return
	}
	if !discussion.CommentsEnabled {
		return
	}

	if _, err := s.mirrorPost(ctx, discussion, message.ID); err != nil {
		s.logger.Warn("could not mirror a channel post into its discussion group",
			slog.String("message_id", message.ID.String()), slog.Any("error", err))
	}
}
