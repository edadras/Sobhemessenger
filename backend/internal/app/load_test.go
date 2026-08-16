package app_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// The latency budgets from §79. They are asserted, not merely reported: a
// change that pushes the send path past them fails the build rather than
// showing up as a slow app.
const (
	apiP95Budget          = 200 * time.Millisecond
	sendAckBudget         = 300 * time.Millisecond
	websocketDeliveBudget = 500 * time.Millisecond
)

// loadTestLimits raises the rate limits a load run would otherwise trip. Only
// the limits in the way are raised: everything else keeps its real value, so
// these tests still exercise the production configuration path.
var loadTestLimits = map[string]string{
	"RL_MESSAGES_PER_MIN":  "100000",
	"RL_API_PER_USER_MIN":  "100000",
	"RL_API_PER_IP_MIN":    "100000",
	"RL_OTP_PER_IP_HOUR":   "100000",
	"RL_LOGIN_PER_IP_HOUR": "100000",
}

// latencies collects samples and answers percentile questions about them.
type latencies struct {
	mu      sync.Mutex
	samples []time.Duration
}

func (l *latencies) record(d time.Duration) {
	l.mu.Lock()
	l.samples = append(l.samples, d)
	l.mu.Unlock()
}

func (l *latencies) percentile(p float64) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), l.samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	// Nearest-rank: the smallest sample at or above the requested rank.
	index := int(float64(len(sorted)-1) * p)
	return sorted[index]
}

func (l *latencies) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.samples)
}

func (l *latencies) report(t *testing.T, name string) {
	t.Helper()
	t.Logf("%s: n=%d p50=%v p95=%v p99=%v max=%v", name, l.count(),
		l.percentile(0.50), l.percentile(0.95), l.percentile(0.99), l.percentile(1.0))
}

// TestSendLatencyMeetsTheBudget drives concurrent senders through the real
// HTTP path and checks the §79 API budget.
//
// This is a floor, not a capacity test: a single process against a local
// database cannot tell you what a cluster does at 100k connections. What it
// can tell you is that the send path has no accidental serialisation in it —
// which is the failure this catches early, and cheaply, on every run.
func TestSendLatencyMeetsTheBudget(t *testing.T) {
	// The rate limiter is correct to reject a burst this size from one user;
	// it has its own tests. Here it is raised out of the way so what is being
	// measured is the send path, not the limiter.
	s := newStack(t, loadTestLimits)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var chat struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	s.post(t, alice.access, "/api/v1/chats/private", map[string]any{"user_id": bob.UserID}, &chat)

	const (
		senders           = 8
		messagesPerSender = 25
	)
	path := "/api/v1/chats/" + chat.ChatID.String() + "/messages"

	var observed latencies
	var wg sync.WaitGroup
	failures := make(chan error, senders*messagesPerSender)

	for sender := 0; sender < senders; sender++ {
		wg.Add(1)
		go func(sender int) {
			defer wg.Done()
			for i := 0; i < messagesPerSender; i++ {
				body := map[string]any{
					"client_message_id": uuid.New(),
					"type":              "text",
					"content":           fmt.Sprintf("load %d-%d", sender, i),
				}
				start := time.Now()
				status := s.do(t, http.MethodPost, alice.access, path, body, nil)
				elapsed := time.Since(start)

				if status != http.StatusCreated {
					failures <- fmt.Errorf("send returned %d", status)
					continue
				}
				observed.record(elapsed)
			}
		}(sender)
	}
	wg.Wait()
	close(failures)

	for err := range failures {
		t.Fatalf("load run failed: %v", err)
	}

	observed.report(t, "message send (HTTP)")
	if p95 := observed.percentile(0.95); p95 > apiP95Budget {
		t.Errorf("send p95 = %v, over the §79 budget of %v", p95, apiP95Budget)
	}

	// Every message must have landed, with contiguous sequences: concurrency
	// must not lose or duplicate one.
	var count, maxSeq int64
	if err := s.db.Pool.QueryRow(t.Context(),
		`SELECT count(*), COALESCE(max(seq), 0) FROM messages WHERE chat_id = $1`,
		chat.ChatID).Scan(&count, &maxSeq); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if want := int64(senders * messagesPerSender); count != want {
		t.Errorf("%d messages stored, want %d", count, want)
	}
	if maxSeq != count {
		t.Errorf("highest sequence is %d for %d messages; the sequence has a gap", maxSeq, count)
	}
}

