// Package calls implements WebRTC signalling (§18).
//
// The server is a signalling relay and nothing more: it never terminates media,
// never inspects SDP, and never sees decrypted audio or video. Peers exchange
// offers, answers and ICE candidates through it, then connect directly or via
// TURN.
package calls

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var ErrNotFound = errors.New("calls: not found")

// Realtime events pushed to participants (§8).
const (
	EventCallIncoming = "call.incoming"
	EventCallAccepted = "call.accepted"
	EventCallRejected = "call.rejected"
	EventCallEnded    = "call.ended"
	EventCallSignal   = "call.signal"
)

// Call is one voice or video session.
type Call struct {
	ID              uuid.UUID     `json:"id"`
	ChatID          *uuid.UUID    `json:"chat_id,omitempty"`
	InitiatorID     uuid.UUID     `json:"initiator_id"`
	Type            string        `json:"type"`
	Scope           string        `json:"scope"`
	State           string        `json:"state"`
	EndReason       string        `json:"end_reason,omitempty"`
	MaxParticipants int           `json:"max_participants"`
	StartedAt       time.Time     `json:"started_at"`
	ConnectedAt     *time.Time    `json:"connected_at,omitempty"`
	EndedAt         *time.Time    `json:"ended_at,omitempty"`
	DurationSeconds *int          `json:"duration_seconds,omitempty"`
	Participants    []Participant `json:"participants,omitempty"`
}

// Participant is one person in a call.
type Participant struct {
	UserID        uuid.UUID  `json:"user_id"`
	DisplayName   string     `json:"display_name,omitempty"`
	State         string     `json:"state"`
	IsMuted       bool       `json:"is_muted"`
	VideoEnabled  bool       `json:"video_enabled"`
	ScreenSharing bool       `json:"screen_sharing"`
	JoinedAt      *time.Time `json:"joined_at,omitempty"`
	LeftAt        *time.Time `json:"left_at,omitempty"`
}

// ICEServer is one STUN or TURN endpoint handed to the client.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Create opens a call and invites its participants in one transaction.
func (r *Repository) Create(ctx context.Context, chatID *uuid.UUID, initiatorID uuid.UUID, callType, scope string, maxParticipants int, invitees []uuid.UUID) (*Call, error) {
	call := &Call{}

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO calls (chat_id, initiator_id, type, scope, max_participants)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, chat_id, initiator_id, type, scope, state, end_reason,
			          max_participants, started_at, connected_at, ended_at, duration_seconds`,
			chatID, initiatorID, callType, scope, maxParticipants,
		).Scan(&call.ID, &call.ChatID, &call.InitiatorID, &call.Type, &call.Scope,
			&call.State, &call.EndReason, &call.MaxParticipants, &call.StartedAt,
			&call.ConnectedAt, &call.EndedAt, &call.DurationSeconds)
		if err != nil {
			return fmt.Errorf("calls: create: %w", err)
		}

		// The initiator is joined immediately; everyone else starts ringing.
		if _, err := tx.Exec(ctx, `
			INSERT INTO call_participants (call_id, user_id, state, joined_at)
			VALUES ($1, $2, 'joined', now())`, call.ID, initiatorID); err != nil {
			return fmt.Errorf("calls: add initiator: %w", err)
		}

		for _, invitee := range invitees {
			if invitee == initiatorID {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO call_participants (call_id, user_id, state)
				VALUES ($1, $2, 'ringing')
				ON CONFLICT DO NOTHING`, call.ID, invitee); err != nil {
				return fmt.Errorf("calls: invite participant: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return call, nil
}

func (r *Repository) ByID(ctx context.Context, callID uuid.UUID) (*Call, error) {
	call := &Call{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, chat_id, initiator_id, type, scope, state, end_reason,
		       max_participants, started_at, connected_at, ended_at, duration_seconds
		FROM calls WHERE id = $1`, callID,
	).Scan(&call.ID, &call.ChatID, &call.InitiatorID, &call.Type, &call.Scope,
		&call.State, &call.EndReason, &call.MaxParticipants, &call.StartedAt,
		&call.ConnectedAt, &call.EndedAt, &call.DurationSeconds)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("calls: read: %w", err)
	}

	participants, err := r.Participants(ctx, callID)
	if err != nil {
		return nil, err
	}
	call.Participants = participants
	return call, nil
}

func (r *Repository) Participants(ctx context.Context, callID uuid.UUID) ([]Participant, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT p.user_id, COALESCE(pr.display_name, ''), p.state, p.is_muted,
		       p.video_enabled, p.screen_sharing, p.joined_at, p.left_at
		FROM call_participants p
		LEFT JOIN user_profiles pr ON pr.user_id = p.user_id
		WHERE p.call_id = $1`, callID)
	if err != nil {
		return nil, fmt.Errorf("calls: list participants: %w", err)
	}
	defer rows.Close()

	var participants []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.UserID, &p.DisplayName, &p.State, &p.IsMuted,
			&p.VideoEnabled, &p.ScreenSharing, &p.JoinedAt, &p.LeftAt); err != nil {
			return nil, err
		}
		participants = append(participants, p)
	}
	return participants, rows.Err()
}

