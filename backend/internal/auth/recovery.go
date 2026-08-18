package auth

// Email account recovery (§4).
//
// `users.recovery_email` has been in the schema since migration 0001 with
// nothing writing to it, and nothing to prove an address written there
// belonged to the account holder. Both halves are here: enrolling an address
// and confirming it, then using it to clear a two-step password whose owner
// has forgotten it.
//
// The rule the whole flow turns on is that an unverified address is worth
// nothing. It is stored on `users.email` until a code proves it, and only then
// copied to `recovery_email` — so a stolen session that adds an attacker's
// address still cannot recover anything without reaching that inbox.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/security"
)

// emailPattern is a deliberately loose sanity check. Validating an address by
// pattern is a losing game; the code sent to it is the real validation.
var emailPattern = regexp.MustCompile(`^[^@\s]+@[^@\s.]+(\.[^@\s.]+)+$`)

const (
	// emailCodeTTL is short: the code is in an inbox the owner is already
	// reading, not waiting on a mobile network.
	emailCodeTTL = 15 * time.Minute
	// emailCodeAttempts before the challenge is spent.
	emailCodeAttempts = 5
	// emailResendWindow keeps a resend from being a way to send someone mail
	// repeatedly.
	emailResendWindow = time.Minute
)

var (
	ErrEmailUnavailable = errors.New("auth: email delivery is not configured")
	ErrEmailNotSet      = errors.New("auth: no recovery email is set")
	ErrChallengeSpent   = errors.New("auth: that code is no longer usable")
)

// EmailChallenge is one outstanding code.
type EmailChallenge struct {
	ID          uuid.UUID
	UserID      uuid.UUID
	Email       string
	CodeHash    []byte
	Purpose     string
	Attempts    int
	MaxAttempts int
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
}

// EmailStatus is what the account settings screen renders.
type EmailStatus struct {
	// Email is masked: the settings screen has to show which address is on
	// the account, and a session that is not fully trusted should not be able
	// to read it back in full.
	Email string `json:"email,omitempty"`
	// Verified is the only field that decides whether recovery works.
	Verified   bool       `json:"verified"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	// Available reports whether this deployment can send mail at all.
	Available bool `json:"available"`
}

// ------------------------------------------------------------- repository

// SetPendingEmail records an address that has not been proved yet.
//
// It writes `users.email` and clears `recovery_email` and `email_verified_at`
// together: changing the address must revoke whatever the old one could do,
// or adding a new address would leave two live ways in.
func (r *Repository) SetPendingEmail(ctx context.Context, userID uuid.UUID, email string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE users
		SET email = $2, recovery_email = NULL, email_verified_at = NULL, updated_at = now()
		WHERE id = $1`, userID, email)
	if err != nil {
		return fmt.Errorf("auth: set pending email: %w", err)
	}
	return nil
}

// ConfirmEmail promotes a proved address to the recovery address.
func (r *Repository) ConfirmEmail(ctx context.Context, userID uuid.UUID, email string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE users
		SET recovery_email = $2, email_verified_at = now(), updated_at = now()
		WHERE id = $1 AND email = $2`, userID, email)
	if err != nil {
		return fmt.Errorf("auth: confirm email: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The address changed under the challenge; the code proved an address
		// the account no longer claims.
		return ErrChallengeSpent
	}
	return nil
}

// ClearEmail removes the address and everything it could do.
func (r *Repository) ClearEmail(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE users
		SET email = NULL, recovery_email = NULL, email_verified_at = NULL, updated_at = now()
		WHERE id = $1`, userID)
	if err != nil {
		return fmt.Errorf("auth: clear email: %w", err)
	}
	return nil
}

