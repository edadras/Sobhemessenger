// Package realtime is the WebSocket layer: connection lifecycle, heartbeats,
// acknowledgement, resume-from-cursor and cross-node routing (§8, §75).
//
// Nodes are interchangeable. A connection registers a NATS subscription for
// its user, so a message published on any node reaches whichever node holds
// that user's socket. Realtime delivery is best-effort by design — the durable
// record is the per-user event log, and a client that misses a frame resumes
// from its cursor.
package realtime

import (
	"log/slog"
	"sync"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/observability"
)

// Hub tracks the sockets held by this node.
type Hub struct {
	mu sync.RWMutex
	// connections is keyed by user, since one user may have several devices
	// connected at once (§10).
	connections map[uuid.UUID]map[*Client]struct{}
	// userSubs holds the NATS subscription backing each connected user, kept
	// alive while at least one of their devices is present.
	userSubs map[uuid.UUID]*nats.Subscription
	// chatSubs are opened on demand for large chats, where there is no
	// per-user fan-out to ride on.
	chatSubs map[uuid.UUID]*chatSubscription
	nodeID   string
	bus      *bus.Bus
	metrics  *observability.Metrics
	logger   *slog.Logger
}

type chatSubscription struct {
	sub      *nats.Subscription
	refCount int
}

func NewHub(nodeID string, messageBus *bus.Bus, metrics *observability.Metrics, logger *slog.Logger) *Hub {
	return &Hub{
		connections: make(map[uuid.UUID]map[*Client]struct{}),
		userSubs:    make(map[uuid.UUID]*nats.Subscription),
		chatSubs:    make(map[uuid.UUID]*chatSubscription),
		nodeID:      nodeID,
		bus:         messageBus,
		metrics:     metrics,
		logger:      logger,
	}
}

// register adds a client and, for the user's first connection on this node,
// opens the NATS subscription that routes their events here.
func (h *Hub) register(client *Client) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients, exists := h.connections[client.userID]
	if !exists {
		clients = make(map[*Client]struct{})
		h.connections[client.userID] = clients
	}
	clients[client] = struct{}{}

	if _, subscribed := h.userSubs[client.userID]; !subscribed {
		userID := client.userID
		sub, err := h.bus.SubscribeRealtimeSubscription(bus.UserSubject(userID.String()), func(data []byte) {
			h.deliverToUser(userID, data)
		})
		if err != nil {
			delete(clients, client)
			if len(clients) == 0 {
				delete(h.connections, client.userID)
			}
			return err
		}
		h.userSubs[client.userID] = sub
	}

	h.metrics.WSConnections.WithLabelValues(h.nodeID).Inc()
	return nil
}

// unregister removes a client and tears down subscriptions that no longer have
// a listener on this node.
func (h *Hub) unregister(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	clients, exists := h.connections[client.userID]
	if !exists {
		return
	}
	if _, present := clients[client]; !present {
		return
	}
	delete(clients, client)
	h.metrics.WSConnections.WithLabelValues(h.nodeID).Dec()

	if len(clients) == 0 {
		delete(h.connections, client.userID)
		if sub, ok := h.userSubs[client.userID]; ok {
			if err := sub.Unsubscribe(); err != nil {
				h.logger.Warn("failed to unsubscribe user subject",
					slog.String("user_id", client.userID.String()), slog.Any("error", err))
			}
			delete(h.userSubs, client.userID)
		}
	}

	for chatID := range client.chatSubscriptions {
		h.releaseChatLocked(chatID)
	}
}

// subscribeChat opens (or refcounts) a chat subject subscription, used when a
// client opens a large chat that has no per-user fan-out.
func (h *Hub) subscribeChat(client *Client, chatID uuid.UUID) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, already := client.chatSubscriptions[chatID]; already {
		return nil
	}

	if existing, ok := h.chatSubs[chatID]; ok {
		existing.refCount++
		client.chatSubscriptions[chatID] = struct{}{}
		return nil
	}

	sub, err := h.bus.SubscribeRealtimeSubscription(bus.ChatSubject(chatID.String()), func(data []byte) {
		h.deliverToChat(chatID, data)
	})
	if err != nil {
		return err
	}
	h.chatSubs[chatID] = &chatSubscription{sub: sub, refCount: 1}
	client.chatSubscriptions[chatID] = struct{}{}
	return nil
}

func (h *Hub) unsubscribeChat(client *Client, chatID uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := client.chatSubscriptions[chatID]; !ok {
		return
	}
	delete(client.chatSubscriptions, chatID)
	h.releaseChatLocked(chatID)
}

// releaseChatLocked drops one reference to a chat subscription. Callers must
// hold h.mu.
func (h *Hub) releaseChatLocked(chatID uuid.UUID) {
	entry, ok := h.chatSubs[chatID]
	if !ok {
		return
	}
	entry.refCount--
	if entry.refCount > 0 {
		return
	}
	if err := entry.sub.Unsubscribe(); err != nil {
		h.logger.Warn("failed to unsubscribe chat subject",
			slog.String("chat_id", chatID.String()), slog.Any("error", err))
	}
	delete(h.chatSubs, chatID)
}

// deliverToUser pushes an event to every socket the user has on this node.
func (h *Hub) deliverToUser(userID uuid.UUID, data []byte) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.connections[userID]))
	for client := range h.connections[userID] {
		clients = append(clients, client)
	}
	h.mu.RUnlock()

	for _, client := range clients {
		client.enqueue(data)
	}
}

// deliverToChat pushes an event to the sockets that have this chat open,
// skipping the sender's own connection so a client never sees its own echo.
func (h *Hub) deliverToChat(chatID uuid.UUID, data []byte) {
	h.mu.RLock()
	var targets []*Client
	for _, clients := range h.connections {
		for client := range clients {
			if _, subscribed := client.chatSubscriptions[chatID]; subscribed {
				targets = append(targets, client)
			}
		}
	}
	h.mu.RUnlock()

	for _, client := range targets {
		client.enqueue(data)
	}
}

// SendToUser pushes an event to a user from outside the WebSocket layer
// (calls, news, moderation), routing through the bus so it reaches whichever
// node holds their socket.
func (h *Hub) SendToUser(userID uuid.UUID, event string, payload any) error {
	return h.bus.PublishRealtime(bus.UserSubject(userID.String()), map[string]any{
		"event":   event,
		"payload": payload,
	})
}

// ConnectionCount reports the sockets held by this node, for /metrics and the
// admin dashboard.
func (h *Hub) ConnectionCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()

	total := 0
	for _, clients := range h.connections {
		total += len(clients)
	}
	return total
}

// IsOnline reports whether a user has a socket on this node. Presence across
// the cluster is tracked in Redis by the presence service.
func (h *Hub) IsOnline(userID uuid.UUID) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.connections[userID]) > 0
}

// Shutdown closes every socket with a going-away frame.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	clients := make([]*Client, 0)
	for _, set := range h.connections {
		for client := range set {
			clients = append(clients, client)
		}
	}
	h.mu.Unlock()

	for _, client := range clients {
		client.close(CloseServerShutdown, "server is shutting down")
	}
}
