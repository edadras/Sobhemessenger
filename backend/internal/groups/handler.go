package groups

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

// Handler exposes /api/v1/groups, /api/v1/channels and the shared chat
// administration endpoints.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

// RegisterRoutes adds the administrative endpoints to the /chats router.
//
// They share that router with the messaging handler rather than mounting a
// second one, because every path here is addressed by chat id and splitting
// them across two prefixes would make the API arbitrary: /chats/{id}/messages
// and /chats/{id}/members belong together.
func (h *Handler) RegisterRoutes(r chi.Router) {
	r.Post("/", h.create)
	r.Get("/discover", h.discover)
	r.Post("/join/{slug}", h.joinByInvite)

	r.Patch("/{chatID}", h.update)
	r.Delete("/{chatID}", h.deleteChat)
	r.Post("/{chatID}/join", h.joinPublic)
	r.Post("/{chatID}/leave", h.leave)

	r.Get("/{chatID}/members", h.members)
	r.Post("/{chatID}/members", h.addMembers)
	r.Delete("/{chatID}/members/{userID}", h.removeMember)
	r.Put("/{chatID}/members/{userID}/role", h.setRole)
	r.Post("/{chatID}/transfer-ownership", h.transferOwnership)

	r.Get("/{chatID}/settings", h.settings)
	r.Put("/{chatID}/settings", h.updateSettings)

	r.Get("/{chatID}/invite-links", h.inviteLinks)
	r.Post("/{chatID}/invite-links", h.createInviteLink)
	r.Delete("/{chatID}/invite-links/{linkID}", h.revokeInviteLink)

	r.Get("/{chatID}/join-requests", h.joinRequests)
	r.Post("/{chatID}/join-requests/{userID}", h.resolveJoinRequest)

	r.Post("/{chatID}/messages/{messageID}/view", h.recordView)
	r.Get("/{chatID}/statistics", h.statistics)

	r.Post("/{chatID}/discussion", h.linkDiscussion)
	r.Delete("/{chatID}/discussion", h.unlinkDiscussion)
	r.Get("/{chatID}/messages/{messageID}/comments", h.comments)
	r.Post("/{chatID}/messages/{messageID}/comments", h.comment)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Type        string      `json:"type"`
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Username    string      `json:"username"`
		IsPublic    bool        `json:"is_public"`
		MemberIDs   []uuid.UUID `json:"member_ids"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	chatID, err := h.service.Create(r.Context(), CreateInput{
		OwnerID:     principal.UserID,
		Type:        body.Type,
		Title:       body.Title,
		Description: body.Description,
		Username:    body.Username,
		IsPublic:    body.IsPublic,
		MemberIDs:   body.MemberIDs,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"chat_id": chatID})
}

func (h *Handler) discover(w http.ResponseWriter, r *http.Request) {
	chats, err := h.service.Discover(r.Context(),
		r.URL.Query().Get("type"),
		r.URL.Query().Get("q"),
		queryInt(r, "limit", 50))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"chats": chats})
}

func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Title        *string    `json:"title,omitempty"`
		Description  *string    `json:"description,omitempty"`
		Username     *string    `json:"username,omitempty"`
		PhotoMediaID *uuid.UUID `json:"photo_media_id,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.Update(r.Context(), chatID, principal.UserID,
		body.Title, body.Description, body.Username, body.PhotoMediaID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) deleteChat(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Delete(r.Context(), chatID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) leave(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Leave(r.Context(), chatID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) members(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	members, err := h.service.Members(r.Context(), chatID, principal.UserID,
		queryInt(r, "limit", 100), queryInt(r, "offset", 0))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"members": members})
}

func (h *Handler) addMembers(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserIDs []uuid.UUID `json:"user_ids"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if len(body.UserIDs) == 0 || len(body.UserIDs) > 200 {
		httpx.Fail(w, r, httpx.Validation("Provide between 1 and 200 users").
			WithField("user_ids", "1-200 entries"))
		return
	}

	results, err := h.service.AddMembers(r.Context(), chatID, principal.UserID, body.UserIDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"results": results})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	targetID, err := pathUUID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.RemoveMember(r.Context(), chatID, principal.UserID, targetID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setRole(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	targetID, err := pathUUID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Role        string          `json:"role"`
		Permissions map[string]bool `json:"permissions,omitempty"`
		CustomTitle string          `json:"custom_title,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetRole(r.Context(), chatID, principal.UserID, targetID,
		body.Role, body.Permissions, body.CustomTitle); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) transferOwnership(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
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

	if err := h.service.TransferOwnership(r.Context(), chatID, principal.UserID, body.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	settings, err := h.service.Settings(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, settings)
}

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body Settings
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UpdateSettings(r.Context(), chatID, principal.UserID, body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) inviteLinks(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	links, err := h.service.InviteLinks(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"links": links})
}

