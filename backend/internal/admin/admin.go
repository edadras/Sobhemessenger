// Package admin implements the operator surface behind role-based access
// control (§31, §32).
//
// Every endpoint here is gated by a specific permission, and every mutation
// writes an audit entry (§33). The audit log is written in the same
// transaction as the change where one exists, so an action can never be
// performed without leaving a record.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/featureflags"
	"github.com/sobh/messenger/backend/internal/httpx"
)

var ErrNotFound = errors.New("admin: not found")

// Permissions (§32).
const (
	PermUsersRead     = "users.read"
	PermUsersWrite    = "users.write"
	PermChatsRead     = "chats.read"
	PermReportsRead   = "reports.read"
	PermReportsWrite  = "reports.write"
	PermBansWrite     = "bans.write"
	PermFlagsWrite    = "flags.write"
	PermAnalyticsRead = "analytics.read"
	PermStorageRead   = "storage.read"
	PermAuditRead     = "audit.read"
)

// UserSummary is the operator's view of an account. It deliberately excludes
// message content and carries the phone number masked (§60).
type UserSummary struct {
	ID           uuid.UUID  `json:"id"`
	PhoneMasked  string     `json:"phone_masked"`
	Username     *string    `json:"username,omitempty"`
	DisplayName  string     `json:"display_name"`
	Status       string     `json:"status"`
	IsBot        bool       `json:"is_bot"`
	DeviceCount  int        `json:"device_count"`
	ChatCount    int        `json:"chat_count"`
	MessageCount int64      `json:"message_count"`
	CreatedAt    time.Time  `json:"created_at"`
	LastSeenAt   *time.Time `json:"last_seen_at,omitempty"`
	AdminRoles   []string   `json:"admin_roles,omitempty"`
}

