// Package datarights implements the two things a person may ask of their own
// account: a copy of what is held about them, and its deletion (§56).
//
// `data_requests` has been in the schema since migration 0008 and unwritten
// since. Both operations are asynchronous by nature — an export walks a dozen
// tables and uploads a file, and a deletion is deliberately delayed — so the
// API records the request and a worker carries it out.
//
// The delay on deletion is the point of the `execute_after` column. Somebody
// who deletes their account in anger, or whose account is deleted by an
// intruder who got hold of a session, has a window in which to take it back.
// The account stays usable during that window: revoking sessions immediately
// would take away the only means of cancelling.
package datarights

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// DeletionDelay is how long a deletion waits before it becomes irreversible.
const DeletionDelay = 7 * 24 * time.Hour

// Request types and statuses, matching the CHECK constraints.
const (
	TypeExport = "export"
	TypeDelete = "delete"

	StatusPending    = "pending"
	StatusProcessing = "processing"
	StatusReady      = "ready"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
	StatusCompleted  = "completed"
)

var (
	ErrNotFound  = errors.New("datarights: no such request")
	ErrNotOpen   = errors.New("datarights: that request can no longer be cancelled")
	ErrDuplicate = errors.New("datarights: a request of that kind is already open")
)

// Request is one export or deletion.
type Request struct {
	ID     uuid.UUID `json:"id"`
	UserID uuid.UUID `json:"user_id"`
	Type   string    `json:"type"`
	Status string    `json:"status"`
	// ResultMediaID is the export archive, once there is one.
	ResultMediaID *uuid.UUID `json:"result_media_id,omitempty"`
	// ExecuteAfter is when a deletion becomes irreversible. Until then the
	// account works normally and the request can be withdrawn.
	ExecuteAfter time.Time  `json:"execute_after"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
}

// Cancellable reports whether withdrawing the request is still possible.
func (r *Request) Cancellable() bool {
	return r.Status == StatusPending || r.Status == StatusProcessing
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

const requestColumns = `id, user_id, type, status, result_media_id,
	execute_after, error, created_at, completed_at`

func scanRequest(row pgx.Row) (*Request, error) {
	request := &Request{}
	err := row.Scan(&request.ID, &request.UserID, &request.Type, &request.Status,
		&request.ResultMediaID, &request.ExecuteAfter, &request.Error,
		&request.CreatedAt, &request.CompletedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("datarights: read request: %w", err)
	}
	return request, nil
}

// Create opens a request, refusing a second one of the same kind while the
// first is still open.
//
// Two exports running at once would produce two archives of the same data for
// no benefit, and two deletions is meaningless. The check and the insert are
// in one transaction so two devices asking together cannot both get through.
func (r *Repository) Create(ctx context.Context, userID uuid.UUID, kind string, executeAfter time.Time) (*Request, error) {
	var request *Request
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var open bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM data_requests
			    WHERE user_id = $1 AND type = $2 AND status IN ('pending', 'processing')
			    FOR UPDATE)`, userID, kind).Scan(&open); err != nil {
			return fmt.Errorf("datarights: check open requests: %w", err)
		}
		if open {
			return ErrDuplicate
		}

		created, err := scanRequest(tx.QueryRow(ctx, `
			INSERT INTO data_requests (user_id, type, execute_after)
			VALUES ($1, $2, $3)
			RETURNING `+requestColumns, userID, kind, executeAfter))
		if err != nil {
			return err
		}
		request = created
		return nil
	})
	return request, err
}

