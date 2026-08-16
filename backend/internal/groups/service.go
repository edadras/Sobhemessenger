package groups

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

const (
	maxTitleRunes       = 128
	maxDescriptionRunes = 1024
	defaultMemberPage   = 100
)

// usernamePattern matches the public handles used for links like
// https://sobh.app/joinchat/<username> (§12).
var usernamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{4,31}$`)

// Service enforces who may administer a group or channel.
//
// Every mutation goes through the same gate: load the caller's membership,
// check the permission the action needs, then act. There is no path that
// trusts a role supplied by the client.
type Service struct {
	repo      *Repository
	messaging *messaging.Repository
	bus       *bus.Bus
	cfg       *config.Config
	logger    *slog.Logger
}

func NewService(repo *Repository, messagingRepo *messaging.Repository, messageBus *bus.Bus, cfg *config.Config, logger *slog.Logger) *Service {
	return &Service{repo: repo, messaging: messagingRepo, bus: messageBus, cfg: cfg, logger: logger}
}

// CreateInput describes a new group or channel.
type CreateInput struct {
	OwnerID     uuid.UUID
	Type        string
	Title       string
	Description string
	Username    string
	IsPublic    bool
	MemberIDs   []uuid.UUID
}

// Create builds a group or channel and seeds its initial membership.
func (s *Service) Create(ctx context.Context, in CreateInput) (uuid.UUID, error) {
	if in.Type != messaging.ChatGroup && in.Type != messaging.ChatChannel {
		return uuid.Nil, httpx.Validation("Unsupported chat type").
			WithField("type", "must be group or channel")
	}

	title := strings.TrimSpace(in.Title)
	if title == "" {
		return uuid.Nil, httpx.Validation("A title is required").WithField("title", "required")
	}
	if utf8.RuneCountInString(title) > maxTitleRunes {
		return uuid.Nil, httpx.Validation("Title is too long").
			WithField("title", "exceeds the maximum length")
	}
	if utf8.RuneCountInString(in.Description) > maxDescriptionRunes {
		return uuid.Nil, httpx.Validation("Description is too long").
			WithField("description", "exceeds the maximum length")
	}

	var username *string
	if handle := strings.TrimSpace(in.Username); handle != "" {
		if !usernamePattern.MatchString(handle) {
			return uuid.Nil, httpx.Validation("Username is not valid").
				WithField("username", "5-32 characters, letters, digits and underscore, starting with a letter")
		}
		username = &handle
	}
	// A public chat is only reachable if it has a handle to reach it by.
	if in.IsPublic && username == nil {
		return uuid.Nil, httpx.Validation("A public chat needs a username").
			WithField("username", "required for public chats")
	}

	chatID, err := s.repo.CreateChat(ctx, in.Type, title, in.Description, in.OwnerID, in.IsPublic, username)
	if err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			return uuid.Nil, httpx.Conflict(httpx.CodeUsernameTaken, "That username is already taken")
		}
		return uuid.Nil, httpx.Internal(err)
	}

	// Seeding members is best-effort: the chat exists either way, and a
	// failure here should not roll back its creation.
	for _, memberID := range in.MemberIDs {
		if memberID == in.OwnerID {
			continue
		}
		if err := s.repo.AddMember(ctx, chatID, memberID, messaging.RoleMember, &in.OwnerID); err != nil {
			s.logger.Warn("could not add initial member",
				slog.String("chat_id", chatID.String()),
				slog.String("user_id", memberID.String()),
				slog.Any("error", err))
		}
	}

	return chatID, nil
}

// AddMembers invites users, reporting per-user outcomes rather than failing
// the whole call when one invite is not permitted.
func (s *Service) AddMembers(ctx context.Context, chatID, actorID uuid.UUID, userIDs []uuid.UUID) (map[string]string, error) {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.AddMembers
	}, "You cannot add members to this chat")
	if err != nil {
		return nil, err
	}

	results := make(map[string]string, len(userIDs))
	for _, userID := range userIDs {
		err := s.repo.AddMember(ctx, chatID, userID, messaging.RoleMember, &actorID)
		switch {
		case err == nil:
			results[userID.String()] = "added"
			s.announce(ctx, chatID, messaging.EventChatMemberAdded, map[string]any{
				"chat_id": chatID, "user_id": userID, "added_by": actorID,
			})
		case errors.Is(err, ErrAlreadyMember):
			results[userID.String()] = "already_member"
		case errors.Is(err, ErrChatFull):
			results[userID.String()] = "chat_full"
		default:
			s.logger.Warn("failed to add member", slog.Any("error", err))
			results[userID.String()] = "failed"
		}
	}
	_ = chatCtx
	return results, nil
}

// RemoveMember removes someone else from the chat.
func (s *Service) RemoveMember(ctx context.Context, chatID, actorID, targetID uuid.UUID) error {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.RemoveMembers
	}, "You cannot remove members from this chat")
	if err != nil {
		return err
	}

	// An administrator may not remove someone at or above their own rank.
	targetCtx, err := s.messaging.ChatContextFor(ctx, chatID, targetID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !targetCtx.IsMember {
		return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
	}
	if rank(targetCtx.Role) <= rank(chatCtx.Role) {
		return httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot remove someone with an equal or higher role")
	}

	if err := s.repo.RemoveMember(ctx, chatID, targetID); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
		}
		return httpx.Internal(err)
	}

	s.announce(ctx, chatID, messaging.EventChatMemberLeft, map[string]any{
		"chat_id": chatID, "user_id": targetID, "removed_by": actorID,
	})
	return nil
}

// Leave removes the caller. The owner must transfer ownership first, so a chat
// is never left without an administrator.
func (s *Service) Leave(ctx context.Context, chatID, userID uuid.UUID) error {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if chatCtx.Role == messaging.RoleOwner && chatCtx.MemberCount > 1 {
		return httpx.Conflict(httpx.CodeConflict,
			"Transfer ownership before leaving, or delete the chat")
	}

	if err := s.repo.RemoveMember(ctx, chatID, userID); err != nil {
		return httpx.Internal(err)
	}
	// A chat the last member leaves has no reason to survive.
	if chatCtx.MemberCount <= 1 {
		if err := s.repo.Delete(ctx, chatID); err != nil {
			s.logger.Warn("could not delete empty chat", slog.Any("error", err))
		}
	}

	s.announce(ctx, chatID, messaging.EventChatMemberLeft, map[string]any{
		"chat_id": chatID, "user_id": userID,
	})
	return nil
}

// SetRole promotes or demotes a member.
func (s *Service) SetRole(ctx context.Context, chatID, actorID, targetID uuid.UUID, role string, permissions map[string]bool, customTitle string) error {
	chatCtx, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageAdmins
	}, "You cannot manage administrators in this chat")
	if err != nil {
		return err
	}

	switch role {
	case messaging.RoleAdmin, messaging.RoleModerator, messaging.RoleMember, messaging.RoleRestricted:
	case messaging.RoleOwner:
		return httpx.Validation("Use the ownership transfer endpoint").
			WithField("role", "owner cannot be assigned directly")
	default:
		return httpx.Validation("Unknown role").WithField("role", "unsupported")
	}

	// Nobody may grant a rank they do not themselves hold.
	if rank(role) < rank(chatCtx.Role) {
		return httpx.Forbidden(httpx.CodePermissionDenied,
			"You cannot grant a role higher than your own")
	}

	if err := s.repo.SetRole(ctx, chatID, targetID, role, permissions, customTitle); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
		}
		return httpx.Internal(err)
	}
	return nil
}

// TransferOwnership hands the chat to another member.
func (s *Service) TransferOwnership(ctx context.Context, chatID, actorID, targetID uuid.UUID) error {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember || chatCtx.Role != messaging.RoleOwner {
		return httpx.Forbidden(httpx.CodePermissionDenied, "Only the owner can transfer ownership")
	}

	if err := s.repo.TransferOwnership(ctx, chatID, actorID, targetID); err != nil {
		if errors.Is(err, ErrNotMember) {
			return httpx.NotFound(httpx.CodeNotFound, "That user is not a member")
		}
		return httpx.Internal(err)
	}
	return nil
}

// Members lists the participants a member is allowed to see.
func (s *Service) Members(ctx context.Context, chatID, actorID uuid.UUID, limit, offset int) ([]Member, error) {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	// A channel's subscriber list is private; only its staff may enumerate it.
	if chatCtx.ChatType == messaging.ChatChannel && rank(chatCtx.Role) > rank(messaging.RoleModerator) {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied,
			"Only channel administrators can see the subscriber list")
	}

	if limit <= 0 || limit > 200 {
		limit = defaultMemberPage
	}
	members, err := s.repo.Members(ctx, chatID, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return members, nil
}

// Update changes title, description, photo or username.
func (s *Service) Update(ctx context.Context, chatID, actorID uuid.UUID, title, description, username *string, photoMediaID *uuid.UUID) error {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot edit this chat"); err != nil {
		return err
	}

	if title != nil {
		trimmed := strings.TrimSpace(*title)
		if trimmed == "" || utf8.RuneCountInString(trimmed) > maxTitleRunes {
			return httpx.Validation("Title is not valid").
				WithField("title", "must be between 1 and 128 characters")
		}
		title = &trimmed
	}
	if description != nil && utf8.RuneCountInString(*description) > maxDescriptionRunes {
		return httpx.Validation("Description is too long").
			WithField("description", "exceeds the maximum length")
	}
	if username != nil && *username != "" && !usernamePattern.MatchString(*username) {
		return httpx.Validation("Username is not valid").
			WithField("username", "5-32 characters, letters, digits and underscore, starting with a letter")
	}

	if err := s.repo.UpdateChat(ctx, chatID, title, description, photoMediaID, username); err != nil {
		if errors.Is(err, ErrUsernameTaken) {
			return httpx.Conflict(httpx.CodeUsernameTaken, "That username is already taken")
		}
		return httpx.Internal(err)
	}

	s.announce(ctx, chatID, messaging.EventChatUpdated, map[string]any{"chat_id": chatID})
	return nil
}

func (s *Service) UpdateSettings(ctx context.Context, chatID, actorID uuid.UUID, settings Settings) error {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot change this chat's settings"); err != nil {
		return err
	}

	if settings.SlowModeSeconds < 0 || settings.SlowModeSeconds > 21600 {
		return httpx.Validation("Slow mode is out of range").
			WithField("slow_mode_seconds", "between 0 and 21600")
	}
	if settings.MaxMembers < 2 || settings.MaxMembers > 1000000 {
		return httpx.Validation("Member limit is out of range").
			WithField("max_members", "between 2 and 1000000")
	}

	if err := s.repo.UpdateSettings(ctx, chatID, settings); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Settings(ctx context.Context, chatID, actorID uuid.UUID) (*Settings, error) {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	settings, err := s.repo.Settings(ctx, chatID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Chat settings not found")
		}
		return nil, httpx.Internal(err)
	}
	return settings, nil
}

// Delete removes the chat. Only the owner may do this.
func (s *Service) Delete(ctx context.Context, chatID, actorID uuid.UUID) error {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember || chatCtx.Role != messaging.RoleOwner {
		return httpx.Forbidden(httpx.CodePermissionDenied, "Only the owner can delete this chat")
	}
	if err := s.repo.Delete(ctx, chatID); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// ---------------------------------------------------------------- invites

func (s *Service) CreateInviteLink(ctx context.Context, chatID, actorID uuid.UUID, name string, memberLimit *int, expiresAt *time.Time) (*InviteLink, error) {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageInvites
	}, "You cannot create invite links for this chat"); err != nil {
		return nil, err
	}

	if expiresAt != nil && expiresAt.Before(time.Now()) {
		return nil, httpx.Validation("Expiry is in the past").
			WithField("expires_at", "must be in the future")
	}
	if memberLimit != nil && (*memberLimit < 1 || *memberLimit > 100000) {
		return nil, httpx.Validation("Member limit is out of range").
			WithField("member_limit", "between 1 and 100000")
	}

	link, err := s.repo.CreateInviteLink(ctx, chatID, actorID, name, memberLimit, expiresAt)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	link.URL = s.cfg.PublicBaseURL + "/joinchat/" + link.Slug
	return link, nil
}

func (s *Service) InviteLinks(ctx context.Context, chatID, actorID uuid.UUID) ([]InviteLink, error) {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageInvites
	}, "You cannot see this chat's invite links"); err != nil {
		return nil, err
	}

	links, err := s.repo.InviteLinks(ctx, chatID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	for i := range links {
		links[i].URL = s.cfg.PublicBaseURL + "/joinchat/" + links[i].Slug
	}
	return links, nil
}

func (s *Service) RevokeInviteLink(ctx context.Context, chatID, actorID, linkID uuid.UUID) error {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.ManageInvites
	}, "You cannot revoke this chat's invite links"); err != nil {
		return err
	}

	if err := s.repo.RevokeInviteLink(ctx, chatID, linkID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Invite link not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

// JoinResult tells the client whether it is in, or waiting for approval.
type JoinResult struct {
	ChatID   uuid.UUID `json:"chat_id"`
	Joined   bool      `json:"joined"`
	Pending  bool      `json:"pending"`
	ChatType string    `json:"chat_type"`
	Title    string    `json:"title"`
}

// JoinByInvite redeems an invite slug.
func (s *Service) JoinByInvite(ctx context.Context, slug string, userID uuid.UUID) (*JoinResult, error) {
	invite, err := s.repo.ResolveInvite(ctx, slug)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "This invite link is no longer valid")
		}
		return nil, httpx.Internal(err)
	}

	result := &JoinResult{ChatID: invite.ChatID, ChatType: invite.ChatType, Title: invite.Title}

	if invite.RequiresApproval {
		if err := s.repo.CreateJoinRequest(ctx, invite.ChatID, userID, &invite.LinkID); err != nil {
			return nil, httpx.Internal(err)
		}
		result.Pending = true
		return result, nil
	}

	err = s.repo.AddMember(ctx, invite.ChatID, userID, messaging.RoleMember, nil)
	switch {
	case err == nil:
		result.Joined = true
		if countErr := s.repo.CountInviteUse(ctx, invite.LinkID); countErr != nil {
			s.logger.Warn("could not record invite use", slog.Any("error", countErr))
		}
		s.announce(ctx, invite.ChatID, messaging.EventChatMemberAdded, map[string]any{
			"chat_id": invite.ChatID, "user_id": userID,
		})
	case errors.Is(err, ErrAlreadyMember):
		result.Joined = true
	case errors.Is(err, ErrChatFull):
		return nil, httpx.Forbidden(httpx.CodeChatFull, "This chat is full")
	default:
		return nil, httpx.Internal(err)
	}
	return result, nil
}

// JoinPublic joins a public group or subscribes to a public channel.
func (s *Service) JoinPublic(ctx context.Context, chatID, userID uuid.UUID) (*JoinResult, error) {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if chatCtx.IsMember {
		return &JoinResult{ChatID: chatID, Joined: true, ChatType: chatCtx.ChatType}, nil
	}

	settings, err := s.repo.Settings(ctx, chatID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, httpx.Internal(err)
	}
	if settings != nil && settings.JoinRequiresApproval {
		if err := s.repo.CreateJoinRequest(ctx, chatID, userID, nil); err != nil {
			return nil, httpx.Internal(err)
		}
		return &JoinResult{ChatID: chatID, Pending: true, ChatType: chatCtx.ChatType}, nil
	}

	if err := s.repo.AddMember(ctx, chatID, userID, messaging.RoleMember, nil); err != nil {
		if errors.Is(err, ErrChatFull) {
			return nil, httpx.Forbidden(httpx.CodeChatFull, "This chat is full")
		}
		if !errors.Is(err, ErrAlreadyMember) {
			return nil, httpx.Internal(err)
		}
	}
	return &JoinResult{ChatID: chatID, Joined: true, ChatType: chatCtx.ChatType}, nil
}

func (s *Service) JoinRequests(ctx context.Context, chatID, actorID uuid.UUID) ([]JoinRequest, error) {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.AddMembers
	}, "You cannot review join requests for this chat"); err != nil {
		return nil, err
	}

	requests, err := s.repo.JoinRequests(ctx, chatID, 200)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return requests, nil
}

func (s *Service) ResolveJoinRequest(ctx context.Context, chatID, actorID, targetID uuid.UUID, approve bool) error {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.AddMembers
	}, "You cannot review join requests for this chat"); err != nil {
		return err
	}

	if err := s.repo.ResolveJoinRequest(ctx, chatID, targetID, actorID, approve); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "No pending request from that user")
		}
		if errors.Is(err, ErrChatFull) {
			return httpx.Forbidden(httpx.CodeChatFull, "This chat is full")
		}
		return httpx.Internal(err)
	}
	return nil
}

// RecordView counts a channel post view (§15).
func (s *Service) RecordView(ctx context.Context, chatID, messageID, userID uuid.UUID) error {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, userID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if err := s.repo.RecordPostView(ctx, messageID, chatID, userID); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) PostStats(ctx context.Context, chatID, actorID uuid.UUID, messageIDs []uuid.UUID) ([]PostStats, error) {
	if _, err := s.authorize(ctx, chatID, actorID, func(p messaging.Permissions) bool {
		return p.EditGroup
	}, "You cannot see this channel's statistics"); err != nil {
		return nil, err
	}

	stats, err := s.repo.PostStats(ctx, chatID, messageIDs)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return stats, nil
}

// Discover lists public chats for the directory.
func (s *Service) Discover(ctx context.Context, chatType, query string, limit int) ([]messaging.Chat, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	chats, err := s.repo.Discover(ctx, chatType, strings.TrimSpace(query), limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return chats, nil
}

// authorize is the single gate every administrative action passes through.
func (s *Service) authorize(ctx context.Context, chatID, actorID uuid.UUID, allowed func(messaging.Permissions) bool, denial string) (*messaging.ChatContext, error) {
	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, actorID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}
	if !allowed(chatCtx.Permissions) {
		return nil, httpx.Forbidden(httpx.CodePermissionDenied, denial)
	}
	return chatCtx, nil
}

// announce broadcasts a membership or metadata change to the chat's live
// listeners.
//
// These go out on the chat subject rather than into the per-user event log:
// a membership change is state a client re-reads on its next chat-list fetch,
// so replaying it after a reconnect would add nothing.
func (s *Service) announce(_ context.Context, chatID uuid.UUID, event string, payload map[string]any) {
	if err := s.bus.PublishRealtime(bus.ChatSubject(chatID.String()), map[string]any{
		"event":   event,
		"payload": payload,
	}); err != nil {
		s.logger.Warn("could not broadcast chat event",
			slog.String("chat_id", chatID.String()),
			slog.String("event", event),
			slog.Any("error", err))
	}
}

// rank orders roles so comparisons read naturally: a lower number outranks a
// higher one.
func rank(role string) int {
	switch role {
	case messaging.RoleOwner:
		return 0
	case messaging.RoleAdmin:
		return 1
	case messaging.RoleModerator:
		return 2
	case messaging.RoleMember:
		return 3
	default:
		return 4
	}
}