// Report is a user-submitted moderation case.
type Report struct {
	ID         uuid.UUID  `json:"id"`
	ReporterID *uuid.UUID `json:"reporter_id,omitempty"`
	TargetType string     `json:"target_type"`
	TargetID   uuid.UUID  `json:"target_id"`
	Reason     string     `json:"reason"`
	Detail     string     `json:"detail,omitempty"`
	Status     string     `json:"status"`
	AssignedTo *uuid.UUID `json:"assigned_to,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Ban restricts an account globally or within one chat.
type Ban struct {
	ID        uuid.UUID  `json:"id"`
	UserID    *uuid.UUID `json:"user_id,omitempty"`
	ChatID    *uuid.UUID `json:"chat_id,omitempty"`
	Scope     string     `json:"scope"`
	Reason    string     `json:"reason"`
	IssuedBy  *uuid.UUID `json:"issued_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// AuditEntry is one recorded operator action.
type AuditEntry struct {
	ID         uuid.UUID       `json:"id"`
	ActorID    *uuid.UUID      `json:"actor_id,omitempty"`
	ActorRole  string          `json:"actor_role,omitempty"`
	Action     string          `json:"action"`
	TargetType string          `json:"target_type,omitempty"`
	TargetID   string          `json:"target_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	IP         *string         `json:"ip,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// Dashboard is the operator overview (§62).
type Dashboard struct {
	Users struct {
		Total       int64 `json:"total"`
		Active      int64 `json:"active"`
		Banned      int64 `json:"banned"`
		NewToday    int64 `json:"new_today"`
		DailyActive int64 `json:"daily_active"`
	} `json:"users"`
	Messages struct {
		Total int64 `json:"total"`
		Today int64 `json:"today"`
	} `json:"messages"`
	Chats struct {
		Private  int64 `json:"private"`
		Groups   int64 `json:"groups"`
		Channels int64 `json:"channels"`
	} `json:"chats"`
	Media struct {
		Objects    int64 `json:"objects"`
		TotalBytes int64 `json:"total_bytes"`
	} `json:"media"`
	News struct {
		Published int64 `json:"published"`
		Drafts    int64 `json:"drafts"`
		Views     int64 `json:"views"`
	} `json:"news"`
	Moderation struct {
		OpenReports int64 `json:"open_reports"`
		ActiveBans  int64 `json:"active_bans"`
	} `json:"moderation"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// Audit records an operator action.
func (r *Repository) Audit(ctx context.Context, actorID uuid.UUID, actorRole, action, targetType, targetID string, detail any, ip, userAgent string) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		encoded = []byte(`{}`)
	}

	_, err = r.db.Pool.Exec(ctx, `
		INSERT INTO audit_logs (actor_id, actor_role, action, target_type, target_id, detail, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		actorID, actorRole, action, targetType, targetID, encoded, nullableIP(ip), userAgent)
	if err != nil {
		return fmt.Errorf("admin: write audit log: %w", err)
	}
	return nil
}

func (r *Repository) AuditLog(ctx context.Context, action string, actorID *uuid.UUID, limit, offset int) ([]AuditEntry, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, actor_id, actor_role, action, target_type, target_id, detail, host(ip), created_at
		FROM audit_logs
		WHERE ($1 = '' OR action = $1)
		  AND ($2::uuid IS NULL OR actor_id = $2)
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`, action, actorID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("admin: read audit log: %w", err)
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		if err := rows.Scan(&entry.ID, &entry.ActorID, &entry.ActorRole, &entry.Action,
			&entry.TargetType, &entry.TargetID, &entry.Detail, &entry.IP, &entry.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// SearchUsers finds accounts by username, display name or exact phone number.
func (r *Repository) SearchUsers(ctx context.Context, query, status string, limit, offset int) ([]UserSummary, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT u.id, u.phone_number, u.username, COALESCE(p.display_name, ''),
		       u.status, u.is_bot, u.created_at, u.last_seen_at,
		       (SELECT count(*) FROM devices d WHERE d.user_id = u.id AND d.revoked_at IS NULL),
		       (SELECT count(*) FROM chat_members m WHERE m.user_id = u.id AND m.left_at IS NULL),
		       (SELECT count(*) FROM messages m WHERE m.sender_id = u.id),
		       COALESCE(array_agg(au.role_key) FILTER (WHERE au.role_key IS NOT NULL), '{}')
		FROM users u
		LEFT JOIN user_profiles p ON p.user_id = u.id
		LEFT JOIN admin_users au ON au.user_id = u.id AND au.revoked_at IS NULL
		WHERE ($1 = '' OR u.username::text ILIKE '%' || $1 || '%'
		       OR p.display_name ILIKE '%' || $1 || '%'
		       OR u.phone_number = $1)
		  AND ($2 = '' OR u.status = $2)
		GROUP BY u.id, p.display_name
		ORDER BY u.created_at DESC
		LIMIT $3 OFFSET $4`, query, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("admin: search users: %w", err)
	}
	defer rows.Close()

	var users []UserSummary
	for rows.Next() {
		var user UserSummary
		var phone string
		if err := rows.Scan(&user.ID, &phone, &user.Username, &user.DisplayName,
			&user.Status, &user.IsBot, &user.CreatedAt, &user.LastSeenAt,
			&user.DeviceCount, &user.ChatCount, &user.MessageCount, &user.AdminRoles); err != nil {
			return nil, err
		}
		// Support staff need to recognise an account, not read its number.
		user.PhoneMasked = maskPhone(phone)
		users = append(users, user)
	}
	return users, rows.Err()
}

// SetUserStatus bans, restricts or reinstates an account.
func (r *Repository) SetUserStatus(ctx context.Context, userID uuid.UUID, status string) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE users SET status = $2, token_version = token_version + 1, updated_at = now()
			WHERE id = $1`, userID, status)
		if err != nil {
			return fmt.Errorf("admin: set user status: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}

		// Banning revokes every session immediately; bumping token_version
		// alone would leave live WebSockets connected until they reconnect.
		if status == "banned" || status == "deleting" {
			if _, err := tx.Exec(ctx, `
				UPDATE sessions SET revoked_at = now(), revoked_reason = 'account_' || $2
				WHERE user_id = $1 AND revoked_at IS NULL`, userID, status); err != nil {
				return fmt.Errorf("admin: revoke sessions: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE devices SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`,
				userID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) Reports(ctx context.Context, status string, limit, offset int) ([]Report, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, reporter_id, target_type, target_id, reason, detail,
		       status, assigned_to, resolution, created_at, resolved_at
		FROM reports
		WHERE ($1 = '' OR status = $1)
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3`, status, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("admin: list reports: %w", err)
	}
	defer rows.Close()

	var reports []Report
	for rows.Next() {
		var report Report
		if err := rows.Scan(&report.ID, &report.ReporterID, &report.TargetType,
			&report.TargetID, &report.Reason, &report.Detail, &report.Status,
			&report.AssignedTo, &report.Resolution, &report.CreatedAt, &report.ResolvedAt); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, rows.Err()
}

// CreateReport is called from the user-facing API when someone reports content.
func (r *Repository) CreateReport(ctx context.Context, reporterID uuid.UUID, targetType string, targetID uuid.UUID, reason, detail string) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO reports (reporter_id, target_type, target_id, reason, detail)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id`, reporterID, targetType, targetID, reason, detail).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("admin: create report: %w", err)
	}
	return id, nil
}

