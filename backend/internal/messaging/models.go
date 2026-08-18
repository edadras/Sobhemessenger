// Package messaging implements chats and messages: the send/edit/delete/react
// lifecycle, per-chat sequencing, and the per-user event log that multi-device
// sync reads from (§6, §7, §9, §13).
package messaging

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Chat types.
const (
	ChatPrivate = "private"
	ChatGroup   = "group"
	ChatChannel = "channel"
	ChatSecret  = "secret"
)

// Message types (§6).
const (
	TypeText     = "text"
	TypeImage    = "image"
	TypeVideo    = "video"
	TypeAudio    = "audio"
	TypeVoice    = "voice"
	TypeFile     = "file"
	TypeLocation = "location"
	TypeContact  = "contact"
	TypeSticker  = "sticker"
	TypeGIF      = "gif"
	TypePoll     = "poll"
	TypeSystem   = "system"
	TypeCall     = "call"
)

// ValidMessageTypes mirrors the CHECK constraint on messages.type. Validating
// here as well turns a database error into a clean 422.
var ValidMessageTypes = map[string]bool{
	TypeText: true, TypeImage: true, TypeVideo: true, TypeAudio: true,
	TypeVoice: true, TypeFile: true, TypeLocation: true, TypeContact: true,
	TypeSticker: true, TypeGIF: true, TypePoll: true, TypeSystem: true,
	TypeCall: true,
}

// Member roles.
const (
	RoleOwner      = "owner"
	RoleAdmin      = "admin"
	RoleModerator  = "moderator"
	RoleMember     = "member"
	RoleRestricted = "restricted"
)

// Event types written to the per-user event log and pushed over WebSocket (§8).
const (
	EventMessageNew      = "message.new"
	EventMessageEdited   = "message.edited"
	EventMessageDeleted  = "message.deleted"
	EventMessageRead     = "message.read"
	EventMessageReaction = "message.reaction"
	EventChatUpdated     = "chat.updated"
	EventChatMemberAdded = "chat.member_added"
	EventChatMemberLeft  = "chat.member_left"
	EventTypingStart     = "typing.start"
	EventTypingStop      = "typing.stop"
	// EventChatHistoryCleared tells a device to drop everything it holds for
	// a chat below the watermark in the payload. It is not a delete: the chat
	// survives, and so does the other member's copy unless the caller asked
	// for both.
	EventChatHistoryCleared = "chat.history_cleared"
)

// Chat is a conversation container of any type.
type Chat struct {
	ID            uuid.UUID  `json:"id"`
	Type          string     `json:"type"`
	CommunityID   *uuid.UUID `json:"community_id,omitempty"`
	Title         string     `json:"title"`
	Description   string     `json:"description"`
	Username      *string    `json:"username,omitempty"`
	PhotoMediaID  *uuid.UUID `json:"photo_media_id,omitempty"`
	CreatorID     *uuid.UUID `json:"creator_id,omitempty"`
	LastSeq       int64      `json:"last_seq"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	MemberCount   int        `json:"member_count"`
	IsPublic      bool       `json:"is_public"`
	CreatedAt     time.Time  `json:"created_at"`

	// Populated for the requesting member only.
	Membership  *Membership `json:"membership,omitempty"`
	LastMessage *Message    `json:"last_message,omitempty"`

	// Peer is the other person in a one-to-one conversation, private or secret.
	//
	// Such a chat has no title of its own — `chats.title` stays empty — because
	// it is named after whoever is on the other side, and that differs for each
	// of the two members. Resolving it here rather than leaving each client to
	// look the user up means the name is decided once: otherwise every client
	// re-implements "which of these two members is not me", and a chat list
	// that renders nameless rows is the failure mode when one of them gets it
	// wrong.
	//
	// It is also what a client needs to open an encrypted chat, which is
	// addressed to a person rather than to a conversation.
	Peer *ChatPeer `json:"peer,omitempty"`
}

// ChatPeer is the other member of a one-to-one conversation.
//
// Deliberately not a full profile. A chat list needs a name, a picture and an
// id to act on. Fields the viewer's privacy settings govern — last seen, the
// phone number — are not here: they belong to the profile endpoint, which
// resolves those rules per viewer. Copying them into the chat list would be a
// second, unguarded way to read them.
type ChatPeer struct {
	UserID      uuid.UUID  `json:"user_id"`
	DisplayName string     `json:"display_name"`
	Username    *string    `json:"username,omitempty"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	IsBot       bool       `json:"is_bot"`
}

// Membership is the caller's own state in a chat.
type Membership struct {
	Role         string      `json:"role"`
	Permissions  Permissions `json:"permissions"`
	LastReadSeq  int64       `json:"last_read_seq"`
	UnreadCount  int         `json:"unread_count"`
	MentionCount int         `json:"mention_count"`
	IsPinned     bool        `json:"is_pinned"`
	IsArchived   bool        `json:"is_archived"`
	MutedUntil   *time.Time  `json:"muted_until,omitempty"`
	Draft        string      `json:"draft,omitempty"`
	JoinedAt     time.Time   `json:"joined_at"`
}

// Permissions is the effective permission set for a member (§14).
type Permissions struct {
	SendMessages   bool `json:"send_messages"`
	SendMedia      bool `json:"send_media"`
	SendFiles      bool `json:"send_files"`
	SendPolls      bool `json:"send_polls"`
	SendStickers   bool `json:"send_stickers"`
	EmbedLinks     bool `json:"embed_links"`
	AddMembers     bool `json:"add_members"`
	RemoveMembers  bool `json:"remove_members"`
	BanMembers     bool `json:"ban_members"`
	PinMessages    bool `json:"pin_messages"`
	EditGroup      bool `json:"edit_group"`
	DeleteMessages bool `json:"delete_messages"`
	ManageAdmins   bool `json:"manage_admins"`
	ManageCalls    bool `json:"manage_calls"`
	ManageInvites  bool `json:"manage_invites"`
}

