package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Protocol timings. The read deadline is longer than the ping interval so a
// single dropped pong does not kill an otherwise healthy connection (§8).
const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingInterval   = 25 * time.Second
	maxFrameBytes  = 256 * 1024
	sendBufferSize = 256
)

// Application close codes, above the reserved WebSocket range.
const (
	CloseServerShutdown = 4000
	CloseAuthExpired    = 4001
	CloseReplaced       = 4002
	CloseProtocolError  = 4003
	CloseSlowConsumer   = 4004
	CloseRateLimited    = 4005
)

// ProtocolVersion is the WebSocket contract version. A client that asks for a
// version this node does not speak is rejected at the handshake (§67).
const ProtocolVersion = 1

// Frame is the envelope for everything on the socket, in both directions.
type Frame struct {
	// ID correlates a client request with its ack. Server-initiated events
	// leave it empty.
	ID      string          `json:"id,omitempty"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// SyncSeq is the recipient's event-log position for this event, so a
	// client can advance its cursor without a separate round trip (§9).
	SyncSeq int64       `json:"sync_seq,omitempty"`
	Error   *FrameError `json:"error,omitempty"`
}

type FrameError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Client is one WebSocket connection.
type Client struct {
	hub       *Hub
	conn      *websocket.Conn
	send      chan []byte
	userID    uuid.UUID
	deviceID  uuid.UUID
	sessionID uuid.UUID
	logger    *slog.Logger

	// chatSubscriptions is read by the hub under its own lock and written only
	// while that lock is held, so it needs no separate mutex.
	chatSubscriptions map[uuid.UUID]struct{}

	closeOnce sync.Once
	closed    chan struct{}
}

func newClient(hub *Hub, conn *websocket.Conn, userID, deviceID, sessionID uuid.UUID, logger *slog.Logger) *Client {
	return &Client{
		hub:               hub,
		conn:              conn,
		send:              make(chan []byte, sendBufferSize),
		userID:            userID,
		deviceID:          deviceID,
		sessionID:         sessionID,
		logger:            logger,
		chatSubscriptions: make(map[uuid.UUID]struct{}),
		closed:            make(chan struct{}),
	}
}

// enqueue hands a pre-marshalled frame to the write pump.
//
// A client that cannot keep up is disconnected rather than allowed to grow an
// unbounded buffer: it will reconnect and resume from its cursor, which is
// cheaper than holding memory for a stalled socket.
func (c *Client) enqueue(data []byte) {
	select {
	case c.send <- data:
	case <-c.closed:
	default:
		c.hub.metrics.WSSendErrors.WithLabelValues("slow_consumer").Inc()
		c.logger.Warn("dropping slow WebSocket consumer",
			slog.String("user_id", c.userID.String()),
			slog.String("device_id", c.deviceID.String()))
		c.close(CloseSlowConsumer, "client is not keeping up")
	}
}

// sendFrame marshals and enqueues an event.
func (c *Client) sendFrame(frame Frame) {
	data, err := json.Marshal(frame)
	if err != nil {
		c.logger.Error("failed to marshal frame", slog.Any("error", err))
		return
	}
	c.hub.metrics.WSEventsSent.WithLabelValues(frame.Event).Inc()
	c.enqueue(data)
}

func (c *Client) sendAck(id, event string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		c.logger.Error("failed to marshal ack payload", slog.Any("error", err))
		return
	}
	c.sendFrame(Frame{ID: id, Event: event, Payload: raw})
}

func (c *Client) sendError(id, code, message string) {
	c.sendFrame(Frame{ID: id, Event: "error", Error: &FrameError{Code: code, Message: message}})
}

// close shuts the connection down exactly once.
func (c *Client) close(code int, reason string) {
	c.closeOnce.Do(func() {
		close(c.closed)
		deadline := time.Now().Add(writeWait)
		_ = c.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason), deadline)
		_ = c.conn.Close()
	})
}

// writePump owns all writes to the socket. gorilla/websocket permits only one
// concurrent writer, so every outbound frame funnels through here.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		c.close(websocket.CloseNormalClosure, "")
	}()

	for {
		select {
		case data, ok := <-c.send:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				return
			}
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				c.hub.metrics.WSSendErrors.WithLabelValues("write_failed").Inc()
				return
			}

		case <-ticker.C:
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}

		case <-c.closed:
			return
		}
	}
}

// readPump handles inbound frames until the socket dies.
func (c *Client) readPump(ctx context.Context, handler *frameHandler) {
	defer func() {
		c.hub.unregister(c)
		c.close(websocket.CloseNormalClosure, "")
	}()

	c.conn.SetReadLimit(maxFrameBytes)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err,
				websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.logger.Info("websocket closed unexpectedly",
					slog.String("user_id", c.userID.String()),
					slog.Any("error", err))
			}
			return
		}

		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			c.sendError("", "PROTOCOL_ERROR", "Frame is not valid JSON")
			continue
		}

		c.hub.metrics.WSEventsRecv.WithLabelValues(frame.Event).Inc()

		// Each frame gets its own deadline so one slow handler cannot stall
		// the whole connection.
		frameCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		handler.handle(frameCtx, c, frame)
		cancel()

		select {
		case <-c.closed:
			return
		default:
		}
	}
}