// MessageAuthor resolves who wrote a message, for scoring a report about it.
// A message posted by a chat rather than a person has no author to score.
func (r *Repository) MessageAuthor(ctx context.Context, messageID uuid.UUID) (*uuid.UUID, error) {
	var author *uuid.UUID
	err := r.db.Pool.QueryRow(ctx,
		`SELECT sender_id FROM messages WHERE id = $1`, messageID).Scan(&author)
	if err != nil {
		return nil, fmt.Errorf("admin: read message author: %w", err)
	}
	return author, nil
}

func (r *Repository) ResolveReport(ctx context.Context, reportID, resolverID uuid.UUID, status, resolution string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE reports
		SET status = $3, resolution = $4, assigned_to = $2, resolved_at = now()
		WHERE id = $1 AND status IN ('open', 'reviewing')`,
		reportID, resolverID, status, resolution)
	if err != nil {
		return fmt.Errorf("admin: resolve report: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) CreateBan(ctx context.Context, ban Ban) (uuid.UUID, error) {
	var id uuid.UUID
	err := r.db.Pool.QueryRow(ctx, `
		INSERT INTO bans (user_id, chat_id, scope, reason, issued_by, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id`,
		ban.UserID, ban.ChatID, ban.Scope, ban.Reason, ban.IssuedBy, ban.ExpiresAt).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("admin: create ban: %w", err)
	}
	return id, nil
}

func (r *Repository) LiftBan(ctx context.Context, banID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE bans SET lifted_at = now() WHERE id = $1 AND lifted_at IS NULL`, banID)
	if err != nil {
		return fmt.Errorf("admin: lift ban: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *Repository) Bans(ctx context.Context, limit, offset int) ([]Ban, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, user_id, chat_id, scope, reason, issued_by, expires_at, created_at
		FROM bans
		WHERE lifted_at IS NULL AND (expires_at IS NULL OR expires_at > now())
		ORDER BY created_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("admin: list bans: %w", err)
	}
	defer rows.Close()

	var bans []Ban
	for rows.Next() {
		var ban Ban
		if err := rows.Scan(&ban.ID, &ban.UserID, &ban.ChatID, &ban.Scope,
			&ban.Reason, &ban.IssuedBy, &ban.ExpiresAt, &ban.CreatedAt); err != nil {
			return nil, err
		}
		bans = append(bans, ban)
	}
	return bans, rows.Err()
}

// GrantRole assigns an administrative role.
func (r *Repository) GrantRole(ctx context.Context, userID uuid.UUID, roleKey string, grantedBy uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO admin_users (user_id, role_key, granted_by)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, role_key) DO UPDATE
		SET revoked_at = NULL, granted_by = EXCLUDED.granted_by, granted_at = now()`,
		userID, roleKey, grantedBy)
	if err != nil {
		if database.IsForeignKeyViolation(err) {
			return ErrNotFound
		}
		return fmt.Errorf("admin: grant role: %w", err)
	}
	return nil
}

func (r *Repository) RevokeRole(ctx context.Context, userID uuid.UUID, roleKey string) error {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE admin_users SET revoked_at = now()
		WHERE user_id = $1 AND role_key = $2 AND revoked_at IS NULL`, userID, roleKey)
	if err != nil {
		return fmt.Errorf("admin: revoke role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Dashboard aggregates the operator overview in one round trip.
func (r *Repository) Dashboard(ctx context.Context) (*Dashboard, error) {
	dashboard := &Dashboard{}

	err := r.db.Pool.QueryRow(ctx, `
		SELECT
			(SELECT count(*) FROM users WHERE deleted_at IS NULL),
			(SELECT count(*) FROM users WHERE status = 'active' AND deleted_at IS NULL),
			(SELECT count(*) FROM users WHERE status = 'banned'),
			(SELECT count(*) FROM users WHERE created_at >= CURRENT_DATE),
			(SELECT count(*) FROM users WHERE last_seen_at >= CURRENT_DATE),
			(SELECT count(*) FROM messages),
			(SELECT count(*) FROM messages WHERE created_at >= CURRENT_DATE),
			(SELECT count(*) FROM chats WHERE type = 'private' AND deleted_at IS NULL),
			(SELECT count(*) FROM chats WHERE type = 'group' AND deleted_at IS NULL),
			(SELECT count(*) FROM chats WHERE type = 'channel' AND deleted_at IS NULL),
			(SELECT count(*) FROM media WHERE deleted_at IS NULL),
			(SELECT COALESCE(sum(size_bytes), 0) FROM media WHERE deleted_at IS NULL),
			(SELECT count(*) FROM news_articles WHERE status = 'published'),
			(SELECT count(*) FROM news_articles WHERE status IN ('draft', 'review')),
			(SELECT COALESCE(sum(view_count), 0) FROM news_articles),
			(SELECT count(*) FROM reports WHERE status IN ('open', 'reviewing')),
			(SELECT count(*) FROM bans WHERE lifted_at IS NULL
			   AND (expires_at IS NULL OR expires_at > now()))`,
	).Scan(
		&dashboard.Users.Total, &dashboard.Users.Active, &dashboard.Users.Banned,
		&dashboard.Users.NewToday, &dashboard.Users.DailyActive,
		&dashboard.Messages.Total, &dashboard.Messages.Today,
		&dashboard.Chats.Private, &dashboard.Chats.Groups, &dashboard.Chats.Channels,
		&dashboard.Media.Objects, &dashboard.Media.TotalBytes,
		&dashboard.News.Published, &dashboard.News.Drafts, &dashboard.News.Views,
		&dashboard.Moderation.OpenReports, &dashboard.Moderation.ActiveBans)
	if err != nil {
		return nil, fmt.Errorf("admin: dashboard: %w", err)
	}
	return dashboard, nil
}

// TimeSeriesPoint is one day of a metric.
type TimeSeriesPoint struct {
	Day   time.Time `json:"day"`
	Value int64     `json:"value"`
}

// TimeSeries returns a daily series for the analytics charts (§62).
func (r *Repository) TimeSeries(ctx context.Context, metric string, days int) ([]TimeSeriesPoint, error) {
	var query string
	switch metric {
	case "new_users":
		query = `SELECT date_trunc('day', created_at)::date AS day, count(*)
		         FROM users WHERE created_at >= CURRENT_DATE - $1::int
		         GROUP BY day ORDER BY day`
	case "messages":
		query = `SELECT date_trunc('day', created_at)::date AS day, count(*)
		         FROM messages WHERE created_at >= CURRENT_DATE - $1::int
		         GROUP BY day ORDER BY day`
	case "active_users":
		query = `SELECT date_trunc('day', last_seen_at)::date AS day, count(*)
		         FROM users WHERE last_seen_at >= CURRENT_DATE - $1::int
		         GROUP BY day ORDER BY day`
	case "news_views":
		query = `SELECT day, sum(views)::bigint
		         FROM news_article_views WHERE day >= CURRENT_DATE - $1::int
		         GROUP BY day ORDER BY day`
	default:
		return nil, ErrUnknownMetric
	}

	rows, err := r.db.Pool.Query(ctx, query, days)
	if err != nil {
		return nil, fmt.Errorf("admin: time series: %w", err)
	}
	defer rows.Close()

	var points []TimeSeriesPoint
	for rows.Next() {
		var point TimeSeriesPoint
		if err := rows.Scan(&point.Day, &point.Value); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

var ErrUnknownMetric = errors.New("admin: unknown metric")

func maskPhone(phone string) string {
	if len(phone) < 8 {
		return "***"
	}
	return phone[:6] + "***" + phone[len(phone)-4:]
}

func nullableIP(ip string) any {
	if ip == "" {
		return nil
	}
	return ip
}

// ---------------------------------------------------------------- service

// InvalidateUser lets the admin module drop cached authentication state after
// a ban or role change, without importing the auth package's internals.
type InvalidateUser func(ctx context.Context, userID uuid.UUID)

type Service struct {
	repo       *Repository
	flags      *featureflags.Service
	invalidate InvalidateUser
	logger     *slog.Logger
	// spam is optional; see SetSpamRecorder.
	spam SpamRecorder
}

// SpamRecorder is told when a user is reported (§34).
//
// A report is the strongest ordinary signal there is — somebody took a
// deliberate action naming this account — so it weighs far more than a block
// or a rate-limit trip. It is an interface so this package does not depend on
// the anti-spam module.
type SpamRecorder interface {
	Reported(ctx context.Context, userID uuid.UUID, reason string)
}

// SetSpamRecorder installs the recorder. Called during assembly, before the
// server accepts a request.
func (s *Service) SetSpamRecorder(recorder SpamRecorder) { s.spam = recorder }

func NewService(repo *Repository, flags *featureflags.Service, invalidate InvalidateUser, logger *slog.Logger) *Service {
	return &Service{repo: repo, flags: flags, invalidate: invalidate, logger: logger}
}

// actor bundles the principal with the request metadata the audit log needs.
type actor struct {
	principal *httpx.Principal
	ip        string
	userAgent string
}

func (s *Service) audit(ctx context.Context, a actor, action, targetType, targetID string, detail any) {
	role := ""
	if len(a.principal.AdminRoles) > 0 {
		role = a.principal.AdminRoles[0]
	}
	if err := s.repo.Audit(ctx, a.principal.UserID, role, action,
		targetType, targetID, detail, a.ip, a.userAgent); err != nil {
		// An unaudited action is a compliance problem, so it is logged loudly
		// even though the action itself has already succeeded.
		s.logger.Error("failed to write audit entry",
			slog.String("action", action), slog.Any("error", err))
	}
}

func (s *Service) Dashboard(ctx context.Context) (*Dashboard, error) {
	dashboard, err := s.repo.Dashboard(ctx)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return dashboard, nil
}

func (s *Service) TimeSeries(ctx context.Context, metric string, days int) ([]TimeSeriesPoint, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	points, err := s.repo.TimeSeries(ctx, metric, days)
	if err != nil {
		if errors.Is(err, ErrUnknownMetric) {
			return nil, httpx.Validation("Unknown metric").
				WithField("metric", "must be new_users, messages, active_users or news_views")
		}
		return nil, httpx.Internal(err)
	}
	return points, nil
}

func (s *Service) SearchUsers(ctx context.Context, query, status string, limit, offset int) ([]UserSummary, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	users, err := s.repo.SearchUsers(ctx, query, status, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return users, nil
}

// SetUserStatus bans or reinstates an account.
func (s *Service) SetUserStatus(ctx context.Context, a actor, userID uuid.UUID, status, reason string) error {
	switch status {
	case "active", "restricted", "banned":
	default:
		return httpx.Validation("Unsupported status").
			WithField("status", "must be active, restricted or banned")
	}
	// An operator cannot ban themselves out of the system by accident.
	if userID == a.principal.UserID {
		return httpx.Validation("You cannot change your own account status").
			WithField("user_id", "cannot target yourself")
	}

	if err := s.repo.SetUserStatus(ctx, userID, status); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "User not found")
		}
		return httpx.Internal(err)
	}

	if s.invalidate != nil {
		s.invalidate(ctx, userID)
	}
	s.audit(ctx, a, "user.status_changed", "user", userID.String(),
		map[string]any{"status": status, "reason": reason})
	return nil
}

func (s *Service) Reports(ctx context.Context, status string, limit, offset int) ([]Report, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	reports, err := s.repo.Reports(ctx, status, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return reports, nil
}

func (s *Service) ResolveReport(ctx context.Context, a actor, reportID uuid.UUID, status, resolution string) error {
	switch status {
	case "reviewing", "actioned", "dismissed":
	default:
		return httpx.Validation("Unsupported report status").
			WithField("status", "must be reviewing, actioned or dismissed")
	}

	if err := s.repo.ResolveReport(ctx, reportID, a.principal.UserID, status, resolution); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Report not found or already resolved")
		}
		return httpx.Internal(err)
	}

	s.audit(ctx, a, "report.resolved", "report", reportID.String(),
		map[string]any{"status": status, "resolution": resolution})
	return nil
}

func (s *Service) CreateBan(ctx context.Context, a actor, ban Ban) (uuid.UUID, error) {
	switch ban.Scope {
	case "global":
		if ban.UserID == nil {
			return uuid.Nil, httpx.Validation("A global ban needs a user").
				WithField("user_id", "required")
		}
		ban.ChatID = nil
	case "chat":
		if ban.ChatID == nil || ban.UserID == nil {
			return uuid.Nil, httpx.Validation("A chat ban needs a user and a chat").
				WithField("chat_id", "required")
		}
	default:
		return uuid.Nil, httpx.Validation("Unsupported ban scope").
			WithField("scope", "must be global or chat")
	}
	if ban.ExpiresAt != nil && ban.ExpiresAt.Before(time.Now()) {
		return uuid.Nil, httpx.Validation("Expiry is in the past").
			WithField("expires_at", "must be in the future")
	}

	ban.IssuedBy = &a.principal.UserID
	id, err := s.repo.CreateBan(ctx, ban)
	if err != nil {
		return uuid.Nil, httpx.Internal(err)
	}

	// A global ban also suspends the account, so the two cannot disagree.
	if ban.Scope == "global" {
		if err := s.repo.SetUserStatus(ctx, *ban.UserID, "banned"); err != nil {
			s.logger.Error("ban recorded but account status not updated",
				slog.String("user_id", ban.UserID.String()), slog.Any("error", err))
		} else if s.invalidate != nil {
			s.invalidate(ctx, *ban.UserID)
		}
	}

	s.audit(ctx, a, "ban.created", "user", ban.UserID.String(),
		map[string]any{"scope": ban.Scope, "reason": ban.Reason, "ban_id": id})
	return id, nil
}

func (s *Service) LiftBan(ctx context.Context, a actor, banID uuid.UUID) error {
	if err := s.repo.LiftBan(ctx, banID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Ban not found or already lifted")
		}
		return httpx.Internal(err)
	}
	s.audit(ctx, a, "ban.lifted", "ban", banID.String(), nil)
	return nil
}

