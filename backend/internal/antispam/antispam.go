// Package antispam keeps a rolling score per subject and restricts the ones
// that earn it (§34).
//
// `spam_scores` has been in the schema since migration 0008 and unwritten
// since. It is not a rate limiter and does not replace one: the limiter
// answers "too fast?" in a sliding window and forgets, while this answers "has
// this account been behaving like a spammer?" over days, from evidence that
// arrives long after the message did — a report filed an hour later, a block
// from someone who never replied.
//
// Three properties matter and are what the tests pin down:
//
//   - The score decays. A ratchet that only rises would eventually restrict
//     every long-lived account, and an account that has behaved for a month
//     should not still be paying for a bad week.
//   - A restriction expires by itself. Nothing here is a ban; bans are a
//     moderator's decision and live in `bans`.
//   - The evidence is weighted by what it is. A report is a deliberate act by
//     one person; being blocked is weaker and much more common.
package antispam

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

// Subject types, matching the CHECK constraint on the table.
const (
	SubjectUser   = "user"
	SubjectIP     = "ip"
	SubjectDevice = "device"
	SubjectPhone  = "phone"
)

// Signal weights.
//
// The numbers are deliberately coarse. Their ratios are the claim being made —
// a report counts for three blocks, a rate-limit trip for almost nothing on
// its own — and pretending to more precision than that would invite tuning
// them as if they meant something exact.
const (
	// WeightReport is a moderation report naming this subject.
	WeightReport = 30
	// WeightBlock is one person blocking this subject.
	WeightBlock = 10
	// WeightRateLimit is a send refused by the rate limiter. Nearly free on
	// its own: a person on a bad connection retrying is not a spammer, and it
	// takes many of these to matter beside one report.
	WeightRateLimit = 2

	// RestrictThreshold is where a subject stops being able to start new
	// conversations.
	RestrictThreshold = 100
	// RestrictFor is how long that lasts before it lapses on its own.
	RestrictFor = 24 * time.Hour

	// DecayPerDay is how much a score sheds each day without new evidence.
	DecayPerDay = 20
)

// ErrRestricted is returned by Check for a subject currently held back.
var ErrRestricted = errors.New("antispam: this account is temporarily restricted")

// Score is what the table holds for one subject.
type Score struct {
	SubjectType     string     `json:"subject_type"`
	SubjectKey      string     `json:"subject_key"`
	Score           int        `json:"score"`
	Reason          string     `json:"reason,omitempty"`
	RestrictedUntil *time.Time `json:"restricted_until,omitempty"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// Restricted reports whether the restriction is in force right now.
func (s *Score) Restricted() bool {
	return s.RestrictedUntil != nil && s.RestrictedUntil.After(time.Now())
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Record adds to a subject's score and applies a restriction if it crosses the
// threshold.
//
// The whole thing is one statement so two signals arriving together cannot
// read the same score and each write their own increment over it.
func (r *Repository) Record(ctx context.Context, subjectType, subjectKey string, weight int, reason string) (*Score, error) {
	score := &Score{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO spam_scores (subject_type, subject_key, score, reason, restricted_until)
		VALUES ($1, $2, $3::int, $4,
		        CASE WHEN $3::int >= $5::int THEN now() + $6::interval ELSE NULL END)
		ON CONFLICT (subject_type, subject_key) DO UPDATE
		SET score = spam_scores.score + EXCLUDED.score,
		    reason = EXCLUDED.reason,
		    -- An existing restriction is never shortened by a new signal: the
		    -- later of the two wins, so more bad behaviour cannot buy an
		    -- earlier release.
		    restricted_until = CASE
		        WHEN spam_scores.score + EXCLUDED.score >= $5::int
		        THEN GREATEST(COALESCE(spam_scores.restricted_until, now()), now() + $6::interval)
		        ELSE spam_scores.restricted_until
		    END,
		    updated_at = now()
		RETURNING subject_type, subject_key, score, reason, restricted_until, updated_at`,
		subjectType, subjectKey, weight, reason, RestrictThreshold, RestrictFor.String(),
	).Scan(&score.SubjectType, &score.SubjectKey, &score.Score, &score.Reason,
		&score.RestrictedUntil, &score.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("antispam: record signal: %w", err)
	}
	return score, nil
}

