package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/sobh/messenger/backend/internal/auth"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/presence"
)

// Inbound event names (§8).
const (
	evtPing            = "ping"
	evtPong            = "pong"
	evtMessageSend     = "message.send"
	evtMessageRead     = "message.read"
	evtMessageEdit     = "message.edit"
	evtMessageDelete   = "message.delete"
	evtMessageReact    = "message.react"
	evtTypingStart     = "typing.start"
	evtTypingStop      = "typing.stop"
	evtChatSubscribe   = "chat.subscribe"
	evtChatUnsubscribe = "chat.unsubscribe"
	evtSyncRequest     = "sync.request"
	evtSyncAck         = "sync.ack"
)

// Outbound acknowledgements.
const (
	evtConnected    = "connected"
	evtMessageSent  = "message.sent"
	evtSyncBatch    = "sync.batch"
	evtAcknowledged = "ack"
)

// Handler upgrades HTTP connections and serves the WebSocket protocol.
type Handler struct {
	hub       *Hub
	auth      *auth.Middleware
	authRepo  *auth.Repository
	messaging *messaging.Service
	presence  *presence.Service
	logger    *slog.Logger
	upgrader  websocket.Upgrader
}

func NewHandler(
	hub *Hub,
	authMiddleware *auth.Middleware,
	authRepo *auth.Repository,
	messagingService *messaging.Service,
	presenceService *presence.Service,
	allowedOrigins []string,
	logger *slog.Logger,
) *Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	allowAll := false
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAll = true
		}
		allowed[origin] = struct{}{}
	}

	return &Handler{
		hub:       hub,
		auth:      authMiddleware,
		authRepo:  authRepo,
		messaging: messagingService,
		presence:  presenceService,
		logger:    logger,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Native apps send no Origin; browsers must match the allowlist.
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" || allowAll {
					return true
				}
				_, ok := allowed[origin]
				return ok
			},
			EnableCompression: true,
		},
	}
}

// ServeHTTP performs the handshake. Authentication happens before the upgrade
// so an unauthenticated caller gets a normal HTTP error rather than a socket.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		// Browsers cannot set headers on a WebSocket handshake, so the token
		// may also arrive via the subprotocol header.
		token = websocketProtocolToken(r.Header.Get("Sec-WebSocket-Protocol"))
	}
	if token == "" {
		httpx.Fail(w, r, httpx.Unauthorized(httpx.CodeUnauthorized, "Authentication is required"))
		return
	}

	if version := r.URL.Query().Get("protocol_version"); version != "" {
		parsed, err := strconv.Atoi(version)
		if err != nil || parsed != ProtocolVersion {
			httpx.Fail(w, r, (&httpx.Error{
				Status:  http.StatusBadRequest,
				Code:    httpx.CodeProtocolVersion,
				Message: "This client speaks a protocol version this server does not support",
			}).WithField("protocol_version", "expected "+strconv.Itoa(ProtocolVersion)))
			return
		}
	}

	principal, err := h.auth.AuthenticateToken(r.Context(), token)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response.
		h.logger.Warn("websocket upgrade failed", slog.Any("error", err))
		return
	}

	client := newClient(h.hub, conn, principal.UserID, principal.DeviceID, principal.SessionID, h.logger)
	if err := h.hub.register(client); err != nil {
		h.logger.Error("failed to register websocket client", slog.Any("error", err))
		client.close(websocket.CloseInternalServerErr, "could not register connection")
		return
	}

	ctx := context.WithoutCancel(r.Context())
	go client.writePump()

	h.presence.MarkOnline(ctx, principal.UserID, principal.DeviceID)
	_ = h.authRepo.TouchDevice(ctx, principal.DeviceID, httpx.ClientIPFrom(r.Context()))

	// The connect frame tells the client where the server thinks it stands, so
	// it can decide whether to resync before doing anything else.
	var cursor int64
	if devices, err := h.authRepo.ListDevices(ctx, principal.UserID); err == nil {
		for _, device := range devices {
			if device.ID == principal.DeviceID {
				cursor = device.SyncCursor
				break
			}
		}
	}
	_, latest, err := h.messaging.Sync(ctx, principal.UserID, cursor, 1)
	if err != nil {
		latest = cursor
	}
	client.sendAck("", evtConnected, map[string]any{
		"user_id":          principal.UserID,
		"device_id":        principal.DeviceID,
		"protocol_version": ProtocolVersion,
		"sync_cursor":      cursor,
		"server_seq":       latest,
		"heartbeat_ms":     pingInterval.Milliseconds(),
	})

	handler := &frameHandler{
		messaging: h.messaging,
		presence:  h.presence,
		authRepo:  h.authRepo,
		hub:       h.hub,
		logger:    h.logger,
	}

	client.readPump(ctx, handler)

	disconnectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	h.presence.MarkOffline(disconnectCtx, principal.UserID, principal.DeviceID)
}

