// Package app_test brings the whole stack up and drives it the way a client
// does: over HTTP and a WebSocket, against a real PostgreSQL, a real Redis
// protocol implementation and a real NATS server with JetStream (§21).
//
// Nothing here stubs a module. The router under test is the one Assemble
// builds for production, so a wiring mistake — a route mounted twice, a
// middleware in the wrong order, a handler that never reaches its service —
// fails these tests rather than reaching a release.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/sobh/messenger/backend/internal/app"
	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/contacts"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/observability"
)

// stack is a running SOBH instance plus the clients needed to talk to it.
type stack struct {
	t      *testing.T
	app    *app.App
	server *httptest.Server
	db     *database.DB
}

// newStack brings up a full instance. Any environment overrides are applied
// before configuration is loaded, which is how the load tests widen the rate
// limits they are not trying to measure.
func newStack(t *testing.T, overrides ...map[string]string) *stack {
	t.Helper()

	dsn := os.Getenv("SOBH_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("SOBH_TEST_POSTGRES_DSN is not set; skipping end-to-end test")
	}

	ctx := context.Background()

	// Server logs are discarded by default so a passing run is quiet; set
	// SOBH_TEST_LOG=1 to see why a request failed.
	logOutput := io.Discard
	if os.Getenv("SOBH_TEST_LOG") != "" {
		logOutput = os.Stderr
	}
	logger := slog.New(slog.NewTextHandler(logOutput, &slog.HandlerOptions{Level: slog.LevelDebug}))

	db, err := database.Connect(ctx, config.Postgres{
		DSN: dsn, MaxConns: 16, MinConns: 2,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, StatementCache: true,
	})
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	t.Cleanup(db.Close)

	redis := miniredis.RunT(t)
	cacheClient, err := cache.Connect(ctx, config.Redis{Addr: redis.Addr(), PoolSize: 8})
	if err != nil {
		t.Fatalf("connect to test redis: %v", err)
	}
	t.Cleanup(func() { _ = cacheClient.Close() })

	natsURL := startNATS(t)
	cfg := testConfig(t, dsn, redis.Addr(), natsURL, overrides...)

	messageBus, err := bus.Connect(cfg.NATS, logger, observability.New(cfg.ServiceName))
	if err != nil {
		t.Fatalf("connect to test nats: %v", err)
	}
	t.Cleanup(messageBus.Close)

	// Storage is left nil: object storage is not part of what these tests
	// exercise, and Assemble is built to run without it.
	instance, err := app.Assemble(ctx, cfg, logger, app.Dependencies{
		DB: db, Cache: cacheClient, Bus: messageBus,
	})
	if err != nil {
		t.Fatalf("assemble application: %v", err)
	}
	t.Cleanup(instance.Hub.Shutdown)

	server := httptest.NewServer(instance.Handler())
	t.Cleanup(server.Close)

	return &stack{t: t, app: instance, server: server, db: db}
}

// startNATS runs an in-process NATS server with JetStream on a free port.
func startNATS(t *testing.T) string {
	t.Helper()

	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // any free port
		JetStream: true,
		StoreDir:  t.TempDir(),
		NoLog:     true,
		NoSigs:    true,
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("create nats server: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server did not become ready")
	}
	t.Cleanup(srv.Shutdown)
	return srv.ClientURL()
}