// List returns the caller's own requests, newest first.
func (r *Repository) List(ctx context.Context, userID uuid.UUID, limit int) ([]Request, error) {
	rows, err := r.db.Pool.Query(ctx,
		`SELECT `+requestColumns+` FROM data_requests
		 WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("datarights: list requests: %w", err)
	}
	defer rows.Close()

	requests := []Request{}
	for rows.Next() {
		request := Request{}
		if err := rows.Scan(&request.ID, &request.UserID, &request.Type, &request.Status,
			&request.ResultMediaID, &request.ExecuteAfter, &request.Error,
			&request.CreatedAt, &request.CompletedAt); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// Cancel withdraws a request that has not yet been carried out.
//
// The status condition is what makes it safe against the worker: a request the
// worker has already finished cannot be cancelled, and one it is midway
// through is cancelled here and refused when it tries to complete.
func (r *Repository) Cancel(ctx context.Context, userID, requestID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE data_requests
		SET status = 'cancelled', completed_at = now()
		WHERE id = $1 AND user_id = $2 AND status IN ('pending', 'processing')`,
		requestID, userID)
	if err != nil {
		return fmt.Errorf("datarights: cancel request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Either it is not theirs, or it is past cancelling. Both report the
		// same way: which of the two it is, is not information an outsider
		// guessing ids should get.
		return ErrNotOpen
	}
	return nil
}

// ClaimDue takes the requests whose time has come, marking them in progress.
//
// FOR UPDATE SKIP LOCKED so several workers may run this at once and each
// request is carried out exactly once.
func (r *Repository) ClaimDue(ctx context.Context, limit int) ([]Request, error) {
	rows, err := r.db.Pool.Query(ctx, `
		UPDATE data_requests d
		   SET status = 'processing'
		 WHERE d.id IN (
		     SELECT id FROM data_requests
		      WHERE status = 'pending' AND execute_after <= now()
		      ORDER BY execute_after
		      FOR UPDATE SKIP LOCKED
		      LIMIT $1)
		RETURNING `+requestColumns, limit)
	if err != nil {
		return nil, fmt.Errorf("datarights: claim due requests: %w", err)
	}
	defer rows.Close()

	var due []Request
	for rows.Next() {
		request := Request{}
		if err := rows.Scan(&request.ID, &request.UserID, &request.Type, &request.Status,
			&request.ResultMediaID, &request.ExecuteAfter, &request.Error,
			&request.CreatedAt, &request.CompletedAt); err != nil {
			return nil, err
		}
		due = append(due, request)
	}
	return due, rows.Err()
}

// MarkReady records a finished export and the archive it produced.
func (r *Repository) MarkReady(ctx context.Context, requestID, mediaID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE data_requests
		SET status = 'ready', result_media_id = $2, completed_at = now(), error = ''
		WHERE id = $1 AND status = 'processing'`, requestID, mediaID)
	if err != nil {
		return fmt.Errorf("datarights: mark ready: %w", err)
	}
	return nil
}

// MarkCompleted records a finished deletion.
func (r *Repository) MarkCompleted(ctx context.Context, requestID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE data_requests
		SET status = 'completed', completed_at = now(), error = ''
		WHERE id = $1 AND status = 'processing'`, requestID)
	if err != nil {
		return fmt.Errorf("datarights: mark completed: %w", err)
	}
	return nil
}

// MarkFailed records why a request could not be carried out and returns it to
// pending, so a transient failure is retried rather than abandoning somebody's
// export.
func (r *Repository) MarkFailed(ctx context.Context, requestID uuid.UUID, reason string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE data_requests
		SET status = 'pending', error = $2, execute_after = now() + interval '15 minutes'
		WHERE id = $1 AND status = 'processing'`, requestID, reason)
	if err != nil {
		return fmt.Errorf("datarights: mark failed: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- service

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Request opens an export or a deletion.
func (s *Service) Request(ctx context.Context, userID uuid.UUID, kind string) (*Request, error) {
	executeAfter := time.Now()
	switch kind {
	case TypeExport:
		// An export has nothing to reconsider, so it starts at once.
	case TypeDelete:
		executeAfter = executeAfter.Add(DeletionDelay)
	default:
		return nil, httpx.Validation("Unsupported request type").
			WithField("type", "must be export or delete")
	}

	request, err := s.repo.Create(ctx, userID, kind, executeAfter)
	if err != nil {
		if errors.Is(err, ErrDuplicate) {
			return nil, httpx.Conflict(httpx.CodeConflict,
				"You already have a request of that kind in progress")
		}
		return nil, httpx.Internal(err)
	}
	return request, nil
}

// List returns the caller's requests.
func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]Request, error) {
	requests, err := s.repo.List(ctx, userID, 50)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return requests, nil
}

// Cancel withdraws one.
func (s *Service) Cancel(ctx context.Context, userID, requestID uuid.UUID) error {
	if err := s.repo.Cancel(ctx, userID, requestID); err != nil {
		if errors.Is(err, ErrNotOpen) {
			return httpx.NotFound(httpx.CodeNotFound,
				"No request of yours is waiting to be carried out")
		}
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
	r.Post("/", h.create)
	r.Delete("/{requestID}", h.cancel)
	return r
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	requests, err := h.service.List(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"requests": requests, "deletion_delay_seconds": int(DeletionDelay.Seconds()),
	})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Type string `json:"type"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	request, err := h.service.Request(r.Context(), principal.UserID, body.Type)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"request": request})
}

func (h *Handler) cancel(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	requestID, err := uuid.Parse(chi.URLParam(r, "requestID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("request id must be a UUID"))
		return
	}

	if err := h.service.Cancel(r.Context(), principal.UserID, requestID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}
