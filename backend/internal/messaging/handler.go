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

func (h *Handler) ChatRoutes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.listChats)
	r.Post("/private", h.openPrivateChat)
	r.Get("/{chatID}/messages", h.history)
	r.Post("/{chatID}/messages", h.send)
	r.Post("/{chatID}/read", h.markRead)
	r.Put("/{chatID}/draft", h.setDraft)
	r.Post("/{chatID}/typing", h.typing)
	return r
}

func (h *Handler) MessageRoutes() http.Handler {
	r := chi.NewRouter()
	r.Patch("/{messageID}", h.edit)
	r.Delete("/{messageID}", h.delete)
	r.Post("/{messageID}/reactions", h.react)
	return r
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

	chats, err := h.service.ListChats(r.Context(), principal.UserID, limit, before)
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