func (s *Service) Bans(ctx context.Context, limit, offset int) ([]Ban, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	bans, err := s.repo.Bans(ctx, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return bans, nil
}

// GrantRole assigns an administrative role. Only a super admin may do this:
// otherwise an administrator could promote themselves.
func (s *Service) GrantRole(ctx context.Context, a actor, userID uuid.UUID, roleKey string) error {
	if !a.principal.HasPermission("*") {
		return httpx.Forbidden(httpx.CodeForbidden, "Only a super admin can assign roles")
	}

	if err := s.repo.GrantRole(ctx, userID, roleKey, a.principal.UserID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "User or role not found")
		}
		return httpx.Internal(err)
	}

	if s.invalidate != nil {
		s.invalidate(ctx, userID)
	}
	s.audit(ctx, a, "role.granted", "user", userID.String(), map[string]any{"role": roleKey})
	return nil
}

func (s *Service) RevokeRole(ctx context.Context, a actor, userID uuid.UUID, roleKey string) error {
	if !a.principal.HasPermission("*") {
		return httpx.Forbidden(httpx.CodeForbidden, "Only a super admin can revoke roles")
	}

	if err := s.repo.RevokeRole(ctx, userID, roleKey); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "That role is not assigned")
		}
		return httpx.Internal(err)
	}

	if s.invalidate != nil {
		s.invalidate(ctx, userID)
	}
	s.audit(ctx, a, "role.revoked", "user", userID.String(), map[string]any{"role": roleKey})
	return nil
}