// Join marks a participant as connected and moves the call to active on the
// first join.
func (r *Repository) Join(ctx context.Context, callID, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var state string
		var joined, maxParticipants int
		err := tx.QueryRow(ctx, `
			SELECT c.state, c.max_participants,
			       (SELECT count(*) FROM call_participants p
			        WHERE p.call_id = c.id AND p.state = 'joined')
			FROM calls c WHERE c.id = $1 FOR UPDATE`,
			callID).Scan(&state, &maxParticipants, &joined)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("calls: lock call: %w", err)
		}
		if state == "ended" || state == "rejected" || state == "missed" {
			return ErrCallEnded
		}
		if joined >= maxParticipants {
			return ErrCallFull
		}

		tag, err := tx.Exec(ctx, `
			INSERT INTO call_participants (call_id, user_id, state, joined_at)
			VALUES ($1, $2, 'joined', now())
			ON CONFLICT (call_id, user_id) DO UPDATE
			SET state = 'joined', joined_at = COALESCE(call_participants.joined_at, now()),
			    left_at = NULL`, callID, userID)
		if err != nil {
			return fmt.Errorf("calls: join: %w", err)
		}
		_ = tag

		if state == "ringing" {
			_, err = tx.Exec(ctx, `
				UPDATE calls SET state = 'active', connected_at = COALESCE(connected_at, now())
				WHERE id = $1`, callID)
		}
		return err
	})
}

var (
	ErrCallEnded = errors.New("calls: call has already ended")
	ErrCallFull  = errors.New("calls: call is full")
)

// Leave removes a participant and ends the call when nobody is left.
func (r *Repository) Leave(ctx context.Context, callID, userID uuid.UUID) (bool, error) {
	var ended bool

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE call_participants SET state = 'left', left_at = now()
			WHERE call_id = $1 AND user_id = $2 AND state <> 'left'`, callID, userID); err != nil {
			return fmt.Errorf("calls: leave: %w", err)
		}

		var remaining int
		if err := tx.QueryRow(ctx, `
			SELECT count(*) FROM call_participants
			WHERE call_id = $1 AND state IN ('joined', 'ringing', 'invited')`,
			callID).Scan(&remaining); err != nil {
			return err
		}
		if remaining > 0 {
			return nil
		}

		// A call with nobody left in it is over.
		ended = true
		_, err := tx.Exec(ctx, `
			UPDATE calls
			SET state = 'ended', ended_at = now(), end_reason = 'all_participants_left',
			    duration_seconds = CASE
			        WHEN connected_at IS NOT NULL
			        THEN EXTRACT(EPOCH FROM (now() - connected_at))::int
			        ELSE 0 END
			WHERE id = $1 AND state NOT IN ('ended', 'rejected', 'missed')`, callID)
		return err
	})
	return ended, err
}

// SetState ends or rejects a call.
func (r *Repository) SetState(ctx context.Context, callID uuid.UUID, state, reason string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE calls
		SET state = $2, end_reason = $3, ended_at = now(),
		    duration_seconds = CASE
		        WHEN connected_at IS NOT NULL
		        THEN EXTRACT(EPOCH FROM (now() - connected_at))::int
		        ELSE 0 END
		WHERE id = $1 AND state NOT IN ('ended', 'rejected', 'missed')`, callID, state, reason)
	if err != nil {
		return fmt.Errorf("calls: set state: %w", err)
	}
	return nil
}

