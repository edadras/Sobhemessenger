// Package polls implements in-chat polls and quizzes (§53).
//
// A poll is attached to a message: creating one produces a message of type
// `poll`, so it flows through the same sequence, sync and permission machinery
// as everything else in a chat.
package polls

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/messaging"
)

var ErrNotFound = errors.New("polls: not found")

const (
	minOptions       = 2
	maxOptions       = 12
	maxQuestionRunes = 300
	maxOptionRunes   = 100
)

// Poll is the question plus its options and the caller's own votes.
type Poll struct {
	ID             uuid.UUID   `json:"id"`
	ChatID         uuid.UUID   `json:"chat_id"`
	MessageID      *uuid.UUID  `json:"message_id,omitempty"`
	Question       string      `json:"question"`
	IsAnonymous    bool        `json:"is_anonymous"`
	AllowsMultiple bool        `json:"allows_multiple"`
	IsQuiz         bool        `json:"is_quiz"`
	CorrectOption  *int        `json:"correct_option,omitempty"`
	TotalVoters    int         `json:"total_voters"`
	ClosesAt       *time.Time  `json:"closes_at,omitempty"`
	ClosedAt       *time.Time  `json:"closed_at,omitempty"`
	CreatedAt      time.Time   `json:"created_at"`
	Options        []Option    `json:"options"`
	MyVotes        []uuid.UUID `json:"my_votes,omitempty"`
}

