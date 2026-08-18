package messaging

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
)

// Handler exposes /api/v1/chats and /api/v1/messages.
//
// Every operation here is also available over the WebSocket. REST is the
// fallback path for clients without a live socket, and the two share the same
// service so behaviour cannot drift.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterChatRoutes adds the conversation endpoints to the /chats router,
// which it shares with the group administration handler.
func (h *Handler) RegisterChatRoutes(r chi.Router) {
	r.Get("/", h.listChats)
	r.Post("/private", h.openPrivateChat)
	r.Get("/{chatID}/messages", h.history)
	r.Post("/{chatID}/messages", h.send)
	r.Post("/{chatID}/read", h.markRead)
	r.Put("/{chatID}/draft", h.setDraft)
	r.Post("/{chatID}/typing", h.typing)
	r.Post("/{chatID}/clear-history", h.clearHistory)

	// Folders sit on the chats router because they are a view of the chat
	// list. The literal segment cannot be mistaken for a chat id: chi matches
	// a static path before a parameter.
	r.Get("/folders", h.folders)
	r.Post("/folders", h.createFolder)
	r.Put("/folders/order", h.reorderFolders)
	r.Patch("/folders/{folderID}", h.updateFolder)
	r.Delete("/folders/{folderID}", h.deleteFolder)
	r.Put("/folders/{folderID}/chats/{chatID}", h.setFolderChat)
	r.Delete("/folders/{folderID}/chats/{chatID}", h.removeFolderChat)

	r.Post("/{chatID}/forum", h.enableForum)
	r.Delete("/{chatID}/forum", h.disableForum)
	r.Get("/{chatID}/topics", h.topics)
	r.Post("/{chatID}/topics", h.createTopic)
	r.Patch("/{chatID}/topics/{topicID}", h.updateTopic)
	r.Delete("/{chatID}/topics/{topicID}", h.deleteTopic)
	r.Get("/{chatID}/topics/{topicID}/messages", h.topicHistory)
	r.Post("/{chatID}/topics/{topicID}/read", h.markTopicRead)
}

// RegisterMessageRoutes adds the per-message routes to a shared router, so
// pinning can register alongside them rather than needing a second mount.
func (h *Handler) RegisterMessageRoutes(r chi.Router) {
	r.Patch("/{messageID}", h.edit)
	r.Delete("/{messageID}", h.delete)
	r.Post("/{messageID}/reactions", h.react)
}

func (h *Handler) SyncRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.sync)
	return r
}

func (h *Handler) listChats(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	limit := queryInt(r, "limit", 50)
	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("before must be an RFC3339 timestamp"))
			return
		}
		before = &parsed
	}

	// A folder is a view of the same list, so it is a parameter here rather
	// than a second endpoint that would have to be kept in step with this one.
	var folderID *uuid.UUID
	if raw := r.URL.Query().Get("folder_id"); raw != "" {
		parsed, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("folder_id must be a UUID"))
			return
		}
		folderID = &parsed
	}

	chats, err := h.service.ListChats(r.Context(), principal.UserID, limit, before, folderID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	meta := httpx.Meta{HasMore: len(chats) == limit}
	if len(chats) > 0 {
		if last := chats[len(chats)-1].LastMessageAt; last != nil {
			meta.NextCursor = last.Format(time.RFC3339Nano)
		}
	}
	httpx.JSONWithMeta(w, r, http.StatusOK, map[string]any{"chats": chats}, meta)
}

func (h *Handler) openPrivateChat(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserID uuid.UUID `json:"user_id"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.UserID == uuid.Nil {
		httpx.Fail(w, r, httpx.Validation("user_id is required").WithField("user_id", "required"))
		return
	}

	chatID, err := h.service.OpenPrivateChat(r.Context(), principal.UserID, body.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"chat_id": chatID})
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
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

	var beforeSeq, afterSeq *int64
	if raw := r.URL.Query().Get("before_seq"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("before_seq must be an integer"))
			return
		}
		beforeSeq = &value
	}
	if raw := r.URL.Query().Get("after_seq"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("after_seq must be an integer"))
			return
		}
		afterSeq = &value
	}

	limit := queryInt(r, "limit", 50)
	messages, err := h.service.History(r.Context(), chatID, principal.UserID, beforeSeq, afterSeq, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	meta := httpx.Meta{HasMore: len(messages) == limit}
	if len(messages) > 0 {
		meta.NextCursor = strconv.FormatInt(messages[len(messages)-1].Seq, 10)
	}
	httpx.JSONWithMeta(w, r, http.StatusOK, map[string]any{"messages": messages}, meta)
}

func (h *Handler) clearHistory(w http.ResponseWriter, r *http.Request) {
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

	// An empty body is the common case — clear it for me — so it is allowed
	// rather than rejected as malformed.
	var body struct {
		ForEveryone bool `json:"for_everyone,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	watermark, err := h.service.ClearHistory(r.Context(), chatID, principal.UserID, body.ForEveryone)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"chat_id": chatID, "cleared_upto_seq": watermark, "for_everyone": body.ForEveryone,
	})
}