// frameHandler dispatches inbound frames to the domain services.
type frameHandler struct {
	messaging *messaging.Service
	presence  *presence.Service
	authRepo  *auth.Repository
	hub       *Hub
	logger    *slog.Logger
}

func (f *frameHandler) handle(ctx context.Context, client *Client, frame Frame) {
	switch frame.Event {
	case evtPing:
		client.sendAck(frame.ID, evtPong, map[string]any{"time": time.Now().UTC()})

	case evtMessageSend:
		f.handleSend(ctx, client, frame)

	case evtMessageRead:
		f.handleRead(ctx, client, frame)

	case evtMessageEdit:
		f.handleEdit(ctx, client, frame)

	case evtMessageDelete:
		f.handleDelete(ctx, client, frame)

	case evtMessageReact:
		f.handleReact(ctx, client, frame)

	case evtTypingStart, evtTypingStop:
		f.handleTyping(ctx, client, frame, frame.Event == evtTypingStart)

	case evtChatSubscribe:
		f.handleChatSubscribe(ctx, client, frame, true)

	case evtChatUnsubscribe:
		f.handleChatSubscribe(ctx, client, frame, false)

	case evtSyncRequest:
		f.handleSyncRequest(ctx, client, frame)

	case evtSyncAck:
		f.handleSyncAck(ctx, client, frame)

	default:
		client.sendError(frame.ID, "UNKNOWN_EVENT", "Unknown event: "+frame.Event)
	}
}

type sendPayload struct {
	ChatID          uuid.UUID              `json:"chat_id"`
	ClientMessageID uuid.UUID              `json:"client_message_id"`
	Type            string                 `json:"type"`
	Content         string                 `json:"content"`
	Entities        json.RawMessage        `json:"entities,omitempty"`
	Payload         json.RawMessage        `json:"payload,omitempty"`
	ReplyToID       *uuid.UUID             `json:"reply_to_id,omitempty"`
	Attachments     []messaging.Attachment `json:"attachments,omitempty"`
	Mentions        []uuid.UUID            `json:"mentions,omitempty"`
	IsSilent        bool                   `json:"is_silent,omitempty"`
}

func (f *frameHandler) handleSend(ctx context.Context, client *Client, frame Frame) {
	var payload sendPayload
	if !decodePayload(client, frame, &payload) {
		return
	}

	message, err := f.messaging.Send(ctx, messaging.SendInput{
		ChatID:          payload.ChatID,
		SenderID:        client.userID,
		ClientMessageID: payload.ClientMessageID,
		Type:            payload.Type,
		Content:         payload.Content,
		Entities:        payload.Entities,
		Payload:         payload.Payload,
		ReplyToID:       payload.ReplyToID,
		Attachments:     payload.Attachments,
		Mentions:        payload.Mentions,
		IsSilent:        payload.IsSilent,
	})
	if err != nil {
		failFrame(client, frame, err)
		return
	}

	// The ack carries the server id and sequence, which is what turns the
	// client's optimistic PENDING row into SENT (§7).
	client.sendAck(frame.ID, evtMessageSent, map[string]any{
		"client_message_id": payload.ClientMessageID,
		"message_id":        message.ID,
		"chat_id":           message.ChatID,
		"seq":               message.Seq,
		"created_at":        message.CreatedAt,
	})
}

func (f *frameHandler) handleRead(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		ChatID uuid.UUID `json:"chat_id"`
		Seq    int64     `json:"seq"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	newSeq, err := f.messaging.MarkRead(ctx, payload.ChatID, client.userID, payload.Seq)
	if err != nil {
		failFrame(client, frame, err)
		return
	}
	client.sendAck(frame.ID, evtAcknowledged, map[string]any{
		"chat_id": payload.ChatID, "last_read_seq": newSeq,
	})
}

func (f *frameHandler) handleEdit(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		MessageID uuid.UUID       `json:"message_id"`
		Content   string          `json:"content"`
		Entities  json.RawMessage `json:"entities,omitempty"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	message, err := f.messaging.Edit(ctx, payload.MessageID, client.userID, payload.Content, payload.Entities)
	if err != nil {
		failFrame(client, frame, err)
		return
	}
	client.sendAck(frame.ID, evtAcknowledged, map[string]any{
		"message_id": message.ID, "edited_at": message.EditedAt,
	})
}

