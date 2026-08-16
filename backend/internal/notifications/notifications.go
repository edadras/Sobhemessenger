// Package notifications owns in-app notifications and push delivery (§30).
//
// The backend decides *what* is notified and *whether* a device should be
// woken; FCM and APNs are only transports. That keeps quiet hours, per-chat
// mutes and preview settings in one place rather than split across two vendor
// payload formats.
package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

var ErrNotFound = errors.New("notifications: not found")

// Notification types (§30).
const (
	TypeNewMessage     = "new_message"
	TypeMention        = "mention"
	TypeReply          = "reply"
	TypeReaction       = "reaction"
	TypeChannelPost    = "channel_post"
	TypeBreakingNews   = "breaking_news"
	TypeCall           = "call"
	TypeGroupInvite    = "group_invite"
	TypeContactRequest = "contact_request"
	TypeStory          = "story"
	TypeSystem         = "system"
)

// Notification is one entry in a user's list.
type Notification struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Data      json.RawMessage `json:"data,omitempty"`
	Priority  string          `json:"priority"`
	ReadAt    *time.Time      `json:"read_at,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Settings are the user's delivery preferences (§30, §55).
type Settings struct {
	PrivateChats    bool   `json:"private_chats"`
	Groups          bool   `json:"groups"`
	Channels        bool   `json:"channels"`
	BreakingNews    bool   `json:"breaking_news"`
	Calls           bool   `json:"calls"`
	Stories         bool   `json:"stories"`
	ShowPreview     bool   `json:"show_preview"`
	QuietHoursStart *int16 `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd   *int16 `json:"quiet_hours_end,omitempty"`
}

// PushToken registers one device with a push provider.
type PushToken struct {
	ID       uuid.UUID `json:"id"`
	DeviceID uuid.UUID `json:"device_id"`
	Provider string    `json:"provider"`
	Token    string    `json:"-"`
	Locale   string    `json:"locale"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Create stores a notification and returns it for immediate delivery.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, notificationType, title, body string, data any, priority string) (*Notification, error) {
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("notifications: encode data: %w", err)
	}

	notification := &Notification{}
	err = r.db.Pool.QueryRow(ctx, `
		INSERT INTO notifications (user_id, type, title, body, data, priority)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, type, title, body, data, priority, read_at, created_at`,
		userID, notificationType, title, body, encoded, priority,
	).Scan(&notification.ID, &notification.Type, &notification.Title, &notification.Body,
		&notification.Data, &notification.Priority, &notification.ReadAt, &notification.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("notifications: create: %w", err)
	}
	return notification, nil
}

func (r *Repository) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, limit int, before *time.Time) ([]Notification, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, type, title, body, data, priority, read_at, created_at
		FROM notifications
		WHERE user_id = $1
		  AND (NOT $2 OR read_at IS NULL)
		  AND ($4::timestamptz IS NULL OR created_at < $4)
		ORDER BY created_at DESC
		LIMIT $3`, userID, unreadOnly, limit, before)
	if err != nil {
		return nil, fmt.Errorf("notifications: list: %w", err)
	}
	defer rows.Close()

	var notifications []Notification
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Type, &n.Title, &n.Body, &n.Data,
			&n.Priority, &n.ReadAt, &n.CreatedAt); err != nil {
			return nil, err
		}
		notifications = append(notifications, n)
	}
	return notifications, rows.Err()
}

func (r *Repository) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var count int
	err := r.db.Pool.QueryRow(ctx,
		`SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`,
		userID).Scan(&count)
	return count, err
}

func (r *Repository) MarkRead(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		_, err := r.db.Pool.Exec(ctx,
			`UPDATE notifications SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`,
			userID)
		return err
	}

	_, err := r.db.Pool.Exec(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE user_id = $1 AND id = ANY($2::uuid[]) AND read_at IS NULL`, userID, ids)
	return err
}

func (r *Repository) Settings(ctx context.Context, userID uuid.UUID) (*Settings, error) {
	settings := &Settings{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT private_chats, groups, channels, breaking_news, calls, stories,
		       show_preview, quiet_hours_start, quiet_hours_end
		FROM notification_settings WHERE user_id = $1`, userID,
	).Scan(&settings.PrivateChats, &settings.Groups, &settings.Channels,
		&settings.BreakingNews, &settings.Calls, &settings.Stories,
		&settings.ShowPreview, &settings.QuietHoursStart, &settings.QuietHoursEnd)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("notifications: read settings: %w", err)
	}
	return settings, nil
}

