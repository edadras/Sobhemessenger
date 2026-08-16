// Package contacts implements the address book and privacy-preserving
// discovery (§54, §55).
//
// The server never receives a raw address book. The client normalises each
// number to E.164, computes HMAC-SHA256 over it with a pepper the server
// publishes, and uploads only the truncated digests. The server matches those
// against `users.phone_hash`, which is derived the same way.
//
// This does not make discovery unlinkable — the space of phone numbers is
// small enough to enumerate given the pepper — but it does mean a stolen
// request body, proxy log or database dump contains no phone numbers, and the
// server never stores the address book of a user who is not registered.
package contacts

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/security"
)

var ErrNotFound = errors.New("contacts: not found")

// maxSyncBatch bounds one sync request. A larger address book is uploaded in
// several batches, which also keeps the rate limiter meaningful.
const maxSyncBatch = 2000

// Contact is one entry in the caller's address book.
type Contact struct {
	UserID      uuid.UUID  `json:"user_id"`
	FirstName   string     `json:"first_name,omitempty"`
	LastName    string     `json:"last_name,omitempty"`
	Username    *string    `json:"username,omitempty"`
	DisplayName string     `json:"display_name"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
	IsFavorite  bool       `json:"is_favorite"`
	IsMutual    bool       `json:"is_mutual"`
	LastSeen    *time.Time `json:"last_seen,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Match is one discovered account, keyed by the digest the client sent so it
// can be paired back to the local address-book entry without the server ever
// learning the number.
type Match struct {
	Digest      string     `json:"digest"`
	UserID      uuid.UUID  `json:"user_id"`
	Username    *string    `json:"username,omitempty"`
	DisplayName string     `json:"display_name"`
	AvatarID    *uuid.UUID `json:"avatar_media_id,omitempty"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// MatchByHash resolves phone digests to accounts.
//
// The digests are compared against the stored `phone_hash` column, so the
// query never touches `phone_number` and an index scan cannot be turned into
// a number lookup.
func (r *Repository) MatchByHash(ctx context.Context, digests [][]byte, viewerID uuid.UUID) ([]Match, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT encode(u.phone_hash, 'hex'), u.id, u.username,
		       COALESCE(p.display_name, ''), p.avatar_media_id
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		WHERE u.phone_hash = ANY($1::bytea[])
		  AND u.deleted_at IS NULL
		  AND u.status = 'active'
		  AND u.id <> $2
		  -- A user who has blocked the caller is not discoverable by them.
		  AND NOT EXISTS (
		      SELECT 1 FROM blocked_users b
		      WHERE b.owner_id = u.id AND b.blocked_id = $2)`,
		digests, viewerID)
	if err != nil {
		return nil, fmt.Errorf("contacts: match by hash: %w", err)
	}
	defer rows.Close()

	var matches []Match
	for rows.Next() {
		var match Match
		if err := rows.Scan(&match.Digest, &match.UserID, &match.Username,
			&match.DisplayName, &match.AvatarID); err != nil {
			return nil, err
		}
		matches = append(matches, match)
	}
	return matches, rows.Err()
}

