package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
)

var ErrNotFound = errors.New("auth: record not found")

// User is the identity row the auth flow works with.
type User struct {
	ID             uuid.UUID
	PhoneNumber    string
	Username       *string
	Status         string
	TwoStepEnabled bool
	PasswordHash   *string
	TwoStepHint    *string
	TokenVersion   int
	CreatedAt      time.Time
	DeletedAt      *time.Time
}

func (u *User) IsActive() bool { return u.Status == "active" && u.DeletedAt == nil }

// Device is one client installation belonging to a user (§10).
type Device struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	Name       string
	Platform   string
	AppVersion string
	PushToken  *string
	SyncCursor int64
	LastSeenAt time.Time
	LastIP     *string
	CreatedAt  time.Time
	RevokedAt  *time.Time
}

// Session pairs a device with its current refresh-token family.
type Session struct {
	ID               uuid.UUID
	UserID           uuid.UUID
	DeviceID         uuid.UUID
	RefreshExpiresAt time.Time
	RevokedAt        *time.Time
	CreatedAt        time.Time
	LastUsedAt       time.Time
	UserAgent        string
	IP               *string
}

// OTPChallenge is a pending verification.
type OTPChallenge struct {
	ID          uuid.UUID
	PhoneNumber string
	CodeHash    []byte
	Purpose     string
	Attempts    int
	MaxAttempts int
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// ---------------------------------------------------------------- users

const userColumns = `id, phone_number, username, status, two_step_enabled,
	password_hash, two_step_hint, token_version, created_at, deleted_at`

func scanUser(row pgx.Row) (*User, error) {
	var u User
	err := row.Scan(&u.ID, &u.PhoneNumber, &u.Username, &u.Status, &u.TwoStepEnabled,
		&u.PasswordHash, &u.TwoStepHint, &u.TokenVersion, &u.CreatedAt, &u.DeletedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: scan user: %w", err)
	}
	return &u, nil
}

func (r *Repository) UserByPhone(ctx context.Context, phone string) (*User, error) {
	return scanUser(r.db.Pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE phone_number = $1`, phone))
}

func (r *Repository) UserByID(ctx context.Context, id uuid.UUID) (*User, error) {
	return scanUser(r.db.Pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1`, id))
}