type sendBody struct {
	ClientMessageID uuid.UUID       `json:"client_message_id"`
	Type            string          `json:"type"`
	Content         string          `json:"content"`
	Entities        json.RawMessage `json:"entities,omitempty"`
	Payload         json.RawMessage `json:"payload,omitempty"`
	ReplyToID       *uuid.UUID      `json:"reply_to_id,omitempty"`
	Attachments     []Attachment    `json:"attachments,omitempty"`
	Mentions        []uuid.UUID     `json:"mentions,omitempty"`
	IsSilent        bool            `json:"is_silent,omitempty"`
	// TopicID files the message under a forum topic. Absent in a forum means
	// General; absent anywhere else means nothing.
	TopicID *uuid.UUID `json:"topic_id,omitempty"`
}

func (h *Handler) send(w http.ResponseWriter, r *http.Request) {
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

	var body sendBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Type == "" {
		body.Type = TypeText
	}

	message, err := h.service.Send(r.Context(), SendInput{
		ChatID:          chatID,
		SenderID:        principal.UserID,
		ClientMessageID: body.ClientMessageID,
		Type:            body.Type,
		Content:         body.Content,
		Entities:        body.Entities,
		Payload:         body.Payload,
		ReplyToID:       body.ReplyToID,
		Attachments:     body.Attachments,
		Mentions:        body.Mentions,
		IsSilent:        body.IsSilent,
		TopicID:         body.TopicID,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, message)
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
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
		Seq int64 `json:"seq"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	newSeq, err := h.service.MarkRead(r.Context(), chatID, principal.UserID, body.Seq)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"last_read_seq": newSeq})
}

func (h *Handler) setDraft(w http.ResponseWriter, r *http.Request) {
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
		Draft string `json:"draft"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.SetDraft(r.Context(), chatID, principal.UserID, body.Draft); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) typing(w http.ResponseWriter, r *http.Request) {
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
		Typing bool `json:"typing"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.SetTyping(r.Context(), chatID, principal.UserID, body.Typing); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) edit(w http.ResponseWriter, r *http.Request) {
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
		Content  string          `json:"content"`
		Entities json.RawMessage `json:"entities,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	message, err := h.service.Edit(r.Context(), messageID, principal.UserID, body.Content, body.Entities)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, message)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
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
	if err := h.service.Delete(r.Context(), messageID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) react(w http.ResponseWriter, r *http.Request) {
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
		Emoji string `json:"emoji"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	added, err := h.service.React(r.Context(), messageID, principal.UserID, body.Emoji)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"emoji": body.Emoji, "added": added})
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	cursor := int64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		parsed, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("cursor must be an integer"))
			return
		}
		cursor = parsed
	}

	events, latest, err := h.service.Sync(r.Context(), principal.UserID, cursor, queryInt(r, "limit", 200))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"events":     events,
		"server_seq": latest,
	})
}

func pathUUID(r *http.Request, name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		return uuid.Nil, httpx.BadRequest(name + " is not a valid UUID")
	}
	return id, nil
}

func queryInt(r *http.Request, name string, fallback int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

// ---------------------------------------------------------------- folders

// folderBody is the whole folder, because that is how it is edited: one small
// object on one screen. A partial update of a filter is harder to reason about
// than replacing it.
type folderBody struct {
	Title              string `json:"title"`
	Emoji              string `json:"emoji,omitempty"`
	Position           int    `json:"position,omitempty"`
	IncludeContacts    bool   `json:"include_contacts,omitempty"`
	IncludeNonContacts bool   `json:"include_non_contacts,omitempty"`
	IncludeGroups      bool   `json:"include_groups,omitempty"`
	IncludeChannels    bool   `json:"include_channels,omitempty"`
	IncludeBots        bool   `json:"include_bots,omitempty"`
	ExcludeMuted       bool   `json:"exclude_muted,omitempty"`
	ExcludeRead        bool   `json:"exclude_read,omitempty"`
	ExcludeArchived    bool   `json:"exclude_archived,omitempty"`
}

func (b folderBody) input() FolderInput {
	return FolderInput{
		Title: b.Title, Emoji: b.Emoji, Position: b.Position,
		IncludeContacts: b.IncludeContacts, IncludeNonContacts: b.IncludeNonContacts,
		IncludeGroups: b.IncludeGroups, IncludeChannels: b.IncludeChannels,
		IncludeBots: b.IncludeBots, ExcludeMuted: b.ExcludeMuted,
		ExcludeRead: b.ExcludeRead, ExcludeArchived: b.ExcludeArchived,
	}
}

func (h *Handler) folders(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	folders, err := h.service.Folders(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"folders": folders})
}

func (h *Handler) createFolder(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body folderBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	folderID, err := h.service.CreateFolder(r.Context(), principal.UserID, body.input())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"folder_id": folderID})
}

func (h *Handler) updateFolder(w http.ResponseWriter, r *http.Request) {
	principal, folderID, err := h.folderContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body folderBody
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UpdateFolder(r.Context(), principal.UserID, folderID, body.input()); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) deleteFolder(w http.ResponseWriter, r *http.Request) {
	principal, folderID, err := h.folderContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.DeleteFolder(r.Context(), principal.UserID, folderID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setFolderChat(w http.ResponseWriter, r *http.Request) {
	principal, folderID, err := h.folderContext(r)
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
		Mode string `json:"mode"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Mode == "" {
		body.Mode = "include"
	}

	if err := h.service.SetFolderChat(r.Context(), principal.UserID, folderID, chatID, body.Mode); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) removeFolderChat(w http.ResponseWriter, r *http.Request) {
	principal, folderID, err := h.folderContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.RemoveFolderChat(r.Context(), principal.UserID, folderID, chatID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) reorderFolders(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Order []uuid.UUID `json:"order"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.ReorderFolders(r.Context(), principal.UserID, body.Order); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) folderContext(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	folderID, err := pathUUID(r, "folderID")
	if err != nil {
		return nil, uuid.Nil, err
	}
	return principal, folderID, nil
}

// ----------------------------------------------------------- forum topics

func (h *Handler) enableForum(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	topic, err := h.service.EnableForum(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"general_topic": topic})
}

func (h *Handler) disableForum(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.DisableForum(r.Context(), chatID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) topics(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	topics, err := h.service.Topics(r.Context(), chatID, principal.UserID, queryInt(r, "limit", 100))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"topics": topics})
}

func (h *Handler) createTopic(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Title     string `json:"title"`
		IconEmoji string `json:"icon_emoji,omitempty"`
		IconColor int    `json:"icon_color,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	topic, err := h.service.CreateTopic(r.Context(), chatID, principal.UserID,
		body.Title, body.IconEmoji, body.IconColor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"topic": topic})
}

func (h *Handler) updateTopic(w http.ResponseWriter, r *http.Request) {
	principal, chatID, topicID, err := h.topicFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Pointers throughout: a topic is edited a field at a time, and a whole
	// object would let a rename silently reopen a topic somebody had closed.
	var body struct {
		Title     *string `json:"title,omitempty"`
		IconEmoji *string `json:"icon_emoji,omitempty"`
		IconColor *int    `json:"icon_color,omitempty"`
		IsClosed  *bool   `json:"is_closed,omitempty"`
		IsHidden  *bool   `json:"is_hidden,omitempty"`
		IsPinned  *bool   `json:"is_pinned,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	topic, err := h.service.UpdateTopic(r.Context(), chatID, topicID, principal.UserID, TopicUpdate{
		Title: body.Title, IconEmoji: body.IconEmoji, IconColor: body.IconColor,
		IsClosed: body.IsClosed, IsHidden: body.IsHidden, IsPinned: body.IsPinned,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"topic": topic})
}

func (h *Handler) deleteTopic(w http.ResponseWriter, r *http.Request) {
	principal, chatID, topicID, err := h.topicFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.DeleteTopic(r.Context(), chatID, topicID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) topicHistory(w http.ResponseWriter, r *http.Request) {
	principal, chatID, topicID, err := h.topicFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var beforeSeq, afterSeq *int64
	if raw := r.URL.Query().Get("before_seq"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("before_seq must be an integer"))
			return
		}
		beforeSeq = &value
	}
	if raw := r.URL.Query().Get("after_seq"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("after_seq must be an integer"))
			return
		}
		afterSeq = &value
	}

	limit := queryInt(r, "limit", 50)
	messages, err := h.service.TopicHistory(r.Context(), chatID, topicID, principal.UserID,
		beforeSeq, afterSeq, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	meta := httpx.Meta{HasMore: len(messages) == limit}
	if len(messages) > 0 {
		meta.NextCursor = strconv.FormatInt(messages[len(messages)-1].Seq, 10)
	}
	httpx.JSONWithMeta(w, r, http.StatusOK, map[string]any{"messages": messages}, meta)
}

func (h *Handler) markTopicRead(w http.ResponseWriter, r *http.Request) {
	principal, chatID, topicID, err := h.topicFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Seq int64 `json:"seq"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	newSeq, err := h.service.MarkTopicRead(r.Context(), chatID, topicID, principal.UserID, body.Seq)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"topic_id": topicID, "last_read_seq": newSeq,
	})
}

func (h *Handler) chatFrom(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	chatID, err := pathUUID(r, "chatID")
	if err != nil {
		return nil, uuid.Nil, err
	}
	return principal, chatID, nil
}

func (h *Handler) topicFrom(r *http.Request) (*httpx.Principal, uuid.UUID, uuid.UUID, error) {
	principal, chatID, err := h.chatFrom(r)
	if err != nil {
		return nil, uuid.Nil, uuid.Nil, err
	}
	topicID, err := pathUUID(r, "topicID")
	if err != nil {
		return nil, uuid.Nil, uuid.Nil, err
	}
	return principal, chatID, topicID, nil
}