func (s *Service) SetFlag(ctx context.Context, a actor, key string, enabled bool, rollout int) error {
	if err := s.flags.Set(ctx, key, enabled, rollout, a.principal.UserID); err != nil {
		return err
	}
	s.audit(ctx, a, "flag.changed", "flag", key,
		map[string]any{"enabled": enabled, "rollout_percent": rollout})
	return nil
}

func (s *Service) AuditLog(ctx context.Context, action string, actorID *uuid.UUID, limit, offset int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	entries, err := s.repo.AuditLog(ctx, action, actorID, limit, offset)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return entries, nil
}

// CreateReport is used by the user-facing report endpoint.
func (s *Service) CreateReport(ctx context.Context, reporterID uuid.UUID, targetType string, targetID uuid.UUID, reason, detail string) (uuid.UUID, error) {
	switch targetType {
	case "user", "message", "chat", "story", "article":
	default:
		return uuid.Nil, httpx.Validation("Unsupported report target").
			WithField("target_type", "must be user, message, chat, story or article")
	}
	switch reason {
	case "spam", "violence", "child_abuse", "illegal", "fraud", "other":
	default:
		return uuid.Nil, httpx.Validation("Unsupported report reason").
			WithField("reason", "must be spam, violence, child_abuse, illegal, fraud or other")
	}

	id, err := s.repo.CreateReport(ctx, reporterID, targetType, targetID, reason, detail)
	if err != nil {
		return uuid.Nil, httpx.Internal(err)
	}

	if s.spam != nil {
		// A report names content; the score belongs to whoever produced it.
		// Only the two target types that resolve to a person are scored — a
		// report about a chat or an article is a moderation case, not
		// evidence against an individual.
		switch targetType {
		case "user":
			s.spam.Reported(ctx, targetID, reason)
		case "message":
			if author, err := s.repo.MessageAuthor(ctx, targetID); err == nil && author != nil {
				s.spam.Reported(ctx, *author, reason)
			}
		}
	}
	return id, nil
}

