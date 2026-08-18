package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/storage"
)

var ErrNotFound = errors.New("media: not found")

// Media is a stored object plus everything the clients need to render it.
type Media struct {
	ID         uuid.UUID  `json:"id"`
	OwnerID    *uuid.UUID `json:"owner_id,omitempty"`
	Kind       string     `json:"kind"`
	MimeType   string     `json:"mime_type"`
	FileName   string     `json:"file_name,omitempty"`
	SizeBytes  int64      `json:"size_bytes"`
	Width      *int       `json:"width,omitempty"`
	Height     *int       `json:"height,omitempty"`
	DurationMs *int       `json:"duration_ms,omitempty"`
	Waveform   []int16    `json:"waveform,omitempty"`
	Blurhash   *string    `json:"blurhash,omitempty"`
	ScanStatus string     `json:"scan_status"`
	// ScanDetail is the signature an infected file matched, or why a scan
	// could not be completed. Empty for everything that scanned clean.
	ScanDetail    string          `json:"scan_detail,omitempty"`
	ProcessStatus string          `json:"process_status"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ReadyAt       *time.Time      `json:"ready_at,omitempty"`
	Variants      []Variant       `json:"variants,omitempty"`

	// Internal: never serialised to a client, which only ever sees a
	// presigned URL.
	Bucket    string `json:"-"`
	ObjectKey string `json:"-"`
}

// Variant is a derived rendition — a thumbnail, or a transcoded resolution.
type Variant struct {
	Variant     string `json:"variant"`
	MimeType    string `json:"mime_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Width       *int   `json:"width,omitempty"`
	Height      *int   `json:"height,omitempty"`
	BitrateKbps *int   `json:"bitrate_kbps,omitempty"`
	ObjectKey   string `json:"-"`
}

// UploadSession tracks a multipart upload in progress.
type UploadSession struct {
	ID           uuid.UUID `json:"id"`
	UserID       uuid.UUID `json:"-"`
	MediaID      uuid.UUID `json:"media_id"`
	Bucket       string    `json:"-"`
	ObjectKey    string    `json:"-"`
	UploadID     string    `json:"-"`
	Kind         string    `json:"kind"`
	DeclaredMIME string    `json:"mime_type"`
	FileName     string    `json:"file_name"`
	TotalSize    int64     `json:"total_size"`
	PartSize     int64     `json:"part_size"`
	TotalParts   int       `json:"total_parts"`
	Status       string    `json:"status"`
	ExpiresAt    time.Time `json:"expires_at"`
}

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }

// CreateUploadSession reserves the media row and the session together, so a
// session can never reference a media object that does not exist.
func (r *Repository) CreateUploadSession(ctx context.Context, s *UploadSession) error {
	return r.db.InTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO media (owner_id, bucket, object_key, kind, mime_type, declared_mime,
			                   file_name, size_bytes, process_status)
			VALUES ($1, $2, $3, $4, $5, $5, $6, $7, 'pending')
			RETURNING id`,
			s.UserID, s.Bucket, s.ObjectKey, s.Kind, s.DeclaredMIME, s.FileName, s.TotalSize,
		).Scan(&s.MediaID)
		if err != nil {
			return fmt.Errorf("media: insert media row: %w", err)
		}

		return tx.QueryRow(ctx, `
			INSERT INTO media_upload_sessions (
				user_id, media_id, bucket, object_key, upload_id, kind, declared_mime,
				file_name, total_size, part_size, total_parts, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id`,
			s.UserID, s.MediaID, s.Bucket, s.ObjectKey, s.UploadID, s.Kind, s.DeclaredMIME,
			s.FileName, s.TotalSize, s.PartSize, s.TotalParts, s.ExpiresAt,
		).Scan(&s.ID)
	})
}

// UploadSessionByID loads a session, scoped to its owner so one user can never
// drive another's upload.
func (r *Repository) UploadSessionByID(ctx context.Context, id, userID uuid.UUID) (*UploadSession, error) {
	s := &UploadSession{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, user_id, media_id, bucket, object_key, upload_id, kind, declared_mime,
		       file_name, total_size, part_size, total_parts, status, expires_at
		FROM media_upload_sessions
		WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&s.ID, &s.UserID, &s.MediaID, &s.Bucket, &s.ObjectKey, &s.UploadID, &s.Kind,
		&s.DeclaredMIME, &s.FileName, &s.TotalSize, &s.PartSize, &s.TotalParts,
		&s.Status, &s.ExpiresAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("media: read upload session: %w", err)
	}
	return s, nil
}

// RecordPart stores one uploaded chunk's ETag. Re-uploading a part simply
// overwrites its record, which is what makes a resumed upload work.
func (r *Repository) RecordPart(ctx context.Context, sessionID uuid.UUID, part storage.Part) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO media_chunks (session_id, part_number, etag, size_bytes)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id, part_number)
		DO UPDATE SET etag = EXCLUDED.etag, size_bytes = EXCLUDED.size_bytes, uploaded_at = now()`,
		sessionID, part.PartNumber, part.ETag, part.Size)
	if err != nil {
		return fmt.Errorf("media: record part: %w", err)
	}
	return nil
}