// CreateUser inserts the identity row plus the profile, privacy defaults and
// notification defaults that every account is expected to have. One
// transaction, so a half-registered account cannot exist.
func (r *Repository) CreateUser(ctx context.Context, phone string, phoneHash []byte, displayName string) (*User, error) {
	var user *User
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var id uuid.UUID
		err := tx.QueryRow(ctx,
			`INSERT INTO users (phone_number, phone_hash) VALUES ($1, $2) RETURNING id`,
			phone, phoneHash).Scan(&id)
		if err != nil {
			return fmt.Errorf("auth: insert user: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO user_profiles (user_id, display_name) VALUES ($1, $2)`,
			id, displayName); err != nil {
			return fmt.Errorf("auth: insert profile: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO user_event_counters (user_id) VALUES ($1)`, id); err != nil {
			return fmt.Errorf("auth: insert event counter: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO notification_settings (user_id) VALUES ($1)`, id); err != nil {
			return fmt.Errorf("auth: insert notification settings: %w", err)
		}

		// Privacy defaults: visible to contacts, never to everyone (§55).
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_privacy_settings (user_id, key, rule)
			SELECT $1, key, rule FROM (VALUES
				('last_seen',     'contacts'),
				('profile_photo', 'everyone'),
				('phone_number',  'nobody'),
				('read_receipts', 'everyone'),
				('typing',        'everyone'),
				('calls',         'contacts'),
				('group_invites', 'contacts'),
				('messages',      'everyone'),
				('stories',       'contacts')
			) AS defaults(key, rule)`, id); err != nil {
			return fmt.Errorf("auth: insert privacy defaults: %w", err)
		}

		user, err = scanUser(tx.QueryRow(ctx, `SELECT `+userColumns+` FROM users WHERE id = $1`, id))
		return err
	})
	if err != nil {
		return nil, err
	}
	return user, nil
}

func (r *Repository) TouchUserLastSeen(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE users SET last_seen_at = now() WHERE id = $1`, userID)
	return err
}

// SetTwoStepPassword stores the Argon2id hash and bumps token_version so every
// token issued before the change stops working (§57).
func (r *Repository) SetTwoStepPassword(ctx context.Context, userID uuid.UUID, hash, hint *string) error {
	// $2 is cast explicitly because it appears once as a value assigned to a
	// text column and once inside `IS NOT NULL`, which carries no type of its
	// own. Without the cast PostgreSQL cannot determine the parameter's type
	// and refuses to prepare the statement — so setting a two-step password
	// failed with a 500 rather than working.
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE users
		SET password_hash = $2::text,
		    two_step_hint = $3::text,
		    two_step_enabled = ($2::text IS NOT NULL),
		    token_version = token_version + 1,
		    updated_at = now()
		WHERE id = $1`, userID, hash, hint)
	return err
}

func (r *Repository) BumpTokenVersion(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE users SET token_version = token_version + 1, updated_at = now() WHERE id = $1`, userID)
	return err
}

// ---------------------------------------------------------------- OTP

func (r *Repository) CreateOTPChallenge(ctx context.Context, phone string, codeHash []byte, purpose string, maxAttempts int, ttl time.Duration, ip string) (*OTPChallenge, error) {
	challenge := &OTPChallenge{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO otp_challenges (phone_number, code_hash, purpose, max_attempts, expires_at, request_ip)
		VALUES ($1, $2, $3, $4, now() + $5::interval, $6)
		RETURNING id, phone_number, code_hash, purpose, attempts, max_attempts, expires_at, consumed_at`,
		phone, codeHash, purpose, maxAttempts, ttl.String(), nullableIP(ip),
	).Scan(&challenge.ID, &challenge.PhoneNumber, &challenge.CodeHash, &challenge.Purpose,
		&challenge.Attempts, &challenge.MaxAttempts, &challenge.ExpiresAt, &challenge.ConsumedAt)
	if err != nil {
		return nil, fmt.Errorf("auth: create OTP challenge: %w", err)
	}
	return challenge, nil
}

func (r *Repository) LatestOTPChallenge(ctx context.Context, phone, purpose string) (*OTPChallenge, error) {
	challenge := &OTPChallenge{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, phone_number, code_hash, purpose, attempts, max_attempts, expires_at, consumed_at
		FROM otp_challenges
		WHERE phone_number = $1 AND purpose = $2 AND consumed_at IS NULL
		ORDER BY created_at DESC
		LIMIT 1`, phone, purpose,
	).Scan(&challenge.ID, &challenge.PhoneNumber, &challenge.CodeHash, &challenge.Purpose,
		&challenge.Attempts, &challenge.MaxAttempts, &challenge.ExpiresAt, &challenge.ConsumedAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read OTP challenge: %w", err)
	}
	return challenge, nil
}

// BurnOTPAttempt increments the attempt counter and returns the new value,
// so a wrong code costs the caller an attempt even if the request is retried.
func (r *Repository) BurnOTPAttempt(ctx context.Context, id uuid.UUID) (int, error) {
	var attempts int
	err := r.db.Pool.QueryRow(ctx,
		`UPDATE otp_challenges SET attempts = attempts + 1 WHERE id = $1 RETURNING attempts`,
		id).Scan(&attempts)
	if err != nil {
		return 0, fmt.Errorf("auth: burn OTP attempt: %w", err)
	}
	return attempts, nil
}

// ConsumeOTPChallenge marks a challenge used. The UPDATE is conditional so two
// concurrent verifications cannot both succeed.
func (r *Repository) ConsumeOTPChallenge(ctx context.Context, id uuid.UUID) (bool, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE otp_challenges SET consumed_at = now() WHERE id = $1 AND consumed_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("auth: consume OTP challenge: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (r *Repository) DeleteExpiredOTPChallenges(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM otp_challenges WHERE expires_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------- devices

// UpsertDevice returns the existing device for this user+platform+name pair or
// creates a new one, enforcing the per-user device cap (§11).
func (r *Repository) UpsertDevice(ctx context.Context, userID uuid.UUID, name, platform, appVersion, ip string, maxDevices int) (*Device, error) {
	var device *Device
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		var active int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM devices WHERE user_id = $1 AND revoked_at IS NULL`,
			userID).Scan(&active); err != nil {
			return fmt.Errorf("auth: count devices: %w", err)
		}
		if active >= maxDevices {
			return ErrTooManyDevices
		}

		var d Device
		err := tx.QueryRow(ctx, `
			INSERT INTO devices (user_id, name, platform, app_version, last_ip)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING id, user_id, name, platform, app_version, push_token,
			          sync_cursor, last_seen_at, created_at, revoked_at`,
			userID, name, platform, appVersion, nullableIP(ip),
		).Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.AppVersion, &d.PushToken,
			&d.SyncCursor, &d.LastSeenAt, &d.CreatedAt, &d.RevokedAt)
		if err != nil {
			return fmt.Errorf("auth: insert device: %w", err)
		}
		device = &d
		return nil
	})
	if err != nil {
		return nil, err
	}
	return device, nil
}

var ErrTooManyDevices = errors.New("auth: device limit reached")

func (r *Repository) ListDevices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, user_id, name, platform, app_version, push_token,
		       sync_cursor, last_seen_at, host(last_ip), created_at, revoked_at
		FROM devices
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY last_seen_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list devices: %w", err)
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.UserID, &d.Name, &d.Platform, &d.AppVersion, &d.PushToken,
			&d.SyncCursor, &d.LastSeenAt, &d.LastIP, &d.CreatedAt, &d.RevokedAt); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

func (r *Repository) TouchDevice(ctx context.Context, deviceID uuid.UUID, ip string) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE devices SET last_seen_at = now(), last_ip = COALESCE($2, last_ip) WHERE id = $1`,
		deviceID, nullableIP(ip))
	return err
}

func (r *Repository) UpdateSyncCursor(ctx context.Context, deviceID uuid.UUID, cursor int64) error {
	// GREATEST keeps the cursor monotonic when acknowledgements arrive out of order.
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE devices SET sync_cursor = GREATEST(sync_cursor, $2) WHERE id = $1`, deviceID, cursor)
	return err
}

// ---------------------------------------------------------------- sessions

func (r *Repository) CreateSession(ctx context.Context, userID, deviceID uuid.UUID, refreshHash []byte, expiresAt time.Time, userAgent, ip string) (*Session, error) {
	session := &Session{}
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO sessions (user_id, device_id, refresh_token_hash, refresh_expires_at, user_agent, ip)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, user_id, device_id, refresh_expires_at, revoked_at, created_at, last_used_at, user_agent`,
		userID, deviceID, refreshHash, expiresAt, userAgent, nullableIP(ip),
	).Scan(&session.ID, &session.UserID, &session.DeviceID, &session.RefreshExpiresAt,
		&session.RevokedAt, &session.CreatedAt, &session.LastUsedAt, &session.UserAgent)
	if err != nil {
		return nil, fmt.Errorf("auth: create session: %w", err)
	}
	return session, nil
}

// RotateRefreshToken atomically swaps the stored hash for a new one.
//
// The UPDATE matches on the *old* hash, so a replayed refresh token finds no
// row and is rejected. That is the detection point for token theft (§33).
func (r *Repository) RotateRefreshToken(ctx context.Context, oldHash, newHash []byte, expiresAt time.Time, ip string) (*Session, error) {
	session := &Session{}
	err := r.db.Pool.QueryRow(ctx, `
		UPDATE sessions
		SET refresh_token_hash = $2,
		    refresh_expires_at = $3,
		    rotated_at = now(),
		    last_used_at = now(),
		    ip = COALESCE($4, ip)
		WHERE refresh_token_hash = $1
		  AND revoked_at IS NULL
		  AND refresh_expires_at > now()
		RETURNING id, user_id, device_id, refresh_expires_at, revoked_at, created_at, last_used_at, user_agent`,
		oldHash, newHash, expiresAt, nullableIP(ip),
	).Scan(&session.ID, &session.UserID, &session.DeviceID, &session.RefreshExpiresAt,
		&session.RevokedAt, &session.CreatedAt, &session.LastUsedAt, &session.UserAgent)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: rotate refresh token: %w", err)
	}
	return session, nil
}