// Option is one answer.
type Option struct {
	ID        uuid.UUID `json:"id"`
	Position  int       `json:"position"`
	Text      string    `json:"text"`
	VoteCount int       `json:"vote_count"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateInput describes a new poll.
type CreateInput struct {
	ChatID         uuid.UUID
	CreatedBy      uuid.UUID
	Question       string
	Options        []string
	IsAnonymous    bool
	AllowsMultiple bool
	IsQuiz         bool
	CorrectOption  *int
	ClosesAt       *time.Time
}

// Create stores the poll and its options together.
func (r *Repository) Create(ctx context.Context, in CreateInput) (*Poll, error) {
	var pollID uuid.UUID

	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO polls (chat_id, created_by, question, is_anonymous,
			                   allows_multiple, is_quiz, correct_option, closes_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id`,
			in.ChatID, in.CreatedBy, in.Question, in.IsAnonymous,
			in.AllowsMultiple, in.IsQuiz, in.CorrectOption, in.ClosesAt).Scan(&pollID)
		if err != nil {
			return fmt.Errorf("polls: create: %w", err)
		}

		for position, text := range in.Options {
			if _, err := tx.Exec(ctx,
				`INSERT INTO poll_options (poll_id, position, text) VALUES ($1, $2, $3)`,
				pollID, position, text); err != nil {
				return fmt.Errorf("polls: create option: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return r.ByID(ctx, pollID, in.CreatedBy)
}

// AttachToMessage links the poll to the message that carries it.
func (r *Repository) AttachToMessage(ctx context.Context, pollID, messageID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE messages SET poll_id = $2 WHERE id = $1`, messageID, pollID)
	return err
}

// ByID loads a poll with the viewer's own votes attached.
func (r *Repository) ByID(ctx context.Context, pollID, viewerID uuid.UUID) (*Poll, error) {
	poll := &Poll{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT p.id, p.chat_id, m.id, p.question, p.is_anonymous, p.allows_multiple,
		       p.is_quiz, p.correct_option, p.total_voters, p.closes_at, p.closed_at, p.created_at
		FROM polls p
		LEFT JOIN messages m ON m.poll_id = p.id
		WHERE p.id = $1`, pollID,
	).Scan(&poll.ID, &poll.ChatID, &poll.MessageID, &poll.Question, &poll.IsAnonymous,
		&poll.AllowsMultiple, &poll.IsQuiz, &poll.CorrectOption, &poll.TotalVoters,
		&poll.ClosesAt, &poll.ClosedAt, &poll.CreatedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("polls: read: %w", err)
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, position, text, vote_count FROM poll_options
		WHERE poll_id = $1 ORDER BY position`, pollID)
	if err != nil {
		return nil, fmt.Errorf("polls: read options: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var option Option
		if err := rows.Scan(&option.ID, &option.Position, &option.Text, &option.VoteCount); err != nil {
			return nil, err
		}
		poll.Options = append(poll.Options, option)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	voteRows, err := r.db.Pool.Query(ctx,
		`SELECT option_id FROM poll_votes WHERE poll_id = $1 AND user_id = $2`, pollID, viewerID)
	if err != nil {
		return nil, fmt.Errorf("polls: read votes: %w", err)
	}
	defer voteRows.Close()

	for voteRows.Next() {
		var optionID uuid.UUID
		if err := voteRows.Scan(&optionID); err != nil {
			return nil, err
		}
		poll.MyVotes = append(poll.MyVotes, optionID)
	}
	return poll, voteRows.Err()
}

// Vote records a ballot.
//
// The whole ballot is replaced in one transaction: for a single-choice poll
// that means changing your mind moves the vote rather than adding a second
// one, and the per-option counters stay consistent with the vote rows.
func (r *Repository) Vote(ctx context.Context, pollID, userID uuid.UUID, optionIDs []uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var allowsMultiple bool
		var closedAt *time.Time
		var closesAt *time.Time
		err := tx.QueryRow(ctx,
			`SELECT allows_multiple, closed_at, closes_at FROM polls WHERE id = $1 FOR UPDATE`,
			pollID).Scan(&allowsMultiple, &closedAt, &closesAt)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("polls: lock poll: %w", err)
		}
		if closedAt != nil || (closesAt != nil && closesAt.Before(time.Now())) {
			return ErrClosed
		}
		if !allowsMultiple && len(optionIDs) > 1 {
			return ErrSingleChoice
		}

		// Every option must belong to this poll, or a caller could vote on
		// another poll's option and corrupt its tally.
		var validOptions int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM poll_options WHERE poll_id = $1 AND id = ANY($2::uuid[])`,
			pollID, optionIDs).Scan(&validOptions); err != nil {
			return fmt.Errorf("polls: validate options: %w", err)
		}
		if validOptions != len(optionIDs) {
			return ErrInvalidOption
		}

		var hadVoted bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM poll_votes WHERE poll_id = $1 AND user_id = $2)`,
			pollID, userID).Scan(&hadVoted); err != nil {
			return err
		}

		// Withdraw the previous ballot, decrementing whatever it counted for.
		if _, err := tx.Exec(ctx, `
			UPDATE poll_options o
			SET vote_count = GREATEST(0, o.vote_count - 1)
			WHERE o.id IN (SELECT option_id FROM poll_votes WHERE poll_id = $1 AND user_id = $2)`,
			pollID, userID); err != nil {
			return fmt.Errorf("polls: decrement previous votes: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`DELETE FROM poll_votes WHERE poll_id = $1 AND user_id = $2`, pollID, userID); err != nil {
			return fmt.Errorf("polls: clear previous votes: %w", err)
		}

		if len(optionIDs) == 0 {
			// An empty ballot retracts the vote entirely.
			if hadVoted {
				_, err = tx.Exec(ctx,
					`UPDATE polls SET total_voters = GREATEST(0, total_voters - 1) WHERE id = $1`,
					pollID)
			}
			return err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO poll_votes (poll_id, option_id, user_id)
			SELECT $1, unnest($2::uuid[]), $3`, pollID, optionIDs, userID); err != nil {
			return fmt.Errorf("polls: record votes: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE poll_options SET vote_count = vote_count + 1
			WHERE id = ANY($1::uuid[])`, optionIDs); err != nil {
			return fmt.Errorf("polls: increment votes: %w", err)
		}

		if !hadVoted {
			_, err = tx.Exec(ctx,
				`UPDATE polls SET total_voters = total_voters + 1 WHERE id = $1`, pollID)
		}
		return err
	})
}

var (
	ErrClosed        = errors.New("polls: poll is closed")
	ErrSingleChoice  = errors.New("polls: this poll accepts a single answer")
	ErrInvalidOption = errors.New("polls: option does not belong to this poll")
	ErrNotCreator    = errors.New("polls: only the creator can close this poll")
)

func (r *Repository) Close(ctx context.Context, pollID, actorID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE polls SET closed_at = now()
		WHERE id = $1 AND created_by = $2 AND closed_at IS NULL`, pollID, actorID)
	if err != nil {
		return fmt.Errorf("polls: close: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotCreator
	}
	return nil
}

// Voters lists who chose an option. Anonymous polls never reveal this.
func (r *Repository) Voters(ctx context.Context, pollID, optionID uuid.UUID, limit int) ([]uuid.UUID, error) {
	var isAnonymous bool
	if err := r.db.Pool.QueryRow(ctx,
		`SELECT is_anonymous FROM polls WHERE id = $1`, pollID).Scan(&isAnonymous); err != nil {
		if database.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if isAnonymous {
		return nil, ErrAnonymous
	}

	rows, err := r.db.Pool.Query(ctx, `
		SELECT user_id FROM poll_votes
		WHERE poll_id = $1 AND option_id = $2
		ORDER BY created_at LIMIT $3`, pollID, optionID, limit)
	if err != nil {
		return nil, fmt.Errorf("polls: list voters: %w", err)
	}
	defer rows.Close()

	var voters []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		voters = append(voters, id)
	}
	return voters, rows.Err()
}

var ErrAnonymous = errors.New("polls: this poll is anonymous")

// ---------------------------------------------------------------- service

type Service struct {
	repo      *Repository
	messaging *messaging.Service
}

func NewService(repo *Repository, messagingService *messaging.Service) *Service {
	return &Service{repo: repo, messaging: messagingService}
}

// Create validates the poll, then publishes it as a chat message so it appears
// in the conversation like any other post.
func (s *Service) Create(ctx context.Context, in CreateInput, clientMessageID uuid.UUID) (*Poll, error) {
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" || utf8.RuneCountInString(in.Question) > maxQuestionRunes {
		return nil, httpx.Validation("Question is not valid").
			WithField("question", fmt.Sprintf("between 1 and %d characters", maxQuestionRunes))
	}

	cleaned := make([]string, 0, len(in.Options))
	for _, option := range in.Options {
		trimmed := strings.TrimSpace(option)
		if trimmed == "" {
			continue
		}
		if utf8.RuneCountInString(trimmed) > maxOptionRunes {
			return nil, httpx.Validation("An option is too long").
				WithField("options", fmt.Sprintf("at most %d characters each", maxOptionRunes))
		}
		cleaned = append(cleaned, trimmed)
	}
	if len(cleaned) < minOptions || len(cleaned) > maxOptions {
		return nil, httpx.Validation("Provide between 2 and 12 options").
			WithField("options", "2-12 entries")
	}
	in.Options = cleaned

	if in.IsQuiz {
		if in.CorrectOption == nil || *in.CorrectOption < 0 || *in.CorrectOption >= len(cleaned) {
			return nil, httpx.Validation("A quiz needs a valid correct answer").
				WithField("correct_option", "must index one of the options")
		}
		// A quiz has exactly one right answer, so multiple choice makes no sense.
		in.AllowsMultiple = false
	}
	if in.ClosesAt != nil && in.ClosesAt.Before(time.Now()) {
		return nil, httpx.Validation("Closing time is in the past").
			WithField("closes_at", "must be in the future")
	}

	poll, err := s.repo.Create(ctx, in)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	payload, err := json.Marshal(map[string]any{"poll_id": poll.ID})
	if err != nil {
		return nil, httpx.Internal(err)
	}

	message, err := s.messaging.Send(ctx, messaging.SendInput{
		ChatID:          in.ChatID,
		SenderID:        in.CreatedBy,
		ClientMessageID: clientMessageID,
		Type:            messaging.TypePoll,
		Content:         in.Question,
		Payload:         payload,
	})
	if err != nil {
		return nil, err
	}

	if err := s.repo.AttachToMessage(ctx, poll.ID, message.ID); err != nil {
		return nil, httpx.Internal(err)
	}
	poll.MessageID = &message.ID
	return poll, nil
}