// TestWebSocketDeliveryMeetsTheBudget measures the interval a user actually
// feels: from the sender's send to the recipient's socket receiving it.
func TestWebSocketDeliveryMeetsTheBudget(t *testing.T) {
	s := newStack(t)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var chat struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	s.post(t, alice.access, "/api/v1/chats/private", map[string]any{"user_id": bob.UserID}, &chat)

	aliceSocket := alice.connect()
	bobSocket := bob.connect()

	const messages = 50
	var ackLatency, deliveryLatency latencies

	// Sent timestamps are keyed by the client message id so the reader can
	// measure delivery without assuming frames arrive in send order.
	var mu sync.Mutex
	sentAt := make(map[string]time.Time, messages)

	received := make(chan struct{})
	go func() {
		defer close(received)
		for i := 0; i < messages; i++ {
			_ = bobSocket.SetReadDeadline(time.Now().Add(10 * time.Second))
			var got frame
			if err := bobSocket.ReadJSON(&got); err != nil {
				return
			}
			if got.Event != "message.new" {
				i--
				continue
			}

			var payload struct {
				Message struct {
					ClientMessageID string `json:"client_message_id"`
				} `json:"message"`
			}
			if err := json.Unmarshal(got.Payload, &payload); err != nil {
				return
			}

			mu.Lock()
			start, ok := sentAt[payload.Message.ClientMessageID]
			mu.Unlock()
			if ok {
				deliveryLatency.record(time.Since(start))
			}
		}
	}()

	for i := 0; i < messages; i++ {
		clientMessageID := uuid.New().String()
		mu.Lock()
		sentAt[clientMessageID] = time.Now()
		mu.Unlock()

		start := time.Now()
		writeFrame(t, aliceSocket, map[string]any{
			"id":    fmt.Sprintf("load-%d", i),
			"event": "message.send",
			"payload": map[string]any{
				"chat_id":           chat.ChatID,
				"client_message_id": clientMessageID,
				"type":              "text",
				"content":           fmt.Sprintf("realtime %d", i),
			},
		})
		awaitFrame(t, aliceSocket, "message.sent", 10*time.Second)
		ackLatency.record(time.Since(start))
	}

	select {
	case <-received:
	case <-time.After(20 * time.Second):
		t.Fatalf("only %d of %d messages were delivered before the timeout",
			deliveryLatency.count(), messages)
	}

	ackLatency.report(t, "send ack (WebSocket)")
	deliveryLatency.report(t, "delivery to recipient (WebSocket)")

	if deliveryLatency.count() != messages {
		t.Errorf("%d of %d messages were delivered", deliveryLatency.count(), messages)
	}
	if p95 := ackLatency.percentile(0.95); p95 > sendAckBudget {
		t.Errorf("send ack p95 = %v, over the §79 budget of %v", p95, sendAckBudget)
	}
	if p95 := deliveryLatency.percentile(0.95); p95 > websocketDeliveBudget {
		t.Errorf("delivery p95 = %v, over the §79 budget of %v", p95, websocketDeliveBudget)
	}
}

// TestManyConcurrentSocketsStayConnected checks that the hub holds a few
// hundred sockets at once and delivers to all of them — the fan-out path a
// busy group exercises.
func TestManyConcurrentSocketsStayConnected(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the concurrent-socket test in short mode")
	}
	s := newStack(t, loadTestLimits)

	const readers = 60
	sender := s.signIn(t)

	type peer struct {
		conn   *websocket.Conn
		chatID uuid.UUID
	}
	peers := make([]peer, 0, readers)

	for i := 0; i < readers; i++ {
		reader := s.signIn(t)
		var chat struct {
			ChatID uuid.UUID `json:"chat_id"`
		}
		s.post(t, sender.access, "/api/v1/chats/private",
			map[string]any{"user_id": reader.UserID}, &chat)
		peers = append(peers, peer{conn: reader.connect(), chatID: chat.ChatID})
	}

	start := time.Now()
	var wg sync.WaitGroup
	delivered := make(chan time.Duration, readers)

	for _, p := range peers {
		wg.Add(1)
		go func(p peer) {
			defer wg.Done()
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				_ = p.conn.SetReadDeadline(deadline)
				var got frame
				if err := p.conn.ReadJSON(&got); err != nil {
					return
				}
				if got.Event == "message.new" {
					delivered <- time.Since(start)
					return
				}
			}
		}(p)
	}

	for _, p := range peers {
		s.post(t, sender.access, "/api/v1/chats/"+p.chatID.String()+"/messages", map[string]any{
			"client_message_id": uuid.New(),
			"type":              "text",
			"content":           "fan out",
		}, nil)
	}

	wg.Wait()
	close(delivered)

	var observed latencies
	for d := range delivered {
		observed.record(d)
	}
	observed.report(t, fmt.Sprintf("delivery across %d sockets", readers))

	if observed.count() != readers {
		t.Errorf("%d of %d sockets received their message", observed.count(), readers)
	}
}