// Get reads a subject's score, or nil when it has none.
func (r *Repository) Get(ctx context.Context, subjectType, subjectKey string) (*Score, error) {
	score := &Score{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT subject_type, subject_key, score, reason, restricted_until, updated_at
		FROM spam_scores WHERE subject_type = $1 AND subject_key = $2`,
		subjectType, subjectKey,
	).Scan(&score.SubjectType, &score.SubjectKey, &score.Score, &score.Reason,
		&score.RestrictedUntil, &score.UpdatedAt)
	if database.IsNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("antispam: read score: %w", err)
	}
	return score, nil
}

// Clear removes a subject's score entirely. A moderator dismissing the
// reports that caused it should not leave the account serving the sentence.
func (r *Repository) Clear(ctx context.Context, subjectType, subjectKey string) error {
	_, err := r.db.Pool.Exec(ctx,
		`DELETE FROM spam_scores WHERE subject_type = $1 AND subject_key = $2`,
		subjectType, subjectKey)
	if err != nil {
		return fmt.Errorf("antispam: clear score: %w", err)
	}
	return nil
}

// Decay sheds score with the passage of time and drops rows that reach zero.
//
// Elapsed time is measured from `updated_at`, so a subject that has been quiet
// for a week sheds a week's worth on the next sweep rather than one day's —
// the result does not depend on how often the worker happens to run.
func (r *Repository) Decay(ctx context.Context, perDay int) (int64, error) {
	var dropped int64
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		// Two statements rather than one data-modifying CTE. A CTE that
		// updated the score and deleted the rows it had just zeroed would
		// have both operate on the same snapshot, and the delete would
		// silently skip every row the update had touched — which is exactly
		// the set it was meant to remove.
		if _, err := tx.Exec(ctx, `
			UPDATE spam_scores
			SET score = GREATEST(0, score - (
			        EXTRACT(EPOCH FROM (now() - updated_at)) / 86400.0 * $1::int)::int),
			    updated_at = now()
			WHERE score > 0 AND updated_at < now() - interval '1 hour'`, perDay); err != nil {
			return fmt.Errorf("antispam: decay scores: %w", err)
		}

		tag, err := tx.Exec(ctx, `
			DELETE FROM spam_scores
			WHERE score = 0
			  -- A lapsed restriction is not worth a row; one still in force
			  -- is, or dropping the row would release the subject early.
			  AND (restricted_until IS NULL OR restricted_until < now())`)
		if err != nil {
			return fmt.Errorf("antispam: drop spent scores: %w", err)
		}
		dropped = tag.RowsAffected()
		return nil
	})
	return dropped, err
}

// ---------------------------------------------------------------- service

// Service is the façade the rest of the server uses. It exists so callers
// record a *reason* — "this user was reported" — rather than a number, which
// keeps the weights in one place where they can be reasoned about together.
type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

// Reported records a moderation report against a user.
func (s *Service) Reported(ctx context.Context, userID uuid.UUID, reason string) {
	s.record(ctx, SubjectUser, userID.String(), WeightReport, "reported: "+reason)
}

// Blocked records one person blocking another.
func (s *Service) Blocked(ctx context.Context, userID uuid.UUID) {
	s.record(ctx, SubjectUser, userID.String(), WeightBlock, "blocked by a user")
}

// RateLimited records a send the limiter refused.
func (s *Service) RateLimited(ctx context.Context, userID uuid.UUID) {
	s.record(ctx, SubjectUser, userID.String(), WeightRateLimit, "sending too fast")
}

// record swallows its error on purpose. Scoring is evidence-gathering beside
// the action that produced it; a failure to write it must not fail the report,
// the block or the send that was already refused.
func (s *Service) record(ctx context.Context, subjectType, subjectKey string, weight int, reason string) {
	if s == nil {
		return
	}
	_, _ = s.repo.Record(ctx, subjectType, subjectKey, weight, reason)
}

// Check reports whether a user may currently start new conversations.
//
// A nil service allows everything, so a deployment that has not wired the
// module in behaves exactly as it did before rather than refusing traffic.
func (s *Service) Check(ctx context.Context, userID uuid.UUID) (*Score, error) {
	if s == nil {
		return nil, nil
	}
	score, err := s.repo.Get(ctx, SubjectUser, userID.String())
	if err != nil {
		// A scoring table that cannot be read is not a reason to stop
		// delivering messages.
		return nil, nil
	}
	if score != nil && score.Restricted() {
		return score, ErrRestricted
	}
	return score, nil
}

// Restricted satisfies messaging.SpamGuard.
func (s *Service) Restricted(ctx context.Context, userID uuid.UUID) (bool, time.Time) {
	if s == nil {
		return false, time.Time{}
	}
	score, err := s.repo.Get(ctx, SubjectUser, userID.String())
	if err != nil || score == nil || !score.Restricted() {
		return false, time.Time{}
	}
	return true, *score.RestrictedUntil
}

// Lift clears a subject's score and any restriction with it.
func (s *Service) Lift(ctx context.Context, userID uuid.UUID) error {
	return s.repo.Clear(ctx, SubjectUser, userID.String())
}

// Score reads one subject's standing, for the admin panel.
func (s *Service) Score(ctx context.Context, userID uuid.UUID) (*Score, error) {
	return s.repo.Get(ctx, SubjectUser, userID.String())
}

// ---------------------------------------------------------------- handler

// Handler exposes the score to moderators, under /admin.
//
// Read and lift live here rather than in the admin package because the
// anti-spam module owns the weights and the meaning of a restriction; admin
// would otherwise have to import the types to render them, which is the
// dependency this module was built without.
type Handler struct {
	service *Service
	// requirePermission is the auth middleware's gate, injected so this
	// package does not depend on the auth package.
	requirePermission func(permission string) func(http.Handler) http.Handler
}

func NewHandler(service *Service, requirePermission func(string) func(http.Handler) http.Handler) *Handler {
	return &Handler{service: service, requirePermission: requirePermission}
}

// Routes are mounted under the operator prefix.
//
// Reading a score is a moderation read and lifting one is a moderation write,
// so they reuse the reports permissions rather than inventing a pair nobody
// has been granted.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.With(h.requirePermission("reports.read")).Get("/{userID}", h.score)
	r.With(h.requirePermission("reports.write")).Delete("/{userID}", h.lift)
	return r
}

func (h *Handler) score(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}

	score, err := h.service.Score(r.Context(), userID)
	if err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	// A subject with no score is not an error: it is the ordinary state of
	// almost every account, and the panel needs a shape to render either way.
	if score == nil {
		score = &Score{SubjectType: SubjectUser, SubjectKey: userID.String()}
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"score":      score,
		"restricted": score.Restricted(),
		"threshold":  RestrictThreshold,
	})
}

func (h *Handler) lift(w http.ResponseWriter, r *http.Request) {
	userID, err := uuid.Parse(chi.URLParam(r, "userID"))
	if err != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}
	if err := h.service.Lift(r.Context(), userID); err != nil {
		httpx.Fail(w, r, httpx.Internal(err))
		return
	}
	httpx.NoContent(w, r)
}