func (r *Repository) SetParticipantState(ctx context.Context, callID, userID uuid.UUID, state string) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE call_participants SET state = $3 WHERE call_id = $1 AND user_id = $2`,
		callID, userID, state)
	return err
}

func (r *Repository) SetMedia(ctx context.Context, callID, userID uuid.UUID, muted, video, screen *bool) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE call_participants
		SET is_muted = COALESCE($3, is_muted),
		    video_enabled = COALESCE($4, video_enabled),
		    screen_sharing = COALESCE($5, screen_sharing)
		WHERE call_id = $1 AND user_id = $2`, callID, userID, muted, video, screen)
	return err
}

// IsParticipant gates every signalling operation.
func (r *Repository) IsParticipant(ctx context.Context, callID, userID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM call_participants WHERE call_id = $1 AND user_id = $2)`,
		callID, userID).Scan(&exists)
	return exists, err
}

// History lists a user's recent calls.
func (r *Repository) History(ctx context.Context, userID uuid.UUID, limit int) ([]Call, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.id, c.chat_id, c.initiator_id, c.type, c.scope, c.state, c.end_reason,
		       c.max_participants, c.started_at, c.connected_at, c.ended_at, c.duration_seconds
		FROM calls c
		JOIN call_participants p ON p.call_id = c.id AND p.user_id = $1
		ORDER BY c.started_at DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("calls: history: %w", err)
	}
	defer rows.Close()

	var calls []Call
	for rows.Next() {
		var call Call
		if err := rows.Scan(&call.ID, &call.ChatID, &call.InitiatorID, &call.Type, &call.Scope,
			&call.State, &call.EndReason, &call.MaxParticipants, &call.StartedAt,
			&call.ConnectedAt, &call.EndedAt, &call.DurationSeconds); err != nil {
			return nil, err
		}
		calls = append(calls, call)
	}
	return calls, rows.Err()
}

// ---------------------------------------------------------------- service

type Service struct {
	repo      *Repository
	messaging *messaging.Repository
	bus       *bus.Bus
	cfg       config.Calls
	logger    *slog.Logger
}

func NewService(repo *Repository, messagingRepo *messaging.Repository, messageBus *bus.Bus, cfg config.Calls, logger *slog.Logger) *Service {
	return &Service{repo: repo, messaging: messagingRepo, bus: messageBus, cfg: cfg, logger: logger}
}

// Start places a call to a chat's members.
func (s *Service) Start(ctx context.Context, chatID, initiatorID uuid.UUID, callType string) (*Call, error) {
	if callType != "voice" && callType != "video" {
		return nil, httpx.Validation("Unsupported call type").
			WithField("type", "must be voice or video")
	}

	chatCtx, err := s.messaging.ChatContextFor(ctx, chatID, initiatorID)
	if err != nil {
		if errors.Is(err, messaging.ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeChatNotFound, "Chat not found")
		}
		return nil, httpx.Internal(err)
	}
	if !chatCtx.IsMember {
		return nil, httpx.Forbidden(httpx.CodeNotChatMember, "You are not a member of this chat")
	}

	scope := "direct"
	if chatCtx.ChatType != messaging.ChatPrivate {
		scope = "group"
		if !chatCtx.Permissions.ManageCalls {
			return nil, httpx.Forbidden(httpx.CodePermissionDenied,
				"You cannot start calls in this chat")
		}
	}

	members, err := s.messaging.ChatMemberIDs(ctx, chatID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if scope == "group" && len(members) > s.cfg.MaxGroupParticipants {
		// Everyone is still invited; the cap applies to simultaneous joins.
		s.logger.Info("group call invited more members than the participant cap",
			slog.Int("members", len(members)),
			slog.Int("cap", s.cfg.MaxGroupParticipants))
	}

	call, err := s.repo.Create(ctx, &chatID, initiatorID, callType, scope,
		s.cfg.MaxGroupParticipants, members)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	for _, member := range members {
		if member == initiatorID {
			continue
		}
		s.notify(member, EventCallIncoming, map[string]any{
			"call": call, "from": initiatorID,
		})
	}
	return call, nil
}

// Accept joins the caller to a ringing call.
func (s *Service) Accept(ctx context.Context, callID, userID uuid.UUID) (*Call, error) {
	if err := s.requireParticipant(ctx, callID, userID); err != nil {
		return nil, err
	}

	err := s.repo.Join(ctx, callID, userID)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		return nil, httpx.NotFound(httpx.CodeCallNotFound, "Call not found")
	case errors.Is(err, ErrCallEnded):
		return nil, httpx.Conflict(httpx.CodeCallEnded, "This call has already ended")
	case errors.Is(err, ErrCallFull):
		return nil, httpx.Forbidden(httpx.CodeCallFull, "This call is full")
	default:
		return nil, httpx.Internal(err)
	}

	call, err := s.repo.ByID(ctx, callID)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	s.broadcast(call, EventCallAccepted, map[string]any{
		"call_id": callID, "user_id": userID,
	}, userID)
	return call, nil
}

// Reject declines an incoming call.
func (s *Service) Reject(ctx context.Context, callID, userID uuid.UUID) error {
	if err := s.requireParticipant(ctx, callID, userID); err != nil {
		return err
	}

	call, err := s.repo.ByID(ctx, callID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeCallNotFound, "Call not found")
		}
		return httpx.Internal(err)
	}

	if err := s.repo.SetParticipantState(ctx, callID, userID, "rejected"); err != nil {
		return httpx.Internal(err)
	}
	// A one-to-one call is over the moment the callee declines; a group call
	// continues without them.
	if call.Scope == "direct" {
		if err := s.repo.SetState(ctx, callID, "rejected", "declined"); err != nil {
			return httpx.Internal(err)
		}
	}

	s.broadcast(call, EventCallRejected, map[string]any{
		"call_id": callID, "user_id": userID,
	}, userID)
	return nil
}

// End hangs up.
func (s *Service) End(ctx context.Context, callID, userID uuid.UUID, reason string) error {
	if err := s.requireParticipant(ctx, callID, userID); err != nil {
		return err
	}

	call, err := s.repo.ByID(ctx, callID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeCallNotFound, "Call not found")
		}
		return httpx.Internal(err)
	}

	ended, err := s.repo.Leave(ctx, callID, userID)
	if err != nil {
		return httpx.Internal(err)
	}
	// In a one-to-one call either party hanging up ends it for both.
	if call.Scope == "direct" && !ended {
		if err := s.repo.SetState(ctx, callID, "ended", reason); err != nil {
			return httpx.Internal(err)
		}
		ended = true
	}

	event := map[string]any{"call_id": callID, "user_id": userID, "ended": ended}
	s.broadcast(call, EventCallEnded, event, uuid.Nil)
	return nil
}

// Signal relays one SDP or ICE payload to another participant.
//
// The payload is passed through untouched: the server has no business parsing
// session descriptions, and doing so would only create a place for it to break.
func (s *Service) Signal(ctx context.Context, callID, fromUserID, toUserID uuid.UUID, signalType string, payload any) error {
	if err := s.requireParticipant(ctx, callID, fromUserID); err != nil {
		return err
	}
	ok, err := s.repo.IsParticipant(ctx, callID, toUserID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !ok {
		return httpx.NotFound(httpx.CodeNotFound, "That user is not in this call")
	}

	s.notify(toUserID, EventCallSignal, map[string]any{
		"call_id": callID,
		"from":    fromUserID,
		"type":    signalType,
		"payload": payload,
	})
	return nil
}

// SetMedia updates mute, video and screen-share state.
func (s *Service) SetMedia(ctx context.Context, callID, userID uuid.UUID, muted, video, screen *bool) error {
	if err := s.requireParticipant(ctx, callID, userID); err != nil {
		return err
	}
	if err := s.repo.SetMedia(ctx, callID, userID, muted, video, screen); err != nil {
		return httpx.Internal(err)
	}

	call, err := s.repo.ByID(ctx, callID)
	if err != nil {
		return httpx.Internal(err)
	}
	s.broadcast(call, "call.media_changed", map[string]any{
		"call_id": callID, "user_id": userID,
		"is_muted": muted, "video_enabled": video, "screen_sharing": screen,
	}, userID)
	return nil
}

// ICEServers returns the STUN and TURN configuration for a client (§18).
//
// TURN credentials are ephemeral and derived from a shared secret using the
// standard REST scheme: the username is an expiry timestamp and the password is
// its HMAC. The TURN server validates them without any coordination with us,
// and they stop working on their own.
func (s *Service) ICEServers(ctx context.Context, userID uuid.UUID) ([]ICEServer, error) {
	servers := make([]ICEServer, 0, 2)

	if len(s.cfg.STUNServers) > 0 {
		servers = append(servers, ICEServer{URLs: s.cfg.STUNServers})
	}

	if len(s.cfg.TURNServers) > 0 && s.cfg.TURNSecret != "" {
		expiry := time.Now().Add(s.cfg.TURNCredentialTTL).Unix()
		username := fmt.Sprintf("%d:%s", expiry, userID)

		mac := hmac.New(sha1.New, []byte(s.cfg.TURNSecret))
		mac.Write([]byte(username))
		credential := base64.StdEncoding.EncodeToString(mac.Sum(nil))

		servers = append(servers, ICEServer{
			URLs:       s.cfg.TURNServers,
			Username:   username,
			Credential: credential,
		})
	}

	return servers, nil
}

func (s *Service) History(ctx context.Context, userID uuid.UUID, limit int) ([]Call, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	calls, err := s.repo.History(ctx, userID, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return calls, nil
}

func (s *Service) Get(ctx context.Context, callID, userID uuid.UUID) (*Call, error) {
	if err := s.requireParticipant(ctx, callID, userID); err != nil {
		return nil, err
	}
	call, err := s.repo.ByID(ctx, callID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeCallNotFound, "Call not found")
		}
		return nil, httpx.Internal(err)
	}
	return call, nil
}

func (s *Service) requireParticipant(ctx context.Context, callID, userID uuid.UUID) error {
	ok, err := s.repo.IsParticipant(ctx, callID, userID)
	if err != nil {
		return httpx.Internal(err)
	}
	if !ok {
		// Indistinguishable from a missing call, so call ids cannot be probed.
		return httpx.NotFound(httpx.CodeCallNotFound, "Call not found")
	}
	return nil
}

// broadcast pushes an event to every participant except `except`.
func (s *Service) broadcast(call *Call, event string, payload map[string]any, except uuid.UUID) {
	for _, participant := range call.Participants {
		if participant.UserID == except {
			continue
		}
		s.notify(participant.UserID, event, payload)
	}
}

func (s *Service) notify(userID uuid.UUID, event string, payload map[string]any) {
	if err := s.bus.PublishRealtime(bus.UserSubject(userID.String()), map[string]any{
		"event":   event,
		"payload": payload,
	}); err != nil {
		s.logger.Warn("could not deliver call event",
			slog.String("event", event),
			slog.String("user_id", userID.String()),
			slog.Any("error", err))
	}
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.history)
	r.Post("/", h.start)
	r.Get("/ice-servers", h.iceServers)
	r.Get("/{callID}", h.get)
	r.Post("/{callID}/accept", h.accept)
	r.Post("/{callID}/reject", h.reject)
	r.Post("/{callID}/end", h.end)
	r.Post("/{callID}/signal", h.signal)
	r.Put("/{callID}/media", h.setMedia)
	return r
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	calls, err := h.service.History(r.Context(), principal.UserID, limit)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"calls": calls})
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID uuid.UUID `json:"chat_id"`
		Type   string    `json:"type"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	call, err := h.service.Start(r.Context(), body.ChatID, principal.UserID, body.Type)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, call)
}