// EmailFor reads the address on an account and whether it has been proved.
func (r *Repository) EmailFor(ctx context.Context, userID uuid.UUID) (string, *time.Time, error) {
	var email *string
	var verifiedAt *time.Time
	err := r.db.Pool.QueryRow(ctx,
		`SELECT email, email_verified_at FROM users WHERE id = $1 AND deleted_at IS NULL`,
		userID).Scan(&email, &verifiedAt)
	if database.IsNoRows(err) {
		return "", nil, ErrNotFound
	}
	if err != nil {
		return "", nil, fmt.Errorf("auth: read email: %w", err)
	}
	if email == nil {
		return "", nil, nil
	}
	return *email, verifiedAt, nil
}

// UserByRecoveryEmail finds an account by its *verified* recovery address.
//
// Unverified addresses are invisible here on purpose: an attacker who added
// their own address to somebody's account and never confirmed it must not be
// able to start a recovery with it.
func (r *Repository) UserByRecoveryEmail(ctx context.Context, email string) (*User, error) {
	row := r.db.Pool.QueryRow(ctx, `SELECT `+userColumns+`
		FROM users
		WHERE lower(recovery_email) = lower($1)
		  AND email_verified_at IS NOT NULL
		  AND deleted_at IS NULL`, email)
	return scanUser(row)
}

// CreateEmailChallenge stores a code hash against an address.
func (r *Repository) CreateEmailChallenge(ctx context.Context, userID uuid.UUID, email string, codeHash []byte, purpose string) (*EmailChallenge, error) {
	challenge := &EmailChallenge{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO email_challenges (user_id, email, code_hash, purpose, max_attempts, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
		RETURNING id, user_id, email, code_hash, purpose, attempts, max_attempts, expires_at, consumed_at`,
		userID, email, codeHash, purpose, emailCodeAttempts, emailCodeTTL.String(),
	).Scan(&challenge.ID, &challenge.UserID, &challenge.Email, &challenge.CodeHash,
		&challenge.Purpose, &challenge.Attempts, &challenge.MaxAttempts,
		&challenge.ExpiresAt, &challenge.ConsumedAt)
	if err != nil {
		return nil, fmt.Errorf("auth: create email challenge: %w", err)
	}
	return challenge, nil
}

// LatestEmailChallenge returns the newest unconsumed challenge for a purpose.
func (r *Repository) LatestEmailChallenge(ctx context.Context, userID uuid.UUID, purpose string) (*EmailChallenge, error) {
	challenge := &EmailChallenge{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, user_id, email, code_hash, purpose, attempts, max_attempts, expires_at, consumed_at
		FROM email_challenges
		WHERE user_id = $1 AND purpose = $2 AND consumed_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1`, userID, purpose,
	).Scan(&challenge.ID, &challenge.UserID, &challenge.Email, &challenge.CodeHash,
		&challenge.Purpose, &challenge.Attempts, &challenge.MaxAttempts,
		&challenge.ExpiresAt, &challenge.ConsumedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read email challenge: %w", err)
	}
	return challenge, nil
}

// RecentEmailChallenge reports whether one was issued inside the window, so a
// resend cannot be turned into a way to mail somebody repeatedly.
func (r *Repository) RecentEmailChallenge(ctx context.Context, userID uuid.UUID, purpose string, window time.Duration) (bool, error) {
	var recent bool
	err := r.db.Pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM email_challenges
		    WHERE user_id = $1 AND purpose = $2 AND created_at > now() - $3::interval)`,
		userID, purpose, window.String()).Scan(&recent)
	if err != nil {
		return false, fmt.Errorf("auth: check recent email challenge: %w", err)
	}
	return recent, nil
}

// BurnEmailAttempt costs the caller an attempt whether or not they retry.
func (r *Repository) BurnEmailAttempt(ctx context.Context, id uuid.UUID) (int, error) {
	var attempts int
	err := r.db.Pool.QueryRow(ctx,
		`UPDATE email_challenges SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`,
		id).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("auth: burn email attempt: %w", err)
	}
	return attempts, nil
}

// ConsumeEmailChallenge marks a challenge used; the condition is what stops
// two concurrent verifications both succeeding.
func (r *Repository) ConsumeEmailChallenge(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE email_challenges SET consumed_at = now() WHERE id = $1 AND consumed_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("auth: consume email challenge: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// DeleteExpiredEmailChallenges is the worker's sweep.
func (r *Repository) DeleteExpiredEmailChallenges(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM email_challenges WHERE expires_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ResetTwoStep clears the second factor and invalidates every issued token.
//
// Both in one transaction: a password reset that left the old sessions alive
// would leave whoever prompted the reset still logged in.
func (r *Repository) ResetTwoStep(ctx context.Context, userID uuid.UUID) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE users
			SET password_hash = NULL, password_salt = NULL, two_step_enabled = FALSE,
			    two_step_hint = NULL, token_version = token_version + 1, updated_at = now()
			WHERE id = $1`, userID); err != nil {
			return fmt.Errorf("auth: reset two-step: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
			userID); err != nil {
			return fmt.Errorf("auth: revoke sessions: %w", err)
		}
		return nil
	})
}

