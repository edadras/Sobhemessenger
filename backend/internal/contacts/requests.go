package contacts

// Contact requests (§54).
//
// `contacts` is a one-sided address book: adding someone is a note to
// yourself and tells them nothing. A contact request is the other half — it
// asks someone to know you back, and on acceptance both address books gain an
// entry at once.
//
// The table has been in the schema since migration 0002 with nothing writing
// to it. What follows is the code it was cut for.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// maxRequestMessage bounds the note attached to a request. It is short on
// purpose: a request people cannot reply to is not a messaging channel, and a
// long one would be a way to message someone who has not agreed to hear from
// you.
const maxRequestMessage = 280

var (
	ErrRequestNotFound  = errors.New("contacts: no such contact request")
	ErrAlreadyContacts  = errors.New("contacts: already in each other's contacts")
	ErrRequestBlocked   = errors.New("contacts: blocked")
	ErrRequestToSelf    = errors.New("contacts: you cannot send yourself a contact request")
	ErrRequestNotYours  = errors.New("contacts: that request is not yours to resolve")
	ErrRequestResolved  = errors.New("contacts: that request has already been resolved")
	ErrRequestUnwelcome = errors.New("contacts: that person does not accept contact requests")
)

// Request is one pending or resolved contact request.
type Request struct {
	ID          uuid.UUID  `json:"id"`
	RequesterID uuid.UUID  `json:"requester_id"`
	TargetID    uuid.UUID  `json:"target_id"`
	Status      string     `json:"status"`
	Message     string     `json:"message,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	// The other party's profile, so a list of requests renders without a
	// second round trip per row.
	DisplayName string     `json:"display_name"`
	Username    *string    `json:"username,omitempty"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
}

// ------------------------------------------------------------- repository