// testConfig builds the configuration the way the process does — through the
// environment and config.Load — so the test cannot accidentally construct a
// Config that Load itself would reject.
func testConfig(t *testing.T, dsn, redisAddr, natsURL string, overrides ...map[string]string) *config.Config {
	t.Helper()

	t.Setenv("SOBH_ENV", "development")
	t.Setenv("NODE_ID", "e2e-"+uuid.NewString()[:8])
	t.Setenv("POSTGRES_DSN", dsn)
	t.Setenv("REDIS_ADDR", redisAddr)
	t.Setenv("NATS_URL", natsURL)
	// Each run gets its own stream, so a leftover one from a previous run
	// cannot change what a consumer sees.
	t.Setenv("NATS_STREAM", "SOBH_E2E_"+strings.ToUpper(uuid.NewString()[:8]))
	t.Setenv("JWT_SIGNING_KEYS", "e2e:end-to-end-signing-key-long-enough-for-hs256")
	t.Setenv("JWT_ACTIVE_KEY_ID", "e2e")
	t.Setenv("PHONE_HASH_PEPPER", "end-to-end-test-pepper-value-32-bytes!")
	t.Setenv("SMS_PROVIDER", "log")
	// The code comes back in the response so the test can complete a real
	// sign-in. Production configuration validation refuses this setting.
	t.Setenv("SMS_ECHO_CODES", "true")
	// There is no OpenSearch here; search degrades to empty results, which is
	// the documented behaviour when the cluster is unreachable.
	t.Setenv("OPENSEARCH_ENABLED", "false")

	for _, override := range overrides {
		for key, value := range override {
			t.Setenv(key, value)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load test configuration: %v", err)
	}
	return cfg
}

// ---------------------------------------------------------------- client

// client is one signed-in device.
type client struct {
	t        *testing.T
	stack    *stack
	access   string
	refresh  string
	UserID   uuid.UUID
	DeviceID uuid.UUID
	phone    string
}

// signIn performs the real OTP exchange over HTTP.
func (s *stack) signIn(t *testing.T) *client {
	t.Helper()

	phone := "+9891" + fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)
	c := &client{t: t, stack: s, phone: phone}

	var request struct {
		DebugCode string `json:"debug_code"`
	}
	s.post(t, "", "/api/v1/auth/otp/request", map[string]any{"phone": phone}, &request)
	if request.DebugCode == "" {
		t.Fatal("the OTP was not echoed; the test configuration is wrong")
	}

	var pair struct {
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token"`
		UserID       uuid.UUID `json:"user_id"`
		DeviceID     uuid.UUID `json:"device_id"`
		IsNewAccount bool      `json:"is_new_account"`
	}
	s.post(t, "", "/api/v1/auth/otp/verify", map[string]any{
		"phone": phone, "code": request.DebugCode,
		"device_name": "e2e", "platform": "android", "app_version": "1.0.0",
	}, &pair)

	if pair.AccessToken == "" || pair.UserID == uuid.Nil {
		t.Fatal("sign-in returned no usable credentials")
	}
	if !pair.IsNewAccount {
		t.Error("a first sign-in was not reported as a new account")
	}

	c.access, c.refresh = pair.AccessToken, pair.RefreshToken
	c.UserID, c.DeviceID = pair.UserID, pair.DeviceID

	t.Cleanup(func() {
		_, _ = s.db.Pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, pair.UserID)
	})
	return c
}