// ---------------------------------------------------------------- service

// SetEmailSender installs the mail transport. Called during assembly, before
// the server accepts a request.
func (s *Service) SetEmailSender(sender EmailSender, echoCodes bool) {
	s.email = sender
	s.emailEcho = echoCodes
}

// EmailStatus reports the address on the account and whether it is proved.
func (s *Service) EmailStatus(ctx context.Context, userID uuid.UUID) (*EmailStatus, error) {
	email, verifiedAt, err := s.repo.EmailFor(ctx, userID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	status := &EmailStatus{Available: Deliverable(s.email), VerifiedAt: verifiedAt}
	if email != "" {
		status.Email = MaskEmail(email)
		status.Verified = verifiedAt != nil
	}
	return status, nil
}

// SetRecoveryEmail enrols an address and sends it a code.
//
// The address is not the recovery address yet. It becomes one when
// VerifyRecoveryEmail is given the code, and not before.
func (s *Service) SetRecoveryEmail(ctx context.Context, userID uuid.UUID, email string) (string, error) {
	if !Deliverable(s.email) {
		return "", httpx.Unavailable("Email recovery is not available on this server")
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if !emailPattern.MatchString(email) || len(email) > 254 {
		return "", httpx.Validation("That is not an email address").
			WithField("email", "must be a valid address")
	}

	recent, err := s.repo.RecentEmailChallenge(ctx, userID, "verify", emailResendWindow)
	if err != nil {
		return "", httpx.Internal(err)
	}
	if recent {
		return "", httpx.RateLimited(int(emailResendWindow.Seconds()))
	}

	if err := s.repo.SetPendingEmail(ctx, userID, email); err != nil {
		return "", httpx.Internal(err)
	}

	code, err := s.issueEmailCode(ctx, userID, email, "verify",
		"Confirm your recovery address")
	if err != nil {
		return "", err
	}
	return code, nil
}

// VerifyRecoveryEmail proves the address and promotes it.
func (s *Service) VerifyRecoveryEmail(ctx context.Context, userID uuid.UUID, code string) error {
	challenge, err := s.consumeEmailCode(ctx, userID, "verify", code)
	if err != nil {
		return err
	}
	if err := s.repo.ConfirmEmail(ctx, userID, challenge.Email); err != nil {
		if errors.Is(err, ErrChallengeSpent) {
			return httpx.Conflict(httpx.CodeConflict,
				"The address on this account changed while that code was outstanding")
		}
		return httpx.Internal(err)
	}
	return nil
}

// RemoveRecoveryEmail takes the address off the account.
func (s *Service) RemoveRecoveryEmail(ctx context.Context, userID uuid.UUID) error {
	if err := s.repo.ClearEmail(ctx, userID); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// StartEmailRecovery sends a code to a verified recovery address.
//
// It answers the same way whether or not the address is on an account. An
// endpoint that said "no such address" would be a way to test which addresses
// have SOBH accounts, one guess at a time.
func (s *Service) StartEmailRecovery(ctx context.Context, email string) (string, error) {
	if !Deliverable(s.email) {
		return "", httpx.Unavailable("Email recovery is not available on this server")
	}

	email = strings.ToLower(strings.TrimSpace(email))
	user, err := s.repo.UserByRecoveryEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil
		}
		return "", httpx.Internal(err)
	}

	recent, err := s.repo.RecentEmailChallenge(ctx, user.ID, "recover", emailResendWindow)
	if err != nil {
		return "", httpx.Internal(err)
	}
	if recent {
		// Still silent about whether the address exists: the caller is told
		// to wait either way.
		return "", nil
	}

	return s.issueEmailCode(ctx, user.ID, email, "recover",
		"Reset the two-step password on your SOBH account")
}

// CompleteEmailRecovery clears the two-step password for whoever holds the
// code sent to the recovery address.
//
// It does not log anybody in. The account is still behind its phone number and
// an OTP; what this removes is the second factor its owner has forgotten.
// Every existing session is revoked with it, so a reset prompted by an
// intruder does not leave that intruder signed in.
func (s *Service) CompleteEmailRecovery(ctx context.Context, email, code string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	user, err := s.repo.UserByRecoveryEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.Unauthorized(httpx.CodeInvalidOTP, "That code is not valid")
		}
		return httpx.Internal(err)
	}

	if _, err := s.consumeEmailCode(ctx, user.ID, "recover", code); err != nil {
		return err
	}
	if err := s.repo.ResetTwoStep(ctx, user.ID); err != nil {
		return httpx.Internal(err)
	}

	s.logger.Info("two-step password reset by email recovery",
		slog.String("user_id", user.ID.String()))
	return nil
}

