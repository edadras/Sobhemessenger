package notifications

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/net/http2"

	"github.com/sobh/messenger/backend/internal/config"
)

// Message is a transport-neutral push, rendered per provider.
type Message struct {
	Token    string
	Title    string
	Body     string
	Data     map[string]string
	Priority string
	Badge    *int
	Sound    string
	// CollapseKey lets a provider replace a superseded notification rather than
	// stacking it — several messages in one chat should not produce a wall of
	// alerts.
	CollapseKey string
}

// SendResult reports what happened to one push.
type SendResult struct {
	Delivered bool
	// TokenInvalid means the provider says this token is dead and it should be
	// retired rather than retried.
	TokenInvalid bool
	Detail       string
}

// Sender delivers to one push provider.
type Sender interface {
	Send(ctx context.Context, message Message) (SendResult, error)
	Provider() string
}

// NewSenders builds the configured providers. A provider without credentials
// is simply absent, so a deployment can run with only one of them.
func NewSenders(cfg config.Push) map[string]Sender {
	senders := make(map[string]Sender, 2)

	if cfg.FCMCredentials != "" {
		if sender, err := newFCMSender(cfg); err == nil {
			senders["fcm"] = sender
		}
	}
	if cfg.APNsPrivateKey != "" && cfg.APNsKeyID != "" && cfg.APNsTeamID != "" {
		if sender, err := newAPNsSender(cfg); err == nil {
			senders["apns"] = sender
		}
	}
	return senders
}

// ---------------------------------------------------------------- FCM

// fcmSender speaks the FCM HTTP v1 API, authenticated with a service account.
type fcmSender struct {
	cfg        config.Push
	http       *http.Client
	projectID  string
	clientMail string
	privateKey *rsaKey

	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time
}

// rsaKey wraps the parsed service-account key.
type rsaKey struct{ key any }

func newFCMSender(cfg config.Push) (*fcmSender, error) {
	var account struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal([]byte(cfg.FCMCredentials), &account); err != nil {
		return nil, fmt.Errorf("notifications: parse FCM credentials: %w", err)
	}

	block, _ := pem.Decode([]byte(account.PrivateKey))
	if block == nil {
		return nil, errors.New("notifications: FCM private key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("notifications: parse FCM private key: %w", err)
	}

	return &fcmSender{
		cfg:        cfg,
		http:       &http.Client{Timeout: 15 * time.Second},
		projectID:  account.ProjectID,
		clientMail: account.ClientEmail,
		privateKey: &rsaKey{key: parsed},
	}, nil
}

func (s *fcmSender) Provider() string { return "fcm" }

// accessTokenFor mints (and caches) an OAuth token from the service account.
func (s *fcmSender) accessTokenFor(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Refresh a minute early so a token cannot expire mid-flight.
	if s.accessToken != "" && time.Now().Before(s.expiresAt.Add(-time.Minute)) {
		return s.accessToken, nil
	}

	now := time.Now()
	claims := jwt.MapClaims{
		"iss":   s.clientMail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   "https://oauth2.googleapis.com/token",
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	}
	assertion, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(s.privateKey.key)
	if err != nil {
		return "", fmt.Errorf("notifications: sign FCM assertion: %w", err)
	}

	form := strings.NewReader(
		"grant_type=urn:ietf:params:oauth:grant-type:jwt-bearer&assertion=" + assertion)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://oauth2.googleapis.com/token", form)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("notifications: request FCM token: %w", err)
	}
	defer resp.Body.Close()

	var payload struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("notifications: decode FCM token: %w", err)
	}
	if payload.AccessToken == "" {
		return "", errors.New("notifications: FCM returned no access token")
	}

	s.accessToken = payload.AccessToken
	s.expiresAt = time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second)
	return s.accessToken, nil
}