func (h *Handler) createInviteLink(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Name        string     `json:"name,omitempty"`
		MemberLimit *int       `json:"member_limit,omitempty"`
		ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	link, err := h.service.CreateInviteLink(r.Context(), chatID, principal.UserID,
		body.Name, body.MemberLimit, body.ExpiresAt)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, link)
}

func (h *Handler) revokeInviteLink(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	linkID, err := pathUUID(r, "linkID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.RevokeInviteLink(r.Context(), chatID, principal.UserID, linkID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) joinByInvite(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.JoinByInvite(r.Context(), chi.URLParam(r, "slug"), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) joinPublic(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.JoinPublic(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) joinRequests(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	requests, err := h.service.JoinRequests(r.Context(), chatID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"requests": requests})
}

func (h *Handler) resolveJoinRequest(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	targetID, err := pathUUID(r, "userID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Approve bool `json:"approve"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.ResolveJoinRequest(r.Context(), chatID, principal.UserID,
		targetID, body.Approve); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) recordView(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	messageID, err := pathUUID(r, "messageID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.RecordView(r.Context(), chatID, messageID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) statistics(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var messageIDs []uuid.UUID
	for _, raw := range r.URL.Query()["message_id"] {
		id, parseErr := uuid.Parse(raw)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("message_id is not a valid UUID"))
			return
		}
		messageIDs = append(messageIDs, id)
	}

	stats, err := h.service.PostStats(r.Context(), chatID, principal.UserID, messageIDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"statistics": stats})
}

func (h *Handler) chatContext(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	chatID, parseErr := pathUUID(r, "chatID")
	if parseErr != nil {
		return nil, uuid.Nil, parseErr
	}
	return principal, chatID, nil
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
	if err != nil || value < 0 {
		return fallback
	}
	return value
}

// ------------------------------------------------- discussion and comments

func (h *Handler) linkDiscussion(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		GroupChatID uuid.UUID `json:"group_chat_id"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.GroupChatID == uuid.Nil {
		httpx.Fail(w, r, httpx.Validation("A discussion group is required").
			WithField("group_chat_id", "required"))
		return
	}

	if err := h.service.LinkDiscussion(r.Context(), chatID, body.GroupChatID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"channel_chat_id": chatID, "discussion_chat_id": body.GroupChatID,
	})
}

func (h *Handler) unlinkDiscussion(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.UnlinkDiscussion(r.Context(), chatID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) comments(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	messageID, err := pathUUID(r, "messageID")
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var beforeSeq *int64
	if raw := r.URL.Query().Get("before_seq"); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("before_seq must be an integer"))
			return
		}
		beforeSeq = &value
	}

	thread, comments, err := h.service.Comments(r.Context(), chatID, messageID,
		principal.UserID, beforeSeq, queryInt(r, "limit", 50))
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"thread": thread, "comments": comments,
	})
}

func (h *Handler) comment(w http.ResponseWriter, r *http.Request) {
	principal, chatID, err := h.chatContext(r)
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
		ClientMessageID uuid.UUID              `json:"client_message_id"`
		Type            string                 `json:"type"`
		Content         string                 `json:"content"`
		Entities        json.RawMessage        `json:"entities,omitempty"`
		Attachments     []messaging.Attachment `json:"attachments,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.Type == "" {
		body.Type = messaging.TypeText
	}

	comment, err := h.service.Comment(r.Context(), chatID, messageID, principal.UserID,
		messaging.SendInput{
			ClientMessageID: body.ClientMessageID,
			Type:            body.Type,
			Content:         body.Content,
			Entities:        body.Entities,
			Attachments:     body.Attachments,
		})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"message": comment})
}