// issueEmailCode mints, stores and sends a code, returning it only when the
// deployment has explicitly asked for that in development.
func (s *Service) issueEmailCode(ctx context.Context, userID uuid.UUID, email, purpose, subject string) (string, error) {
	code, err := security.NumericCode(s.cfg.OTPLength)
	if err != nil {
		return "", httpx.Internal(err)
	}
	if _, err := s.repo.CreateEmailChallenge(ctx, userID, email,
		security.HashToken(code), purpose); err != nil {
		return "", httpx.Internal(err)
	}

	body := fmt.Sprintf(
		"Your SOBH code is %s.\n\nIt expires in %d minutes. If you did not ask for it, ignore this message and nothing will change.",
		code, int(emailCodeTTL.Minutes()))
	if err := s.email.Send(ctx, email, subject, body); err != nil {
		s.logger.Error("could not send a recovery email",
			slog.String("to", MaskEmail(email)), slog.Any("error", err))
		return "", httpx.Unavailable("Could not send the email; try again shortly")
	}

	if s.emailEcho {
		return code, nil
	}
	return "", nil
}

// consumeEmailCode checks a code against the outstanding challenge, spending
// an attempt whether it matches or not.
func (s *Service) consumeEmailCode(ctx context.Context, userID uuid.UUID, purpose, code string) (*EmailChallenge, error) {
	challenge, err := s.repo.LatestEmailChallenge(ctx, userID, purpose)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "That code is not valid")
		}
		return nil, httpx.Internal(err)
	}
	if time.Now().After(challenge.ExpiresAt) {
		return nil, httpx.Unauthorized(httpx.CodeOTPExpired, "That code has expired")
	}

	attempts, err := s.repo.BurnEmailAttempt(ctx, challenge.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if attempts > challenge.MaxAttempts {
		_, _ = s.repo.ConsumeEmailChallenge(ctx, challenge.ID)
		return nil, httpx.Unauthorized(httpx.CodeOTPAttemptsBurned, "Too many attempts; ask for a new code")
	}

	if !security.CompareTokenHash(code, challenge.CodeHash) {
		return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "That code is not valid")
	}

	consumed, err := s.repo.ConsumeEmailChallenge(ctx, challenge.ID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if !consumed {
		// Another request used it first; a code is good once.
		return nil, httpx.Unauthorized(httpx.CodeInvalidOTP, "That code is not valid")
	}
	return challenge, nil
}