func (r *Repository) UpdateSettings(ctx context.Context, userID uuid.UUID, s Settings) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO notification_settings (
			user_id, private_chats, groups, channels, breaking_news, calls, stories,
			show_preview, quiet_hours_start, quiet_hours_end)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (user_id) DO UPDATE SET
			private_chats = EXCLUDED.private_chats, groups = EXCLUDED.groups,
			channels = EXCLUDED.channels, breaking_news = EXCLUDED.breaking_news,
			calls = EXCLUDED.calls, stories = EXCLUDED.stories,
			show_preview = EXCLUDED.show_preview,
			quiet_hours_start = EXCLUDED.quiet_hours_start,
			quiet_hours_end = EXCLUDED.quiet_hours_end,
			updated_at = now()`,
		userID, s.PrivateChats, s.Groups, s.Channels, s.BreakingNews, s.Calls,
		s.Stories, s.ShowPreview, s.QuietHoursStart, s.QuietHoursEnd)
	if err != nil {
		return fmt.Errorf("notifications: update settings: %w", err)
	}
	return nil
}

// RegisterToken records a device's push token.
//
// A token can migrate between devices and accounts when a phone is handed on,
// so the provider+token pair is the key and the row is reassigned rather than
// duplicated — otherwise the previous owner would keep receiving pushes.
func (r *Repository) RegisterToken(ctx context.Context, userID, deviceID uuid.UUID, provider, token, locale string) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO push_tokens (user_id, device_id, provider, token, locale)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, token) DO UPDATE
		SET user_id = EXCLUDED.user_id, device_id = EXCLUDED.device_id,
		    locale = EXCLUDED.locale, is_valid = TRUE, failure_count = 0,
		    updated_at = now()`,
		userID, deviceID, provider, token, locale)
	if err != nil {
		return fmt.Errorf("notifications: register token: %w", err)
	}

	_, err = r.db.Pool.Exec(ctx,
		`UPDATE devices SET push_token = $2, push_provider = $3 WHERE id = $1`,
		deviceID, token, provider)
	return err
}

func (r *Repository) UnregisterToken(ctx context.Context, provider, token string) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE push_tokens SET is_valid = FALSE WHERE provider = $1 AND token = $2`,
		provider, token)
	return err
}

// TokensFor returns the valid push tokens for a user, honouring their settings.
func (r *Repository) TokensFor(ctx context.Context, userID uuid.UUID) ([]PushToken, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT t.id, t.device_id, t.provider, t.token, t.locale
		FROM push_tokens t
		JOIN devices d ON d.id = t.device_id AND d.revoked_at IS NULL
		WHERE t.user_id = $1 AND t.is_valid`, userID)
	if err != nil {
		return nil, fmt.Errorf("notifications: list tokens: %w", err)
	}
	defer rows.Close()

	var tokens []PushToken
	for rows.Next() {
		var token PushToken
		if err := rows.Scan(&token.ID, &token.DeviceID, &token.Provider,
			&token.Token, &token.Locale); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

// RecordDelivery tracks a push attempt so a failing token can be retired.
func (r *Repository) RecordDelivery(ctx context.Context, notificationID uuid.UUID, tokenID *uuid.UUID, provider, status, errText string) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO notification_deliveries (notification_id, push_token_id, provider, status, attempts, error, sent_at)
		VALUES ($1, $2, $3, $4, 1, $5, CASE WHEN $4 = 'sent' THEN now() ELSE NULL END)`,
		notificationID, tokenID, provider, status, errText)
	return err
}

// InvalidateToken retires a token the provider rejected as unregistered.
func (r *Repository) InvalidateToken(ctx context.Context, tokenID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE push_tokens SET is_valid = FALSE, failure_count = failure_count + 1 WHERE id = $1`,
		tokenID)
	return err
}

// BreakingNewsAudience lists users who accept breaking-news pushes (§27).
func (r *Repository) BreakingNewsAudience(ctx context.Context, limit, offset int) ([]uuid.UUID, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT s.user_id
		FROM notification_settings s
		JOIN users u ON u.id = s.user_id AND u.deleted_at IS NULL AND u.status = 'active'
		WHERE s.breaking_news
		ORDER BY s.user_id
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("notifications: breaking news audience: %w", err)
	}
	defer rows.Close()

	var users []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		users = append(users, id)
	}
	return users, rows.Err()
}

// ---------------------------------------------------------------- service

type Service struct {
	repo *Repository
	bus  *bus.Bus
}

func NewService(repo *Repository, messageBus *bus.Bus) *Service {
	return &Service{repo: repo, bus: messageBus}
}