// ---------------------------------------------------------------- handler

type Handler struct {
	service *Service
	// requirePermission is the auth middleware's gate, injected so this package
	// does not depend on the auth package.
	requirePermission func(permission string) func(http.Handler) http.Handler
}

func NewHandler(service *Service, requirePermission func(string) func(http.Handler) http.Handler) *Handler {
	return &Handler{service: service, requirePermission: requirePermission}
}

func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()

	r.With(h.requirePermission(PermAnalyticsRead)).Get("/dashboard", h.dashboard)
	r.With(h.requirePermission(PermAnalyticsRead)).Get("/analytics/{metric}", h.timeSeries)

	r.With(h.requirePermission(PermUsersRead)).Get("/users", h.searchUsers)
	r.With(h.requirePermission(PermUsersWrite)).Put("/users/{userID}/status", h.setUserStatus)
	r.With(h.requirePermission(PermUsersWrite)).Post("/users/{userID}/roles", h.grantRole)
	r.With(h.requirePermission(PermUsersWrite)).Delete("/users/{userID}/roles/{roleKey}", h.revokeRole)

	r.With(h.requirePermission(PermReportsRead)).Get("/reports", h.reports)
	r.With(h.requirePermission(PermReportsWrite)).Put("/reports/{reportID}", h.resolveReport)

	r.With(h.requirePermission(PermBansWrite)).Get("/bans", h.bans)
	r.With(h.requirePermission(PermBansWrite)).Post("/bans", h.createBan)
	r.With(h.requirePermission(PermBansWrite)).Delete("/bans/{banID}", h.liftBan)

	r.With(h.requirePermission(PermFlagsWrite)).Put("/feature-flags/{key}", h.setFlag)
	r.With(h.requirePermission(PermAuditRead)).Get("/audit-log", h.auditLog)

	return r
}