// ReplaceContacts rewrites the caller's contact list from a matched set.
//
// It is a full replacement rather than a merge: the address book on the device
// is the source of truth, and a number removed there must disappear here too.
func (r *Repository) ReplaceContacts(ctx context.Context, ownerID uuid.UUID, entries []ContactUpsert) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		ids := make([]uuid.UUID, 0, len(entries))
		for _, entry := range entries {
			ids = append(ids, entry.UserID)
		}

		// Favourites are a server-side flag the client does not send, so the
		// delete spares them by only removing entries absent from the batch.
		if _, err := tx.Exec(ctx, `
			DELETE FROM contacts
			WHERE owner_id = $1 AND NOT (contact_id = ANY($2::uuid[]))`,
			ownerID, ids); err != nil {
			return fmt.Errorf("contacts: prune contacts: %w", err)
		}

		for _, entry := range entries {
			if entry.UserID == ownerID {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO contacts (owner_id, contact_id, first_name, last_name)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (owner_id, contact_id) DO UPDATE
				SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name`,
				ownerID, entry.UserID, entry.FirstName, entry.LastName); err != nil {
				return fmt.Errorf("contacts: upsert contact: %w", err)
			}
		}
		return nil
	})
}

// ContactUpsert is one matched address-book entry.
type ContactUpsert struct {
	UserID    uuid.UUID
	FirstName string
	LastName  string
}

// List returns the caller's contacts, flagging which ones have added them back.
func (r *Repository) List(ctx context.Context, ownerID uuid.UUID) ([]Contact, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT c.contact_id, c.first_name, c.last_name, u.username,
		       COALESCE(p.display_name, ''), p.avatar_media_id, c.is_favorite,
		       EXISTS (
		           SELECT 1 FROM contacts back
		           WHERE back.owner_id = c.contact_id AND back.contact_id = $1
		       ) AS is_mutual,
		       -- Last seen is only revealed when the peer's privacy allows it.
		       CASE WHEN visible_last_seen(c.contact_id, $1) THEN u.last_seen_at END,
		       c.created_at
		FROM contacts c
		JOIN users u ON u.id = c.contact_id AND u.deleted_at IS NULL
		LEFT JOIN user_profiles p ON p.user_id = c.contact_id
		WHERE c.owner_id = $1
		ORDER BY c.is_favorite DESC, COALESCE(NULLIF(c.first_name, ''), p.display_name)`,
		ownerID)
	if err != nil {
		return nil, fmt.Errorf("contacts: list: %w", err)
	}
	defer rows.Close()

	var contacts []Contact
	for rows.Next() {
		var contact Contact
		if err := rows.Scan(&contact.UserID, &contact.FirstName, &contact.LastName,
			&contact.Username, &contact.DisplayName, &contact.AvatarID,
			&contact.IsFavorite, &contact.IsMutual, &contact.LastSeen,
			&contact.CreatedAt); err != nil {
			return nil, err
		}
		contacts = append(contacts, contact)
	}
	return contacts, rows.Err()
}