// Deliver records a notification and queues the push.
//
// The user's settings and quiet hours are evaluated here, so a muted category
// never reaches the queue at all rather than being filtered by the worker.
func (s *Service) Deliver(ctx context.Context, userID uuid.UUID, notificationType, title, body string, data any, priority string) error {
	settings, err := s.repo.Settings(ctx, userID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if settings != nil && !allowsType(settings, notificationType) {
		return nil
	}

	notification, err := s.repo.Create(ctx, userID, notificationType, title, body, data, priority)
	if err != nil {
		return err
	}

	// Calls override quiet hours: a silenced incoming call is a missed call.
	if settings != nil && notificationType != TypeCall && inQuietHours(settings, time.Now()) {
		return nil
	}

	// Preview-off users get the notification without its contents.
	pushTitle, pushBody := title, body
	if settings != nil && !settings.ShowPreview {
		pushTitle = "SOBH"
		pushBody = ""
	}

	return s.bus.PublishJob(ctx, bus.SubjectJobPushSend, notification.ID.String(), map[string]any{
		"notification_id": notification.ID,
		"user_id":         userID,
		"type":            notificationType,
		"title":           pushTitle,
		"body":            pushBody,
		"data":            data,
		"priority":        priority,
	})
}

// allowsType maps a notification type onto the user's category switches.
func allowsType(s *Settings, notificationType string) bool {
	switch notificationType {
	case TypeNewMessage, TypeReply, TypeReaction:
		return s.PrivateChats || s.Groups
	case TypeChannelPost:
		return s.Channels
	case TypeBreakingNews:
		return s.BreakingNews
	case TypeCall:
		return s.Calls
	case TypeStory:
		return s.Stories
	default:
		// Mentions, invites and system notices are always delivered: they are
		// directed at the user personally.
		return true
	}
}

// inQuietHours handles both same-day (09→17) and overnight (22→07) windows.
func inQuietHours(s *Settings, now time.Time) bool {
	if s.QuietHoursStart == nil || s.QuietHoursEnd == nil {
		return false
	}
	start, end := int(*s.QuietHoursStart), int(*s.QuietHoursEnd)
	if start == end {
		return false
	}

	hour := now.Hour()
	if start < end {
		return hour >= start && hour < end
	}
	return hour >= start || hour < end
}

func (s *Service) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, limit int, before *time.Time) ([]Notification, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	notifications, err := s.repo.List(ctx, userID, unreadOnly, limit, before)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return notifications, nil
}

func (s *Service) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	count, err := s.repo.UnreadCount(ctx, userID)
	if err != nil {
		return 0, httpx.Internal(err)
	}
	return count, nil
}

func (s *Service) MarkRead(ctx context.Context, userID uuid.UUID, ids []uuid.UUID) error {
	if err := s.repo.MarkRead(ctx, userID, ids); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Settings(ctx context.Context, userID uuid.UUID) (*Settings, error) {
	settings, err := s.repo.Settings(ctx, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// A user created before this table existed still gets the defaults.
			return &Settings{
				PrivateChats: true, Groups: true, Channels: true,
				BreakingNews: true, Calls: true, ShowPreview: true,
			}, nil
		}
		return nil, httpx.Internal(err)
	}
	return settings, nil
}

func (s *Service) UpdateSettings(ctx context.Context, userID uuid.UUID, settings Settings) error {
	for _, hour := range []*int16{settings.QuietHoursStart, settings.QuietHoursEnd} {
		if hour != nil && (*hour < 0 || *hour > 23) {
			return httpx.Validation("Quiet hours must be an hour of the day").
				WithField("quiet_hours", "between 0 and 23")
		}
	}
	if err := s.repo.UpdateSettings(ctx, userID, settings); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) RegisterToken(ctx context.Context, userID, deviceID uuid.UUID, provider, token, locale string) error {
	switch provider {
	case "fcm", "apns", "web":
	default:
		return httpx.Validation("Unsupported push provider").
			WithField("provider", "must be fcm, apns or web")
	}
	if token == "" {
		return httpx.Validation("A push token is required").WithField("token", "required")
	}
	if locale == "" {
		locale = "fa"
	}

	if err := s.repo.RegisterToken(ctx, userID, deviceID, provider, token, locale); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) UnregisterToken(ctx context.Context, provider, token string) error {
	if err := s.repo.UnregisterToken(ctx, provider, token); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/unread-count", h.unreadCount)
	r.Post("/read", h.markRead)
	r.Get("/settings", h.settings)
	r.Put("/settings", h.updateSettings)
	r.Post("/tokens", h.registerToken)
	r.Delete("/tokens", h.unregisterToken)
	return r
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var before *time.Time
	if raw := r.URL.Query().Get("before"); raw != "" {
		parsed, parseErr := time.Parse(time.RFC3339, raw)
		if parseErr != nil {
			httpx.Fail(w, r, httpx.BadRequest("before must be an RFC3339 timestamp"))
			return
		}
		before = &parsed
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))

	notifications, err := h.service.List(r.Context(), principal.UserID,
		r.URL.Query().Get("unread") == "true", limit, before)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"notifications": notifications})
}

func (h *Handler) unreadCount(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	count, err := h.service.UnreadCount(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"unread_count": count})
}

func (h *Handler) markRead(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		IDs []uuid.UUID `json:"ids,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}

	if err := h.service.MarkRead(r.Context(), principal.UserID, body.IDs); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) settings(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	settings, err := h.service.Settings(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, settings)
}

func (h *Handler) updateSettings(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body Settings
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UpdateSettings(r.Context(), principal.UserID, body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) registerToken(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Provider string `json:"provider"`
		Token    string `json:"token"`
		Locale   string `json:"locale"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.RegisterToken(r.Context(), principal.UserID, principal.DeviceID,
		body.Provider, body.Token, body.Locale); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) unregisterToken(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Provider string `json:"provider"`
		Token    string `json:"token"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.UnregisterToken(r.Context(), body.Provider, body.Token); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}