func (s *Service) Get(ctx context.Context, pollID, viewerID uuid.UUID) (*Poll, error) {
	poll, err := s.repo.ByID(ctx, pollID, viewerID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Poll not found")
		}
		return nil, httpx.Internal(err)
	}

	// A quiz hides its answer until the reader has committed to one.
	if poll.IsQuiz && len(poll.MyVotes) == 0 && poll.ClosedAt == nil {
		poll.CorrectOption = nil
	}
	return poll, nil
}

func (s *Service) Vote(ctx context.Context, pollID, userID uuid.UUID, optionIDs []uuid.UUID) (*Poll, error) {
	if len(optionIDs) > maxOptions {
		return nil, httpx.Validation("Too many options selected").
			WithField("option_ids", "at most 12 entries")
	}

	err := s.repo.Vote(ctx, pollID, userID, optionIDs)
	switch {
	case err == nil:
	case errors.Is(err, ErrNotFound):
		return nil, httpx.NotFound(httpx.CodeNotFound, "Poll not found")
	case errors.Is(err, ErrClosed):
		return nil, httpx.Conflict(httpx.CodeConflict, "This poll is closed")
	case errors.Is(err, ErrSingleChoice):
		return nil, httpx.Validation("This poll accepts a single answer").
			WithField("option_ids", "exactly one entry")
	case errors.Is(err, ErrInvalidOption):
		return nil, httpx.Validation("That option does not belong to this poll").
			WithField("option_ids", "invalid option")
	default:
		return nil, httpx.Internal(err)
	}

	return s.Get(ctx, pollID, userID)
}