func (h *Handler) iceServers(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	servers, err := h.service.ICEServers(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"ice_servers": servers})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	call, err := h.service.Get(r.Context(), callID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, call)
}

func (h *Handler) accept(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	call, err := h.service.Accept(r.Context(), callID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, call)
}

func (h *Handler) reject(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Reject(r.Context(), callID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) end(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Reason string `json:"reason,omitempty"`
	}
	if r.ContentLength > 0 {
		if err := httpx.DecodeJSON(r, &body); err != nil {
			httpx.Fail(w, r, err)
			return
		}
	}
	if body.Reason == "" {
		body.Reason = "hangup"
	}

	if err := h.service.End(r.Context(), callID, principal.UserID, body.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) signal(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		To      uuid.UUID `json:"to"`
		Type    string    `json:"type"`
		Payload any       `json:"payload"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	switch body.Type {
	case "offer", "answer", "ice-candidate":
	default:
		httpx.Fail(w, r, httpx.Validation("Unsupported signal type").
			WithField("type", "must be offer, answer or ice-candidate"))
		return
	}

	if err := h.service.Signal(r.Context(), callID, principal.UserID,
		body.To, body.Type, body.Payload); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setMedia(w http.ResponseWriter, r *http.Request) {
	principal, callID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		IsMuted       *bool `json:"is_muted,omitempty"`
		VideoEnabled  *bool `json:"video_enabled,omitempty"`
		ScreenSharing *bool `json:"screen_sharing,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetMedia(r.Context(), callID, principal.UserID,
		body.IsMuted, body.VideoEnabled, body.ScreenSharing); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) context(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	callID, parseErr := uuid.Parse(chi.URLParam(r, "callID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("callID is not a valid UUID")
	}
	return principal, callID, nil
}