func (r *Repository) Parts(ctx context.Context, sessionID uuid.UUID) ([]storage.Part, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT part_number, etag, size_bytes FROM media_chunks
		WHERE session_id = $1 ORDER BY part_number`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("media: list parts: %w", err)
	}
	defer rows.Close()

	var parts []storage.Part
	for rows.Next() {
		var part storage.Part
		if err := rows.Scan(&part.PartNumber, &part.ETag, &part.Size); err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	return parts, rows.Err()
}

func (r *Repository) SetSessionStatus(ctx context.Context, sessionID uuid.UUID, status string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE media_upload_sessions
		SET status = $2, completed_at = CASE WHEN $2 = 'completed' THEN now() ELSE completed_at END
		WHERE id = $1`, sessionID, status)
	return err
}

// FinalizeMedia records the verified facts about an object once its bytes are
// in place.
func (r *Repository) FinalizeMedia(ctx context.Context, mediaID uuid.UUID, mime string, size int64, sha256 []byte, scanStatus string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE media
		SET mime_type = $2, size_bytes = $3, sha256 = $4, scan_status = $5,
		    process_status = 'pending'
		WHERE id = $1`, mediaID, mime, size, sha256, scanStatus)
	if err != nil {
		return fmt.Errorf("media: finalize: %w", err)
	}
	return nil
}

// SetScanResult records the outcome of a virus scan and what the scanner said.
//
// `scan_detail` holds the signature name for an infected file and the reason
// for a failed scan. Without it an upload rejected as infected can only be
// reported as "rejected" — which is no help to the person who uploaded a file
// they believe is clean, and no help to whoever has to decide whether the
// scanner is right.
func (r *Repository) SetScanResult(ctx context.Context, mediaID uuid.UUID, status, detail string) error {
	// Truncated: a scanner that returns a page of output should not be able to
	// write a page into every row.
	if len(detail) > 500 {
		detail = detail[:500]
	}
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE media SET scan_status = $2, scan_detail = $3 WHERE id = $1`,
		mediaID, status, detail)
	if err != nil {
		return fmt.Errorf("media: set scan result: %w", err)
	}
	return nil
}

// MarkReady is called by the worker once every variant exists.
func (r *Repository) MarkReady(ctx context.Context, mediaID uuid.UUID, width, height, durationMs *int, waveform []int16, blurhash *string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE media
		SET process_status = 'ready', ready_at = now(),
		    width = COALESCE($2, width),
		    height = COALESCE($3, height),
		    duration_ms = COALESCE($4, duration_ms),
		    waveform = COALESCE($5, waveform),
		    blurhash = COALESCE($6, blurhash)
		WHERE id = $1`, mediaID, width, height, durationMs, waveform, blurhash)
	return err
}

func (r *Repository) MarkProcessing(ctx context.Context, mediaID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx,
		`UPDATE media SET process_status = 'processing' WHERE id = $1`, mediaID)
	return err
}

func (r *Repository) MarkFailed(ctx context.Context, mediaID uuid.UUID, reason string) error {
	_, err := r.db.Pool.Exec(ctx, `
		UPDATE media SET process_status = 'failed',
		    metadata = metadata || jsonb_build_object('error', $2::text)
		WHERE id = $1`, mediaID, reason)
	return err
}

func (r *Repository) AddVariant(ctx context.Context, mediaID uuid.UUID, v Variant) error {
	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO media_variants (media_id, variant, object_key, mime_type,
		                            size_bytes, width, height, bitrate_kbps)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (media_id, variant) DO UPDATE
		SET object_key = EXCLUDED.object_key, mime_type = EXCLUDED.mime_type,
		    size_bytes = EXCLUDED.size_bytes, width = EXCLUDED.width,
		    height = EXCLUDED.height, bitrate_kbps = EXCLUDED.bitrate_kbps`,
		mediaID, v.Variant, v.ObjectKey, v.MimeType, v.SizeBytes, v.Width, v.Height, v.BitrateKbps)
	if err != nil {
		return fmt.Errorf("media: add variant: %w", err)
	}
	return nil
}