func (r *Repository) Add(ctx context.Context, ownerID, contactID uuid.UUID, firstName, lastName string) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO contacts (owner_id, contact_id, first_name, last_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (owner_id, contact_id) DO UPDATE
		SET first_name = EXCLUDED.first_name, last_name = EXCLUDED.last_name`,
		ownerID, contactID, firstName, lastName)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("contacts: add: %w", err)
	}
	return nil
}

func (r *Repository) Remove(ctx context.Context, ownerID, contactID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM contacts WHERE owner_id = $1 AND contact_id = $2`, ownerID, contactID)
	if err != nil {
		return fmt.Errorf("contacts: remove: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) SetFavorite(ctx context.Context, ownerID, contactID uuid.UUID, favorite bool) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE contacts SET is_favorite = $3 WHERE owner_id = $1 AND contact_id = $2`,
		ownerID, contactID, favorite)
	if err != nil {
		return fmt.Errorf("contacts: set favourite: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Block prevents a user from contacting the caller and removes them from both
// address books.
func (r *Repository) Block(ctx context.Context, ownerID, blockedID uuid.UUID, reason string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO blocked_users (owner_id, blocked_id, reason)
			VALUES ($1, $2, $3)
			ON CONFLICT (owner_id, blocked_id) DO UPDATE SET reason = EXCLUDED.reason`,
			ownerID, blockedID, reason); err != nil {
			if database.IsCheckViolation(err) {
				return ErrCannotBlockSelf
			}
			return fmt.Errorf("contacts: block: %w", err)
		}

		// Blocking implies removing the contact in both directions: leaving the
		// entry behind would keep showing a person the user has cut off.
		_, err := tx.Exec(ctx, `
			DELETE FROM contacts
			WHERE (owner_id = $1 AND contact_id = $2) OR (owner_id = $2 AND contact_id = $1)`,
			ownerID, blockedID)
		return err
	})
}

var ErrCannotBlockSelf = errors.New("contacts: you cannot block yourself")

func (r *Repository) Unblock(ctx context.Context, ownerID, blockedID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM blocked_users WHERE owner_id = $1 AND blocked_id = $2`, ownerID, blockedID)
	if err != nil {
		return fmt.Errorf("contacts: unblock: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) Blocked(ctx context.Context, ownerID uuid.UUID) ([]Contact, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT b.blocked_id, '', '', u.username, COALESCE(p.display_name, ''),
		       p.avatar_media_id, FALSE, FALSE, NULL::timestamptz, b.created_at
		FROM blocked_users b
		JOIN users u ON u.id = b.blocked_id
		LEFT JOIN user_profiles p ON p.user_id = b.blocked_id
		WHERE b.owner_id = $1
		ORDER BY b.created_at DESC`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("contacts: list blocked: %w", err)
	}
	defer rows.Close()

	var blocked []Contact
	for rows.Next() {
		var contact Contact
		if err := rows.Scan(&contact.UserID, &contact.FirstName, &contact.LastName,
			&contact.Username, &contact.DisplayName, &contact.AvatarID,
			&contact.IsFavorite, &contact.IsMutual, &contact.LastSeen,
			&contact.CreatedAt); err != nil {
			return nil, err
		}
		blocked = append(blocked, contact)
	}
	return blocked, rows.Err()
}

// IsBlocked reports whether either party has blocked the other, which is what
// messaging and calls check before connecting two people.
func (r *Repository) IsBlocked(ctx context.Context, a, b uuid.UUID) (bool, error) {
	var blocked bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM blocked_users
		    WHERE (owner_id = $1 AND blocked_id = $2)
		       OR (owner_id = $2 AND blocked_id = $1))`, a, b).Scan(&blocked)
	return blocked, err
}

// ---------------------------------------------------------------- service

type Service struct {
	repo    *Repository
	limiter *ratelimit.Limiter
	rules   ratelimit.Rules
	cfg     config.Auth
}

func NewService(repo *Repository, limiter *ratelimit.Limiter, rules ratelimit.Rules, cfg config.Auth) *Service {
	return &Service{repo: repo, limiter: limiter, rules: rules, cfg: cfg}
}

// DiscoveryParameters tell the client how to derive the digests it uploads.
//
// The pepper is served to authenticated clients rather than embedded in the
// app, so it can be rotated without shipping a release — and so it is not
// sitting in a decompiled APK for anyone who never registered.
type DiscoveryParameters struct {
	Algorithm    string `json:"algorithm"`
	Pepper       string `json:"pepper"`
	DigestLength int    `json:"digest_length"`
	MaxBatchSize int    `json:"max_batch_size"`
}

func (s *Service) DiscoveryParameters() DiscoveryParameters {
	return DiscoveryParameters{
		Algorithm:    "HMAC-SHA256",
		Pepper:       string(s.cfg.PhoneHashPepper),
		DigestLength: 32,
		MaxBatchSize: maxSyncBatch,
	}
}

// SyncInput is one batch of hashed address-book entries.
type SyncInput struct {
	UserID  uuid.UUID
	Entries []SyncEntry
	// Replace is true for a full sync; a partial batch only adds.
	Replace bool
}

// SyncEntry is one hashed number with the local name the client shows for it.
type SyncEntry struct {
	Digest    string `json:"digest"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// SyncResult reports which entries resolved to accounts.
type SyncResult struct {
	Matched  []Match `json:"matched"`
	Uploaded int     `json:"uploaded"`
}

// Sync matches a batch of digests and updates the caller's contact list.
func (s *Service) Sync(ctx context.Context, in SyncInput) (*SyncResult, error) {
	if len(in.Entries) == 0 {
		return &SyncResult{Matched: []Match{}}, nil
	}
	if len(in.Entries) > maxSyncBatch {
		return nil, httpx.Validation("Too many entries in one batch").
			WithField("entries", fmt.Sprintf("at most %d per request", maxSyncBatch))
	}

	// The limiter is absent only where there is no Redis to hold the counters —
	// the integration suite. Every path that serves a request has one.
	if s.limiter != nil {
		allowed, err := s.limiter.Allow(ctx, s.rules.ContactSync, in.UserID.String())
		if err == nil && !allowed.Allowed {
			return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
		}
	}

	// Decode and de-duplicate. A malformed digest fails the whole batch rather
	// than being skipped: a client sending junk has a bug worth surfacing.
	digests := make([][]byte, 0, len(in.Entries))
	names := make(map[string]SyncEntry, len(in.Entries))
	for _, entry := range in.Entries {
		raw, decodeErr := hex.DecodeString(strings.TrimSpace(entry.Digest))
		if decodeErr != nil || len(raw) != 32 {
			return nil, httpx.Validation("A digest is not a 32-byte hex string").
				WithField("digest", "must be hex-encoded HMAC-SHA256")
		}
		key := strings.ToLower(entry.Digest)
		if _, seen := names[key]; seen {
			continue
		}
		names[key] = entry
		digests = append(digests, raw)
	}

	matches, err := s.repo.MatchByHash(ctx, digests, in.UserID)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	upserts := make([]ContactUpsert, 0, len(matches))
	for i := range matches {
		entry := names[strings.ToLower(matches[i].Digest)]
		upserts = append(upserts, ContactUpsert{
			UserID:    matches[i].UserID,
			FirstName: truncate(entry.FirstName, 100),
			LastName:  truncate(entry.LastName, 100),
		})
	}

	if in.Replace {
		if err := s.repo.ReplaceContacts(ctx, in.UserID, upserts); err != nil {
			return nil, httpx.Internal(err)
		}
	} else {
		for _, upsert := range upserts {
			if err := s.repo.Add(ctx, in.UserID, upsert.UserID,
				upsert.FirstName, upsert.LastName); err != nil && !errors.Is(err, ErrNotFound) {
				return nil, httpx.Internal(err)
			}
		}
	}

	if matches == nil {
		matches = []Match{}
	}
	return &SyncResult{Matched: matches, Uploaded: len(digests)}, nil
}

func (s *Service) List(ctx context.Context, ownerID uuid.UUID) ([]Contact, error) {
	contacts, err := s.repo.List(ctx, ownerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return contacts, nil
}

func (s *Service) Add(ctx context.Context, ownerID, contactID uuid.UUID, firstName, lastName string) error {
	if ownerID == contactID {
		return httpx.Validation("You cannot add yourself").WithField("user_id", "cannot be yourself")
	}
	if err := s.repo.Add(ctx, ownerID, contactID, truncate(firstName, 100), truncate(lastName, 100)); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "User not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Remove(ctx context.Context, ownerID, contactID uuid.UUID) error {
	if err := s.repo.Remove(ctx, ownerID, contactID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That person is not in your contacts")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) SetFavorite(ctx context.Context, ownerID, contactID uuid.UUID, favorite bool) error {
	if err := s.repo.SetFavorite(ctx, ownerID, contactID, favorite); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That person is not in your contacts")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Block(ctx context.Context, ownerID, blockedID uuid.UUID, reason string) error {
	if ownerID == blockedID {
		return httpx.Validation("You cannot block yourself").WithField("user_id", "cannot be yourself")
	}
	if err := s.repo.Block(ctx, ownerID, blockedID, truncate(reason, 500)); err != nil {
		if errors.Is(err, ErrCannotBlockSelf) {
			return httpx.Validation("You cannot block yourself").WithField("user_id", "cannot be yourself")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Unblock(ctx context.Context, ownerID, blockedID uuid.UUID) error {
	if err := s.repo.Unblock(ctx, ownerID, blockedID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That user is not blocked")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Blocked(ctx context.Context, ownerID uuid.UUID) ([]Contact, error) {
	blocked, err := s.repo.Blocked(ctx, ownerID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return blocked, nil
}

// DigestFor is the server-side equivalent of what the client computes. It
// exists so tests and the registration path derive the value identically.
func DigestFor(phone string, pepper []byte) string {
	return hex.EncodeToString(security.HashPhone(phone, pepper))
}

func truncate(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max])
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.list)
	r.Get("/discovery-parameters", h.discoveryParameters)
	r.Post("/sync", h.sync)
	r.Post("/", h.add)
	r.Delete("/{userID}", h.remove)
	r.Put("/{userID}/favorite", h.setFavorite)
	r.Get("/blocked", h.blocked)
	r.Post("/blocked", h.block)
	r.Delete("/blocked/{userID}", h.unblock)
	return r
}

func (h *Handler) discoveryParameters(w http.ResponseWriter, r *http.Request) {
	if _, err := httpx.MustPrincipal(r.Context()); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, h.service.DiscoveryParameters())
}

func (h *Handler) sync(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Entries []SyncEntry `json:"entries"`
		Replace bool        `json:"replace"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	result, err := h.service.Sync(r.Context(), SyncInput{
		UserID:  principal.UserID,
		Entries: body.Entries,
		Replace: body.Replace,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, result)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	contacts, err := h.service.List(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"contacts": contacts})
}

func (h *Handler) add(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserID    uuid.UUID `json:"user_id"`
		FirstName string    `json:"first_name"`
		LastName  string    `json:"last_name"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.Add(r.Context(), principal.UserID, body.UserID,
		body.FirstName, body.LastName); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request) {
	principal, contactID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Remove(r.Context(), principal.UserID, contactID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setFavorite(w http.ResponseWriter, r *http.Request) {
	principal, contactID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Favorite bool `json:"favorite"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetFavorite(r.Context(), principal.UserID, contactID, body.Favorite); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) blocked(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	blocked, err := h.service.Blocked(r.Context(), principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"blocked": blocked})
}

func (h *Handler) block(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserID uuid.UUID `json:"user_id"`
		Reason string    `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.Block(r.Context(), principal.UserID, body.UserID, body.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) unblock(w http.ResponseWriter, r *http.Request) {
	principal, blockedID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Unblock(r.Context(), principal.UserID, blockedID); err != nil {
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
	userID, parseErr := uuid.Parse(chi.URLParam(r, "userID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("userID is not a valid UUID")
	}
	return principal, userID, nil
}