func (r *Repository) SessionByID(ctx context.Context, id uuid.UUID) (*Session, error) {
	session := &Session{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, user_id, device_id, refresh_expires_at, revoked_at, created_at, last_used_at, user_agent
		FROM sessions WHERE id = $1`, id,
	).Scan(&session.ID, &session.UserID, &session.DeviceID, &session.RefreshExpiresAt,
		&session.RevokedAt, &session.CreatedAt, &session.LastUsedAt, &session.UserAgent)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("auth: read session: %w", err)
	}
	return session, nil
}

func (r *Repository) ListSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, user_id, device_id, refresh_expires_at, revoked_at, created_at,
		       last_used_at, user_agent, host(ip)
		FROM sessions
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY last_used_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("auth: list sessions: %w", err)
	}
	defer rows.Close()

	var sessions []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.ID, &s.UserID, &s.DeviceID, &s.RefreshExpiresAt, &s.RevokedAt,
			&s.CreatedAt, &s.LastUsedAt, &s.UserAgent, &s.IP); err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// RevokeSession revokes one session and its device in a single transaction.
func (r *Repository) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID, reason string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		var deviceID uuid.UUID
		err := tx.QueryRow(ctx, `
			UPDATE sessions SET revoked_at = now(), revoked_reason = $3
			WHERE id = $2 AND user_id = $1 AND revoked_at IS NULL
			RETURNING device_id`, userID, sessionID, reason).Scan(&deviceID)
		if database.IsNoRows(err) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("auth: revoke session: %w", err)
		}

		_, err = tx.Exec(ctx, `UPDATE devices SET revoked_at = now() WHERE id = $1`, deviceID)
		if err != nil {
			return fmt.Errorf("auth: revoke device: %w", err)
		}
		_, err = tx.Exec(ctx, `UPDATE push_tokens SET is_valid = FALSE WHERE device_id = $1`, deviceID)
		return err
	})
}

// RevokeAllSessions logs the user out everywhere, optionally sparing the
// session that issued the request.
func (r *Repository) RevokeAllSessions(ctx context.Context, userID uuid.UUID, except uuid.UUID, reason string) ([]uuid.UUID, error) {
	var revoked []uuid.UUID
	err := r.db.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE sessions SET revoked_at = now(), revoked_reason = $3
			WHERE user_id = $1 AND revoked_at IS NULL AND id <> $2
			RETURNING id, device_id`, userID, except, reason)
		if err != nil {
			return fmt.Errorf("auth: revoke sessions: %w", err)
		}

		var deviceIDs []uuid.UUID
		for rows.Next() {
			var sessionID, deviceID uuid.UUID
			if err := rows.Scan(&sessionID, &deviceID); err != nil {
				rows.Close()
				return err
			}
			revoked = append(revoked, sessionID)
			deviceIDs = append(deviceIDs, deviceID)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		if len(deviceIDs) > 0 {
			if _, err := tx.Exec(ctx,
				`UPDATE devices SET revoked_at = now() WHERE id = ANY($1)`, deviceIDs); err != nil {
				return fmt.Errorf("auth: revoke devices: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE push_tokens SET is_valid = FALSE WHERE device_id = ANY($1)`, deviceIDs); err != nil {
				return err
			}
		}
		return nil
	})
	return revoked, err
}