func (r *Repository) ByID(ctx context.Context, id uuid.UUID) (*Media, error) {
	m := &Media{}
	err := r.db.Pool.QueryRow(ctx, `
		SELECT id, owner_id, bucket, object_key, kind, mime_type, file_name, size_bytes,
		       width, height, duration_ms, waveform, blurhash, scan_status, scan_detail,
		       process_status,
		       metadata, created_at, ready_at
		FROM media WHERE id = $1 AND deleted_at IS NULL`, id,
	).Scan(&m.ID, &m.OwnerID, &m.Bucket, &m.ObjectKey, &m.Kind, &m.MimeType, &m.FileName,
		&m.SizeBytes, &m.Width, &m.Height, &m.DurationMs, &m.Waveform, &m.Blurhash,
		&m.ScanStatus, &m.ScanDetail, &m.ProcessStatus, &m.Metadata, &m.CreatedAt, &m.ReadyAt)
	if database.IsNoRows(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("media: read media: %w", err)
	}

	variants, err := r.variants(ctx, id)
	if err != nil {
		return nil, err
	}
	m.Variants = variants
	return m, nil
}

func (r *Repository) variants(ctx context.Context, mediaID uuid.UUID) ([]Variant, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT variant, object_key, mime_type, size_bytes, width, height, bitrate_kbps
		FROM media_variants WHERE media_id = $1 ORDER BY variant`, mediaID)
	if err != nil {
		return nil, fmt.Errorf("media: list variants: %w", err)
	}
	defer rows.Close()

	var out []Variant
	for rows.Next() {
		var v Variant
		if err := rows.Scan(&v.Variant, &v.ObjectKey, &v.MimeType, &v.SizeBytes,
			&v.Width, &v.Height, &v.BitrateKbps); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// PendingProcessing lists objects the worker still has to handle, so a restart
// picks up work that was in flight.
func (r *Repository) PendingProcessing(ctx context.Context, limit int) ([]Media, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, owner_id, bucket, object_key, kind, mime_type, file_name, size_bytes
		FROM media
		WHERE process_status = 'pending' AND scan_status IN ('clean', 'skipped')
		  AND deleted_at IS NULL
		ORDER BY created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("media: list pending: %w", err)
	}
	defer rows.Close()

	var out []Media
	for rows.Next() {
		var m Media
		if err := rows.Scan(&m.ID, &m.OwnerID, &m.Bucket, &m.ObjectKey, &m.Kind,
			&m.MimeType, &m.FileName, &m.SizeBytes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SoftDelete hides an object; the storage reaper removes the bytes later.
func (r *Repository) SoftDelete(ctx context.Context, mediaID, ownerID uuid.UUID) error {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE media SET deleted_at = now() WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL`,
		mediaID, ownerID)
	if err != nil {
		return fmt.Errorf("media: delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ExpiredSessions lists abandoned uploads whose storage parts should be
// released.
func (r *Repository) ExpiredSessions(ctx context.Context, limit int) ([]UploadSession, error) {
	rows, err := r.db.Pool.Query(ctx, `
		SELECT id, user_id, media_id, bucket, object_key, upload_id, kind, declared_mime,
		       file_name, total_size, part_size, total_parts, status, expires_at
		FROM media_upload_sessions
		WHERE status = 'expired'
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("media: list expired sessions: %w", err)
	}
	defer rows.Close()

	var out []UploadSession
	for rows.Next() {
		var s UploadSession
		if err := rows.Scan(&s.ID, &s.UserID, &s.MediaID, &s.Bucket, &s.ObjectKey, &s.UploadID,
			&s.Kind, &s.DeclaredMIME, &s.FileName, &s.TotalSize, &s.PartSize, &s.TotalParts,
			&s.Status, &s.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *Repository) DeleteSession(ctx context.Context, sessionID uuid.UUID) error {
	_, err := r.db.Pool.Exec(ctx, `DELETE FROM media_upload_sessions WHERE id = $1`, sessionID)
	return err
}