// PermissionsForRole returns the defaults a role carries before any per-member
// override is applied.
func PermissionsForRole(role, chatType string) Permissions {
	switch role {
	case RoleOwner:
		return Permissions{
			SendMessages: true, SendMedia: true, SendFiles: true, SendPolls: true,
			SendStickers: true, EmbedLinks: true, AddMembers: true, RemoveMembers: true,
			BanMembers: true, PinMessages: true, EditGroup: true, DeleteMessages: true,
			ManageAdmins: true, ManageCalls: true, ManageInvites: true,
		}
	case RoleAdmin:
		return Permissions{
			SendMessages: true, SendMedia: true, SendFiles: true, SendPolls: true,
			SendStickers: true, EmbedLinks: true, AddMembers: true, RemoveMembers: true,
			BanMembers: true, PinMessages: true, EditGroup: true, DeleteMessages: true,
			ManageCalls: true, ManageInvites: true,
		}
	case RoleModerator:
		return Permissions{
			SendMessages: true, SendMedia: true, SendFiles: true, SendPolls: true,
			SendStickers: true, EmbedLinks: true, RemoveMembers: true,
			PinMessages: true, DeleteMessages: true,
		}
	case RoleRestricted:
		return Permissions{SendMessages: true}
	default:
		// Ordinary members may post everywhere except channels, where posting
		// is an administrative act.
		canPost := chatType != ChatChannel
		// A one-to-one chat has no hierarchy: both people join as members and
		// neither is an owner, so a permission withheld from members is
		// withheld from everyone. Pinning is the case that matters — without
		// this nobody can pin in a private conversation at all, which is not a
		// restriction anyone asked for but the accident of reusing the group
		// role model for a chat that has no roles.
		oneToOne := chatType == ChatPrivate || chatType == ChatSecret
		return Permissions{
			SendMessages: canPost, SendMedia: canPost, SendFiles: canPost,
			SendPolls: canPost, SendStickers: canPost, EmbedLinks: canPost,
			AddMembers:  chatType == ChatGroup,
			PinMessages: oneToOne,
			ManageCalls: oneToOne,
		}
	}
}

// applyOverrides layers a per-member JSON override on top of the role default.
// Only keys present in the override are changed.
func applyOverrides(base Permissions, raw []byte) Permissions {
	if len(raw) == 0 {
		return base
	}
	var overrides map[string]bool
	if err := json.Unmarshal(raw, &overrides); err != nil {
		return base
	}
	targets := map[string]*bool{
		"send_messages": &base.SendMessages, "send_media": &base.SendMedia,
		"send_files": &base.SendFiles, "send_polls": &base.SendPolls,
		"send_stickers": &base.SendStickers, "embed_links": &base.EmbedLinks,
		"add_members": &base.AddMembers, "remove_members": &base.RemoveMembers,
		"ban_members": &base.BanMembers, "pin_messages": &base.PinMessages,
		"edit_group": &base.EditGroup, "delete_messages": &base.DeleteMessages,
		"manage_admins": &base.ManageAdmins, "manage_calls": &base.ManageCalls,
		"manage_invites": &base.ManageInvites,
	}
	for key, value := range overrides {
		if target, ok := targets[key]; ok {
			*target = value
		}
	}
	return base
}

// Message is one entry in a chat's sequence.
type Message struct {
	ID              uuid.UUID       `json:"id"`
	ChatID          uuid.UUID       `json:"chat_id"`
	Seq             int64           `json:"seq"`
	SenderID        *uuid.UUID      `json:"sender_id,omitempty"`
	ClientMessageID uuid.UUID       `json:"client_message_id"`
	Type            string          `json:"type"`
	Content         string          `json:"content"`
	Entities        json.RawMessage `json:"entities,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	ReplyToID       *uuid.UUID      `json:"reply_to_id,omitempty"`
	ForwardFrom     *ForwardInfo    `json:"forward_from,omitempty"`
	Attachments     []Attachment    `json:"attachments,omitempty"`
	Reactions       []ReactionGroup `json:"reactions,omitempty"`
	// ReplyMarkup is the inline keyboard under the message, when a bot put one
	// there. It is its own field rather than part of Payload because Payload is
	// shaped by the message type and a keyboard is orthogonal to all of them.
	ReplyMarkup json.RawMessage `json:"reply_markup,omitempty"`
	IsPinned    bool            `json:"is_pinned"`
	ViewCount   int             `json:"view_count,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
	EditedAt    *time.Time      `json:"edited_at,omitempty"`
	DeletedAt   *time.Time      `json:"deleted_at,omitempty"`
}

type ForwardInfo struct {
	ChatID    *uuid.UUID `json:"chat_id,omitempty"`
	MessageID *uuid.UUID `json:"message_id,omitempty"`
	UserID    *uuid.UUID `json:"user_id,omitempty"`
	Signature string     `json:"signature,omitempty"`
}

type Attachment struct {
	MediaID  uuid.UUID `json:"media_id"`
	Position int       `json:"position"`
	Caption  string    `json:"caption,omitempty"`
}

// ReactionGroup aggregates one emoji across a message.
type ReactionGroup struct {
	Emoji string `json:"emoji"`
	Count int    `json:"count"`
	ByMe  bool   `json:"by_me"`
}

// Event is one entry of the per-user sync log (§9).
type Event struct {
	Seq       int64           `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}