// CreateRequest opens a request, or returns the one already open.
//
// Re-sending is deliberately idempotent rather than an error: a client that
// retries a timed-out call, or a person who taps twice, should end up with the
// one request they meant to send.
func (r *Repository) CreateRequest(ctx context.Context, requesterID, targetID uuid.UUID, message string) (*Request, error) {
	if requesterID == targetID {
		return nil, ErrRequestToSelf
	}

	request := &Request{}
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var blocked bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM blocked_users
			    WHERE (owner_id = $1 AND blocked_id = $2)
			       OR (owner_id = $2 AND blocked_id = $1))`,
			requesterID, targetID).Scan(&blocked); err != nil {
			return fmt.Errorf("contacts: check block: %w", err)
		}
		if blocked {
			return ErrRequestBlocked
		}

		// Already mutual: there is nothing to ask for.
		var mutual bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM contacts WHERE owner_id = $1 AND contact_id = $2)
			   AND EXISTS (SELECT 1 FROM contacts WHERE owner_id = $2 AND contact_id = $1)`,
			requesterID, targetID).Scan(&mutual); err != nil {
			return fmt.Errorf("contacts: check mutual: %w", err)
		}
		if mutual {
			return ErrAlreadyContacts
		}

		// The privacy rule that governs who may reach you at all governs this
		// too: a request is an unsolicited approach, and someone who has shut
		// those off has said so already.
		var welcome bool
		if err := tx.QueryRow(ctx,
			`SELECT privacy_allows($1, $2, 'messages')`,
			targetID, requesterID).Scan(&welcome); err != nil {
			return fmt.Errorf("contacts: check privacy: %w", err)
		}
		if !welcome {
			return ErrRequestUnwelcome
		}

		err := tx.QueryRow(ctx, `
			INSERT INTO contact_requests (requester_id, target_id, message)
			VALUES ($1, $2, $3)
			ON CONFLICT (requester_id, target_id) WHERE status = 'pending'
			DO UPDATE SET message = EXCLUDED.message
			RETURNING id, requester_id, target_id, status, message, created_at, resolved_at`,
			requesterID, targetID, message,
		).Scan(&request.ID, &request.RequesterID, &request.TargetID, &request.Status,
			&request.Message, &request.CreatedAt, &request.ResolvedAt)
		if err != nil {
			if database.IsForeignKeyViolation(err) {
				return ErrNotFound
			}
			return fmt.Errorf("contacts: create request: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

// Requests lists one direction of the caller's requests.
//
// Incoming is limited to those still pending — a resolved incoming request is
// not something anyone needs to act on — while outgoing keeps its history, so
// the sender can see that what they asked for was declined rather than
// wondering whether it ever arrived.
func (r *Repository) Requests(ctx context.Context, userID uuid.UUID, incoming bool, limit int) ([]Request, error) {
	query := `
		SELECT r.id, r.requester_id, r.target_id, r.status, r.message,
		       r.created_at, r.resolved_at,
		       COALESCE(NULLIF(p.display_name, ''), u.username, '') AS display_name,
		       u.username, p.avatar_media_id
		FROM contact_requests r
		JOIN users u ON u.id = CASE WHEN $2::boolean THEN r.requester_id ELSE r.target_id END
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE (CASE WHEN $2::boolean THEN r.target_id ELSE r.requester_id END) = $1
		  AND u.deleted_at IS NULL
		  AND ($2::boolean IS FALSE OR r.status = 'pending')
		ORDER BY r.created_at DESC
		LIMIT $3`

	rows, err := r.db.Pool.Query(ctx, query, userID, incoming, limit)
	if err != nil {
		return nil, fmt.Errorf("contacts: list requests: %w", err)
	}
	defer rows.Close()

	requests := []Request{}
	for rows.Next() {
		var request Request
		if err := rows.Scan(&request.ID, &request.RequesterID, &request.TargetID,
			&request.Status, &request.Message, &request.CreatedAt, &request.ResolvedAt,
			&request.DisplayName, &request.Username, &request.AvatarID); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

// ResolveRequest accepts, rejects or cancels a request.
//
// Accepting writes both address-book entries in the same transaction as the
// status change: a request that reported success but left one side without the
// other would be worse than a failure, because nobody would go looking for it.
func (r *Repository) ResolveRequest(ctx context.Context, requestID, actorID uuid.UUID, status string) (*Request, error) {
	request := &Request{}
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var currentStatus string
		err := tx.QueryRow(ctx, `
			SELECT id, requester_id, target_id, status, message, created_at
			FROM contact_requests WHERE id = $1 FOR UPDATE`, requestID,
		).Scan(&request.ID, &request.RequesterID, &request.TargetID,
			&currentStatus, &request.Message, &request.CreatedAt)
		if database.IsNoRows(err) {
			return ErrRequestNotFound
		}
		if err != nil {
			return fmt.Errorf("contacts: lock request: %w", err)
		}
		if currentStatus != "pending" {
			return ErrRequestResolved
		}

		// Only the person a request was sent to may accept or reject it, and
		// only the sender may cancel it.
		switch status {
		case "accepted", "rejected":
			if actorID != request.TargetID {
				return ErrRequestNotYours
			}
		case "cancelled":
			if actorID != request.RequesterID {
				return ErrRequestNotYours
			}
		default:
			return fmt.Errorf("contacts: unknown resolution %q", status)
		}

		if err := tx.QueryRow(ctx, `
			UPDATE contact_requests
			SET status = $2, resolved_at = now()
			WHERE id = $1
			RETURNING status, resolved_at`, requestID, status,
		).Scan(&request.Status, &request.ResolvedAt); err != nil {
			return fmt.Errorf("contacts: resolve request: %w", err)
		}

		if status != "accepted" {
			return nil
		}

		// Accepting is what makes the relationship mutual. Names are left
		// empty: the display name is resolved from the profile, and inventing
		// an address-book alias on someone's behalf is not this code's job.
		if _, err := tx.Exec(ctx, `
			INSERT INTO contacts (owner_id, contact_id)
			VALUES ($1, $2), ($2, $1)
			ON CONFLICT (owner_id, contact_id) DO NOTHING`,
			request.RequesterID, request.TargetID); err != nil {
			return fmt.Errorf("contacts: add mutual contacts: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return request, nil
}

// ---------------------------------------------------------------- service

// SendRequest asks someone to become a mutual contact.
func (s *Service) SendRequest(ctx context.Context, requesterID, targetID uuid.UUID, message string) (*Request, error) {
	if targetID == uuid.Nil {
		return nil, httpx.Validation("A user is required").WithField("user_id", "required")
	}

	// Rate-limited on the same budget as the rest of the API rather than a
	// bespoke one: unsolicited requests are exactly the traffic a limiter is
	// for, and a separate allowance would be a separate thing to get wrong.
	if s.limiter != nil {
		allowed, err := s.limiter.Allow(ctx, s.rules.APIPerUser, "contact-request:"+requesterID.String())
		if err == nil && !allowed.Allowed {
			return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
		}
	}

	request, err := s.repo.CreateRequest(ctx, requesterID, targetID,
		truncate(strings.TrimSpace(message), maxRequestMessage))
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestToSelf):
			return nil, httpx.Validation("You cannot send yourself a contact request").
				WithField("user_id", "cannot be yourself")
		case errors.Is(err, ErrAlreadyContacts):
			return nil, httpx.Conflict(httpx.CodeConflict, "You are already contacts")
		case errors.Is(err, ErrRequestBlocked), errors.Is(err, ErrRequestUnwelcome):
			// One answer for both, so a request cannot be used to discover
			// that a particular person has blocked you.
			return nil, httpx.Forbidden(httpx.CodeForbidden,
				"That person does not accept contact requests")
		case errors.Is(err, ErrNotFound):
			return nil, httpx.NotFound(httpx.CodeNotFound, "User not found")
		}
		return nil, httpx.Internal(err)
	}
	return request, nil
}

// Requests lists the caller's incoming or outgoing requests.
func (s *Service) Requests(ctx context.Context, userID uuid.UUID, direction string, limit int) ([]Request, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	incoming := direction != "outgoing"

	requests, err := s.repo.Requests(ctx, userID, incoming, limit)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return requests, nil
}

// ResolveRequest accepts, rejects or cancels a request on the caller's behalf.
func (s *Service) ResolveRequest(ctx context.Context, requestID, actorID uuid.UUID, status string) (*Request, error) {
	request, err := s.repo.ResolveRequest(ctx, requestID, actorID, status)
	if err != nil {
		switch {
		case errors.Is(err, ErrRequestNotFound), errors.Is(err, ErrRequestNotYours):
			// Not-yours reports as not-found: telling someone a request they
			// have no part in exists would leak that two other people are in
			// touch.
			return nil, httpx.NotFound(httpx.CodeNotFound, "No such contact request")
		case errors.Is(err, ErrRequestResolved):
			return nil, httpx.Conflict(httpx.CodeConflict,
				"That request has already been resolved")
		}
		return nil, httpx.Internal(err)
	}
	return request, nil
}