func (s *fcmSender) Send(ctx context.Context, message Message) (SendResult, error) {
	token, err := s.accessTokenFor(ctx)
	if err != nil {
		return SendResult{}, err
	}

	androidPriority := "normal"
	if message.Priority == "high" {
		androidPriority = "high"
	}

	payload := map[string]any{
		"message": map[string]any{
			"token": message.Token,
			"notification": map[string]any{
				"title": message.Title,
				"body":  message.Body,
			},
			"data": message.Data,
			"android": map[string]any{
				"priority":     androidPriority,
				"collapse_key": message.CollapseKey,
				"notification": map[string]any{
					"sound":                   defaultString(message.Sound, "default"),
					"channel_id":              "sobh_messages",
					"default_vibrate_timings": true,
				},
			},
		},
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return SendResult{}, err
	}

	url := fmt.Sprintf("%s/projects/%s/messages:send",
		strings.TrimRight(s.cfg.FCMEndpoint, "/"), s.projectID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("notifications: FCM send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 300 {
		return SendResult{Delivered: true}, nil
	}

	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	// 404 UNREGISTERED and 400 INVALID_ARGUMENT on the token mean the
	// registration is gone for good.
	invalid := resp.StatusCode == http.StatusNotFound ||
		bytes.Contains(detail, []byte("UNREGISTERED")) ||
		bytes.Contains(detail, []byte("INVALID_ARGUMENT"))

	return SendResult{
		TokenInvalid: invalid,
		Detail:       fmt.Sprintf("fcm %d: %s", resp.StatusCode, bytes.TrimSpace(detail)),
	}, nil
}

// ---------------------------------------------------------------- APNs

// apnsSender speaks APNs over HTTP/2 with token-based authentication.
type apnsSender struct {
	cfg  config.Push
	http *http.Client
	key  *ecdsa.PrivateKey

	mu       sync.Mutex
	jwtToken string
	issuedAt time.Time
}

func newAPNsSender(cfg config.Push) (*apnsSender, error) {
	block, _ := pem.Decode([]byte(cfg.APNsPrivateKey))
	if block == nil {
		return nil, errors.New("notifications: APNs key is not valid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("notifications: parse APNs key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("notifications: APNs key is not an ECDSA key")
	}

	// APNs requires HTTP/2; the default transport would negotiate HTTP/1.1
	// against this endpoint and every request would fail.
	transport := &http.Transport{}
	if err := http2.ConfigureTransport(transport); err != nil {
		return nil, fmt.Errorf("notifications: configure HTTP/2: %w", err)
	}

	return &apnsSender{
		cfg:  cfg,
		http: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		key:  key,
	}, nil
}

func (s *apnsSender) Provider() string { return "apns" }

// authToken returns the provider token. Apple requires it be refreshed no more
// than once every 20 minutes and no less than once an hour.
func (s *apnsSender) authToken() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.jwtToken != "" && time.Since(s.issuedAt) < 45*time.Minute {
		return s.jwtToken, nil
	}

	now := time.Now()
	token := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": s.cfg.APNsTeamID,
		"iat": now.Unix(),
	})
	token.Header["kid"] = s.cfg.APNsKeyID

	signed, err := token.SignedString(s.key)
	if err != nil {
		return "", fmt.Errorf("notifications: sign APNs token: %w", err)
	}

	s.jwtToken = signed
	s.issuedAt = now
	return signed, nil
}

func (s *apnsSender) Send(ctx context.Context, message Message) (SendResult, error) {
	token, err := s.authToken()
	if err != nil {
		return SendResult{}, err
	}

	alert := map[string]any{"title": message.Title}
	if message.Body != "" {
		alert["body"] = message.Body
	}
	aps := map[string]any{
		"alert": alert,
		"sound": defaultString(message.Sound, "default"),
	}
	if message.Badge != nil {
		aps["badge"] = *message.Badge
	}

	payload := map[string]any{"aps": aps}
	for key, value := range message.Data {
		payload[key] = value
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return SendResult{}, err
	}

	url := fmt.Sprintf("%s/3/device/%s",
		strings.TrimRight(s.cfg.APNsEndpoint, "/"), message.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return SendResult{}, err
	}
	req.Header.Set("authorization", "bearer "+token)
	req.Header.Set("apns-topic", s.cfg.APNsBundleID)
	req.Header.Set("apns-push-type", "alert")
	if message.CollapseKey != "" {
		req.Header.Set("apns-collapse-id", truncate(message.CollapseKey, 64))
	}
	if message.Priority == "high" {
		req.Header.Set("apns-priority", "10")
	} else {
		req.Header.Set("apns-priority", "5")
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return SendResult{}, fmt.Errorf("notifications: APNs send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return SendResult{Delivered: true}, nil
	}

	detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	// 410 Gone and BadDeviceToken both mean the device no longer accepts pushes.
	invalid := resp.StatusCode == http.StatusGone ||
		bytes.Contains(detail, []byte("BadDeviceToken")) ||
		bytes.Contains(detail, []byte("Unregistered"))

	return SendResult{
		TokenInvalid: invalid,
		Detail:       fmt.Sprintf("apns %d: %s", resp.StatusCode, bytes.TrimSpace(detail)),
	}, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