func (h *Handler) actorFrom(r *http.Request) (actor, error) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		return actor{}, err
	}
	return actor{
		principal: principal,
		ip:        httpx.ClientIPFrom(r.Context()),
		userAgent: r.UserAgent(),
	}, nil
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	dashboard, err := h.service.Dashboard(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, dashboard)
}

func (h *Handler) timeSeries(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	points, err := h.service.TimeSeries(r.Context(), chi.URLParam(r, "metric"), days)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"points": points})
}

func (h *Handler) searchUsers(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	users, err := h.service.SearchUsers(r.Context(),
		r.URL.Query().Get("q"), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"users": users})
}

func (h *Handler) setUserStatus(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, parseErr := uuid.Parse(chi.URLParam(r, "userID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}

	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.SetUserStatus(r.Context(), a, userID, body.Status, body.Reason); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) grantRole(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, parseErr := uuid.Parse(chi.URLParam(r, "userID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}

	var body struct {
		Role string `json:"role"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.GrantRole(r.Context(), a, userID, body.Role); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) revokeRole(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	userID, parseErr := uuid.Parse(chi.URLParam(r, "userID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("userID is not a valid UUID"))
		return
	}

	if err := h.service.RevokeRole(r.Context(), a, userID, chi.URLParam(r, "roleKey")); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) reports(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	reports, err := h.service.Reports(r.Context(), r.URL.Query().Get("status"), limit, offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"reports": reports})
}

func (h *Handler) resolveReport(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	reportID, parseErr := uuid.Parse(chi.URLParam(r, "reportID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("reportID is not a valid UUID"))
		return
	}

	var body struct {
		Status     string `json:"status"`
		Resolution string `json:"resolution"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if err := h.service.ResolveReport(r.Context(), a, reportID, body.Status, body.Resolution); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) bans(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	bans, err := h.service.Bans(r.Context(), limit, offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"bans": bans})
}

func (h *Handler) createBan(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		UserID    *uuid.UUID `json:"user_id,omitempty"`
		ChatID    *uuid.UUID `json:"chat_id,omitempty"`
		Scope     string     `json:"scope"`
		Reason    string     `json:"reason"`
		ExpiresAt *time.Time `json:"expires_at,omitempty"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.CreateBan(r.Context(), a, Ban{
		UserID: body.UserID, ChatID: body.ChatID, Scope: body.Scope,
		Reason: body.Reason, ExpiresAt: body.ExpiresAt,
	})
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"ban_id": id})
}

func (h *Handler) liftBan(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	banID, parseErr := uuid.Parse(chi.URLParam(r, "banID"))
	if parseErr != nil {
		httpx.Fail(w, r, httpx.BadRequest("banID is not a valid UUID"))
		return
	}

	if err := h.service.LiftBan(r.Context(), a, banID); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) setFlag(w http.ResponseWriter, r *http.Request) {
	a, err := h.actorFrom(r)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		Enabled        bool `json:"enabled"`
		RolloutPercent int  `json:"rollout_percent"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	if body.RolloutPercent == 0 && body.Enabled {
		body.RolloutPercent = 100
	}

	if err := h.service.SetFlag(r.Context(), a, chi.URLParam(r, "key"),
		body.Enabled, body.RolloutPercent); err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.NoContent(w, r)
}

func (h *Handler) auditLog(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))

	var actorID *uuid.UUID
	if raw := r.URL.Query().Get("actor_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			httpx.Fail(w, r, httpx.BadRequest("actor_id is not a valid UUID"))
			return
		}
		actorID = &id
	}

	entries, err := h.service.AuditLog(r.Context(), r.URL.Query().Get("action"), actorID, limit, offset)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"entries": entries})
}

// ReportRoutes are the user-facing report endpoints (§34).
func (h *Handler) ReportRoutes() http.Handler {
	r := chi.NewRouter()
	r.Post("/", h.createReport)
	return r
}

func (h *Handler) createReport(w http.ResponseWriter, r *http.Request) {
	principal, err := httpx.MustPrincipal(r.Context())
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	var body struct {
		TargetType string    `json:"target_type"`
		TargetID   uuid.UUID `json:"target_id"`
		Reason     string    `json:"reason"`
		Detail     string    `json:"detail"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	id, err := h.service.CreateReport(r.Context(), principal.UserID,
		body.TargetType, body.TargetID, body.Reason, body.Detail)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}
	httpx.JSON(w, r, http.StatusCreated, map[string]any{"report_id": id})
}