func (s *Service) Close(ctx context.Context, pollID, actorID uuid.UUID) error {
	if err := s.repo.Close(ctx, pollID, actorID); err != nil {
		if errors.Is(err, ErrNotCreator) {
			return httpx.Forbidden(httpx.CodeForbidden, "Only the poll's creator can close it")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) Voters(ctx context.Context, pollID, optionID uuid.UUID) ([]uuid.UUID, error) {
	voters, err := s.repo.Voters(ctx, pollID, optionID, 500)
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			return nil, httpx.NotFound(httpx.CodeNotFound, "Poll not found")
		case errors.Is(err, ErrAnonymous):
			return nil, httpx.Forbidden(httpx.CodeForbidden, "This poll is anonymous")
		default:
			return nil, httpx.Internal(err)
		}
	}
	return voters, nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.create)
	r.Get("/{pollID}", h.get)
	r.Post("/{pollID}/vote", h.vote)
	r.Post("/{pollID}/close", h.close)
	r.Get("/{pollID}/options/{optionID}/voters", h.voters)
	return r
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		ChatID          uuid.UUID  `json:"chat_id"`
		ClientMessageID uuid.UUID  `json:"client_message_id"`
		Question        string     `json:"question"`
		Options         []string   `json:"options"`
		IsAnonymous     bool       `json:"is_anonymous"`
		AllowsMultiple  bool       `json:"allows_multiple"`
		IsQuiz          bool       `json:"is_quiz"`
		CorrectOption   *int       `json:"correct_option,omitempty"`
		ClosesAt        *time.Time `json:"closes_at,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.ClientMessageID == uuid.Nil {
		httpx.Fail(w, r, httpx.Validation("client_message_id is required").
			WithField("client_message_id", "required for idempotent delivery"))
		return
	}

	poll, err := h.service.Create(r.Context(), CreateInput{
		ChatID:         body.ChatID,
		CreatedBy:      principal.UserID,
		Question:       body.Question,
		Options:        body.Options,
		IsAnonymous:    body.IsAnonymous,
		AllowsMultiple: body.AllowsMultiple,
		IsQuiz:         body.IsQuiz,
		CorrectOption:  body.CorrectOption,
		ClosesAt:       body.ClosesAt,
	}, body.ClientMessageID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, poll)
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	principal, pollID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	poll, err := h.service.Get(r.Context(), pollID, principal.UserID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, poll)
}

func (h *Handler) vote(w http.ResponseWriter, r *http.Request) {
	principal, pollID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		OptionIDs []uuid.UUID `json:"option_ids"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	poll, err := h.service.Vote(r.Context(), pollID, principal.UserID, body.OptionIDs)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, poll)
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	principal, pollID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if err := h.service.Close(r.Context(), pollID, principal.UserID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) voters(w http.ResponseWriter, r *http.Request) {
	_, pollID, err := h.context(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	optionID, parseErr := uuid.Parse(chi.URLParam(r, "optionID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("optionID is not a valid UUID"))
		return
	}

	voters, err := h.service.Voters(r.Context(), pollID, optionID)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"voters": voters})
}

func (h *Handler) context(r *http.Request) (*httpx.Principal, uuid.UUID, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return nil, uuid.Nil, err
	}
	pollID, parseErr := uuid.Parse(chi.URLParam(r, "pollID"))
	if parseErr != nil {
		return nil, uuid.Nil, httpx.BadRequest("pollID is not a valid UUID")
	}
	return principal, pollID, nil
}