func (f *frameHandler) handleDelete(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		MessageID uuid.UUID `json:"message_id"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	if err := f.messaging.Delete(ctx, payload.MessageID, client.userID); err != nil {
		failFrame(client, frame, err)
		return
	}
	client.sendAck(frame.ID, evtAcknowledged, map[string]any{"message_id": payload.MessageID})
}

func (f *frameHandler) handleReact(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		MessageID uuid.UUID `json:"message_id"`
		Emoji     string    `json:"emoji"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	added, err := f.messaging.React(ctx, payload.MessageID, client.userID, payload.Emoji)
	if err != nil {
		failFrame(client, frame, err)
		return
	}
	client.sendAck(frame.ID, evtAcknowledged, map[string]any{
		"message_id": payload.MessageID, "emoji": payload.Emoji, "added": added,
	})
}

func (f *frameHandler) handleTyping(ctx context.Context, client *Client, frame Frame, typing bool) {
	var payload struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}
	if err := f.messaging.SetTyping(ctx, payload.ChatID, client.userID, typing); err != nil {
		failFrame(client, frame, err)
	}
	// Typing indicators are fire-and-forget; a successful call needs no ack.
}

func (f *frameHandler) handleChatSubscribe(ctx context.Context, client *Client, frame Frame, subscribe bool) {
	var payload struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	if !subscribe {
		f.hub.unsubscribeChat(client, payload.ChatID)
		client.sendAck(frame.ID, evtAcknowledged, map[string]any{"chat_id": payload.ChatID, "subscribed": false})
		return
	}

	// Membership is checked before opening a subscription: without this a
	// client could listen to any chat by id.
	if _, err := f.messaging.History(ctx, payload.ChatID, client.userID, nil, nil, 1); err != nil {
		failFrame(client, frame, err)
		return
	}
	if err := f.hub.subscribeChat(client, payload.ChatID); err != nil {
		client.sendError(frame.ID, "SUBSCRIBE_FAILED", "Could not subscribe to this chat")
		return
	}
	client.sendAck(frame.ID, evtAcknowledged, map[string]any{"chat_id": payload.ChatID, "subscribed": true})
}

func (f *frameHandler) handleSyncRequest(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		Cursor int64 `json:"cursor"`
		Limit  int   `json:"limit"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}

	events, latest, err := f.messaging.Sync(ctx, client.userID, payload.Cursor, payload.Limit)
	if err != nil {
		failFrame(client, frame, err)
		return
	}
	client.sendAck(frame.ID, evtSyncBatch, map[string]any{
		"events":     events,
		"server_seq": latest,
		"has_more":   len(events) > 0 && events[len(events)-1].Seq < latest,
	})
}

func (f *frameHandler) handleSyncAck(ctx context.Context, client *Client, frame Frame) {
	var payload struct {
		Cursor int64 `json:"cursor"`
	}
	if !decodePayload(client, frame, &payload) {
		return
	}
	if err := f.authRepo.UpdateSyncCursor(ctx, client.deviceID, payload.Cursor); err != nil {
		f.logger.Warn("failed to persist sync cursor",
			slog.String("device_id", client.deviceID.String()), slog.Any("error", err))
	}
}

func decodePayload(client *Client, frame Frame, dst any) bool {
	if len(frame.Payload) == 0 {
		client.sendError(frame.ID, "PROTOCOL_ERROR", "Frame is missing its payload")
		return false
	}
	if err := json.Unmarshal(frame.Payload, dst); err != nil {
		client.sendError(frame.ID, "PROTOCOL_ERROR", "Payload could not be decoded")
		return false
	}
	return true
}

// failFrame maps a domain error onto the socket using the same codes the REST
// API returns, so a client has one error vocabulary to handle.
func failFrame(client *Client, frame Frame, err error) {
	apiErr := httpx.AsError(err)
	client.sendError(frame.ID, string(apiErr.Code), apiErr.Message)
}

// websocketProtocolToken extracts a bearer token passed as a subprotocol,
// formatted "sobh.auth.<token>". Browsers cannot set an Authorization header
// on a WebSocket handshake, so this is the browser path.
func websocketProtocolToken(header string) string {
	const prefix = "sobh.auth."
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if token, found := strings.CutPrefix(part, prefix); found && token != "" {
			return token
		}
	}
	return ""
}