// envelope is the single response shape every endpoint uses.
type envelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (s *stack) do(t *testing.T, method, token, path string, body, out any) int {
	t.Helper()

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, s.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("%s %s: decode envelope: %v", method, path, err)
	}
	if out != nil {
		if !env.Success {
			t.Fatalf("%s %s failed with %d: %v", method, path, resp.StatusCode, env.Error)
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			t.Fatalf("%s %s: decode data: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

func (s *stack) post(t *testing.T, token, path string, body, out any) {
	t.Helper()
	s.do(t, http.MethodPost, token, path, body, out)
}

func (s *stack) get(t *testing.T, token, path string, out any) {
	t.Helper()
	s.do(t, http.MethodGet, token, path, nil, out)
}

// frame is the WebSocket envelope from protocol v1.
type frame struct {
	ID      string          `json:"id,omitempty"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload,omitempty"`
	SyncSeq int64           `json:"sync_seq,omitempty"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// connect opens an authenticated WebSocket and waits for the "connected" frame.
func (c *client) connect() *websocket.Conn {
	c.t.Helper()

	wsURL := "ws" + strings.TrimPrefix(c.stack.server.URL, "http") +
		"/ws?token=" + url.QueryEscape(c.access) + "&protocol_version=1"

	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		c.t.Fatalf("websocket handshake failed (status %d): %v", status, err)
	}
	c.t.Cleanup(func() { _ = conn.Close() })

	if got := readFrame(c.t, conn, 5*time.Second); got.Event != "connected" {
		c.t.Fatalf("first frame was %q, want %q", got.Event, "connected")
	}
	return conn
}

func readFrame(t *testing.T, conn *websocket.Conn, timeout time.Duration) frame {
	t.Helper()

	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var got frame
	if err := conn.ReadJSON(&got); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return got
}

// awaitFrame reads until an expected event arrives, ignoring unrelated ones.
func awaitFrame(t *testing.T, conn *websocket.Conn, event string, timeout time.Duration) frame {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		var got frame
		if err := conn.ReadJSON(&got); err != nil {
			t.Fatalf("waiting for %q: %v", event, err)
		}
		if got.Event == event {
			return got
		}
	}
	t.Fatalf("timed out waiting for %q", event)
	return frame{}
}

func writeFrame(t *testing.T, conn *websocket.Conn, f map[string]any) {
	t.Helper()
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := conn.WriteJSON(f); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

// ---------------------------------------------------------------- tests

func TestProbesReportReadyWithoutObjectStorage(t *testing.T) {
	s := newStack(t)

	var live struct {
		Status string `json:"status"`
	}
	s.get(t, "", "/health", &live)
	if live.Status != "ok" {
		t.Errorf("/health status = %q, want %q", live.Status, "ok")
	}

	var ready struct {
		Ready        bool `json:"ready"`
		Dependencies []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"dependencies"`
	}
	s.get(t, "", "/ready", &ready)
	if !ready.Ready {
		t.Errorf("/ready reported not ready: %+v", ready.Dependencies)
	}

	statuses := map[string]string{}
	for _, dep := range ready.Dependencies {
		statuses[dep.Name] = dep.Status
	}
	for _, name := range []string{"postgres", "redis", "nats"} {
		if statuses[name] != "ok" {
			t.Errorf("dependency %s = %q, want ok", name, statuses[name])
		}
	}
	// Object storage was deliberately not configured; that is not an outage.
	if statuses["minio"] != "not_configured" {
		t.Errorf("minio = %q, want not_configured", statuses["minio"])
	}
}

func TestUnauthenticatedRequestsAreRejected(t *testing.T) {
	s := newStack(t)

	for _, path := range []string{"/api/v1/chats", "/api/v1/sync", "/api/v1/contacts"} {
		if status := s.do(t, http.MethodGet, "", path, nil, nil); status != http.StatusUnauthorized {
			t.Errorf("GET %s without a token = %d, want 401", path, status)
		}
	}
}

// The whole point of the stack: two people sign in, one messages the other,
// and it arrives over the socket.
func TestSignInThenMessageDeliveredOverWebSocket(t *testing.T) {
	s := newStack(t)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var chat struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	s.post(t, alice.access, "/api/v1/chats/private", map[string]any{"user_id": bob.UserID}, &chat)
	if chat.ChatID == uuid.Nil {
		t.Fatal("opening a private chat returned no chat id")
	}

	bobSocket := bob.connect()
	aliceSocket := alice.connect()

	clientMessageID := uuid.New()
	writeFrame(t, aliceSocket, map[string]any{
		"id":    "send-1",
		"event": "message.send",
		"payload": map[string]any{
			"chat_id":           chat.ChatID,
			"client_message_id": clientMessageID,
			"type":              "text",
			"content":           "سلام بابک",
		},
	})

	// The sender is acknowledged...
	ack := awaitFrame(t, aliceSocket, "message.sent", 5*time.Second)
	if ack.ID != "send-1" {
		t.Errorf("ack correlated to %q, want %q", ack.ID, "send-1")
	}

	// ...and the recipient receives the message itself.
	delivered := awaitFrame(t, bobSocket, "message.new", 5*time.Second)
	var payload struct {
		Message struct {
			ID      uuid.UUID `json:"id"`
			ChatID  uuid.UUID `json:"chat_id"`
			Content string    `json:"content"`
			Seq     int64     `json:"seq"`
		} `json:"message"`
	}
	if err := json.Unmarshal(delivered.Payload, &payload); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	if payload.Message.Content != "سلام بابک" {
		t.Errorf("delivered content = %q, want the Persian text that was sent", payload.Message.Content)
	}
	if payload.Message.ChatID != chat.ChatID {
		t.Errorf("delivered chat = %s, want %s", payload.Message.ChatID, chat.ChatID)
	}
	if payload.Message.Seq != 1 {
		t.Errorf("first message seq = %d, want 1", payload.Message.Seq)
	}
}

// A client that was offline must be able to catch up from its cursor alone.
func TestSyncCatchesUpAClientThatWasOffline(t *testing.T) {
	s := newStack(t)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var chat struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	s.post(t, alice.access, "/api/v1/chats/private", map[string]any{"user_id": bob.UserID}, &chat)

	// Bob never connects. Alice sends while he is away.
	const sent = 3
	for i := 0; i < sent; i++ {
		var message struct {
			Seq int64 `json:"seq"`
		}
		s.post(t, alice.access, "/api/v1/chats/"+chat.ChatID.String()+"/messages", map[string]any{
			"client_message_id": uuid.New(),
			"type":              "text",
			"content":           fmt.Sprintf("while you were out %d", i),
		}, &message)
		if message.Seq != int64(i+1) {
			t.Errorf("message %d got seq %d, want %d", i, message.Seq, i+1)
		}
	}

	var syncResult struct {
		Events []struct {
			Seq  int64  `json:"seq"`
			Type string `json:"type"`
		} `json:"events"`
		ServerSeq int64 `json:"server_seq"`
	}
	s.get(t, bob.access, "/api/v1/sync?cursor=0", &syncResult)

	if len(syncResult.Events) != sent {
		t.Fatalf("bob synced %d events, want %d", len(syncResult.Events), sent)
	}
	for i, event := range syncResult.Events {
		if event.Type != "message.new" {
			t.Errorf("event %d type = %q, want message.new", i, event.Type)
		}
	}
	if syncResult.ServerSeq != syncResult.Events[len(syncResult.Events)-1].Seq {
		t.Errorf("server_seq = %d, want the last event seq %d",
			syncResult.ServerSeq, syncResult.Events[len(syncResult.Events)-1].Seq)
	}

	// Resuming from the cursor yields nothing new, which is what makes the
	// cursor safe to persist on the device.
	cursor := syncResult.ServerSeq
	var second struct {
		Events []json.RawMessage `json:"events"`
	}
	s.get(t, bob.access, fmt.Sprintf("/api/v1/sync?cursor=%d", cursor), &second)
	if len(second.Events) != 0 {
		t.Errorf("resuming from the cursor replayed %d events, want 0", len(second.Events))
	}
}

// The offline outbox retries indefinitely, so a repeated send must not
// duplicate the message — end to end, not just at the repository.
func TestRetriedSendIsIdempotentThroughTheAPI(t *testing.T) {
	s := newStack(t)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var chat struct {
		ChatID uuid.UUID `json:"chat_id"`
	}
	s.post(t, alice.access, "/api/v1/chats/private", map[string]any{"user_id": bob.UserID}, &chat)

	body := map[string]any{
		"client_message_id": uuid.New(),
		"type":              "text",
		"content":           "retry me",
	}
	path := "/api/v1/chats/" + chat.ChatID.String() + "/messages"

	var first, second struct {
		ID  uuid.UUID `json:"id"`
		Seq int64     `json:"seq"`
	}
	s.post(t, alice.access, path, body, &first)
	s.post(t, alice.access, path, body, &second)

	if first.ID == uuid.Nil {
		t.Fatal("the send response carried no message id")
	}
	if first.ID != second.ID {
		t.Errorf("a retry produced a different message: %s and %s", first.ID, second.ID)
	}
	if first.Seq != second.Seq {
		t.Errorf("a retry produced a different sequence: %d and %d", first.Seq, second.Seq)
	}

	var count int
	if err := s.db.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM messages WHERE chat_id = $1`, chat.ChatID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if count != 1 {
		t.Errorf("%d messages stored after a retry, want 1", count)
	}
}

// Contact discovery works through the real router, with the pepper the server
// itself publishes.
func TestContactDiscoveryOverTheAPI(t *testing.T) {
	s := newStack(t)

	alice := s.signIn(t)
	bob := s.signIn(t)

	var params struct {
		Algorithm    string `json:"algorithm"`
		Pepper       string `json:"pepper"`
		DigestLength int    `json:"digest_length"`
	}
	s.get(t, alice.access, "/api/v1/contacts/discovery-parameters", &params)
	if params.Algorithm != "HMAC-SHA256" || params.DigestLength != 32 {
		t.Fatalf("unexpected discovery parameters: %+v", params)
	}

	var result struct {
		Matched []struct {
			UserID uuid.UUID `json:"user_id"`
		} `json:"matched"`
	}
	s.post(t, alice.access, "/api/v1/contacts/sync", map[string]any{
		"replace": true,
		"entries": []map[string]any{
			// The digest is derived with the pepper the server just published,
			// exactly as a client would — not with one the test knows privately.
			{"digest": contacts.DigestFor(bob.phone, []byte(params.Pepper)), "first_name": "Bob"},
		},
	}, &result)

	if len(result.Matched) != 1 || result.Matched[0].UserID != bob.UserID {
		t.Fatalf("discovery matched %+v, want just bob (%s)", result.Matched, bob.UserID)
	}

	var list struct {
		Contacts []struct {
			UserID    uuid.UUID `json:"user_id"`
			FirstName string    `json:"first_name"`
		} `json:"contacts"`
	}
	s.get(t, alice.access, "/api/v1/contacts", &list)
	if len(list.Contacts) != 1 || list.Contacts[0].FirstName != "Bob" {
		t.Errorf("contact list = %+v, want one entry named Bob", list.Contacts)
	}
}

// An expired or forged token must not open a socket.
func TestWebSocketRejectsABadToken(t *testing.T) {
	s := newStack(t)

	wsURL := "ws" + strings.TrimPrefix(s.server.URL, "http") + "/ws?token=not-a-real-token"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a forged token opened a websocket")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Errorf("handshake with a forged token = %d, want 401", status)
	}
}

func TestRefreshRotatesTheRefreshToken(t *testing.T) {
	s := newStack(t)
	alice := s.signIn(t)

	var refreshed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	s.post(t, "", "/api/v1/auth/refresh", map[string]any{"refresh_token": alice.refresh}, &refreshed)

	if refreshed.RefreshToken == alice.refresh {
		t.Error("the refresh token was not rotated")
	}

	// Replaying the old token is a theft signal and must be refused.
	status := s.do(t, http.MethodPost, "", "/api/v1/auth/refresh",
		map[string]any{"refresh_token": alice.refresh}, nil)
	if status < 400 {
		t.Errorf("replaying a rotated refresh token = %d, want a 4xx", status)
	}
}

func TestUnknownEndpointReturnsTheErrorEnvelope(t *testing.T) {
	s := newStack(t)

	req, err := http.NewRequest(http.MethodGet, s.server.URL+"/api/v1/no-such-thing", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := s.server.Client().Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Success || env.Error == nil {
		t.Errorf("a 404 did not use the error envelope: %+v", env)
	}
}