// ---------------------------------------------------------------- audit

func (r *Repository) RecordLogin(ctx context.Context, userID uuid.UUID, event, ip, userAgent, platform string, succeeded bool) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO login_history (user_id, event, ip, user_agent, platform, succeeded)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		userID, event, nullableIP(ip), userAgent, platform, succeeded)
	return err
}

// LoginRecord is one entry in the sign-in log shown on the security screen.
//
// The tags are not decoration: this is a response body, and without them the
// endpoint answered in Go field names while every other response in the API
// is snake_case.
type LoginRecord struct {
	Event     string    `json:"event"`
	IP        *string   `json:"ip,omitempty"`
	UserAgent string    `json:"user_agent"`
	Platform  string    `json:"platform"`
	Succeeded bool      `json:"succeeded"`
	CreatedAt time.Time `json:"created_at"`
}

func (r *Repository) LoginHistory(ctx context.Context, userID uuid.UUID, limit int) ([]LoginRecord, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT event, host(ip), user_agent, platform, succeeded, created_at
		FROM login_history WHERE user_id = $1
		ORDER BY created_at DESC LIMIT $2`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("auth: login history: %w", err)
	}
	defer rows.Close()

	var records []LoginRecord
	for rows.Next() {
		var record LoginRecord
		if err := rows.Scan(&record.Event, &record.IP, &record.UserAgent,
			&record.Platform, &record.Succeeded, &record.CreatedAt); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// AdminRolesFor returns the permission set backing RBAC checks (§32).
func (r *Repository) AdminRolesFor(ctx context.Context, userID uuid.UUID) (roles []string, permissions []string, err error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT ar.key, ar.permissions
		FROM admin_users au
		JOIN admin_roles ar ON ar.key = au.role_key
		WHERE au.user_id = $1 AND au.revoked_at IS NULL`, userID)
	if err != nil {
		return nil, nil, fmt.Errorf("auth: read admin roles: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]struct{})
	for rows.Next() {
		var role string
		var perms []string
		if err := rows.Scan(&role, &perms); err != nil {
			return nil, nil, err
		}
		roles = append(roles, role)
		for _, p := range perms {
			if _, ok := seen[p]; !ok {
				seen[p] = struct{}{}
				permissions = append(permissions, p)
			}
		}
	}
	return roles, permissions, rows.Err()
}

// nullableIP converts a possibly-empty string into a value Postgres accepts
// for an INET column.
func nullableIP(ip string) any {
	if ip == "" {
		return nil
	}
	if net.ParseIP(ip) == nil {
		return nil
	}
	return ip
}
