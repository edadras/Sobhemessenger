package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/storage"
)

// headerBytes is how much of an object is read back to identify it. Every
// signature the platform knows sits well inside this window, so validation
// costs one small ranged read regardless of file size.
const headerBytes = 512

type Service struct {
	repo      *Repository
	storage   *storage.Client
	validator *Validator
	bus       *bus.Bus
	limiter   *ratelimit.Limiter
	rules     ratelimit.Rules
	cfg       config.Media
	metrics   *observability.Metrics
	logger    *slog.Logger
}

func NewService(
	repo *Repository,
	storageClient *storage.Client,
	messageBus *bus.Bus,
	limiter *ratelimit.Limiter,
	rules ratelimit.Rules,
	cfg config.Media,
	metrics *observability.Metrics,
	logger *slog.Logger,
) *Service {
	return &Service{
		repo: repo, storage: storageClient, validator: NewValidator(cfg),
		bus: messageBus, limiter: limiter, rules: rules, cfg: cfg,
		metrics: metrics, logger: logger,
	}
}

// CreateUploadInput describes the file a client wants to upload.
type CreateUploadInput struct {
	UserID   uuid.UUID
	Kind     string
	MimeType string
	FileName string
	Size     int64
}

// PartUpload is one presigned chunk the client PUTs directly to storage.
type PartUpload struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
	Size       int64  `json:"size"`
}

// CreateUploadResult is everything the client needs to perform the upload
// without ever seeing a storage credential.
type CreateUploadResult struct {
	SessionID uuid.UUID    `json:"session_id"`
	MediaID   uuid.UUID    `json:"media_id"`
	PartSize  int64        `json:"part_size"`
	Parts     []PartUpload `json:"parts"`
	ExpiresAt time.Time    `json:"expires_at"`
}

// CreateUpload opens a resumable upload (§19).
func (s *Service) CreateUpload(ctx context.Context, in CreateUploadInput) (*CreateUploadResult, error) {
	if err := s.validator.CheckRequest(in.Kind, in.MimeType, in.FileName, in.Size); err != nil {
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			return nil, httpx.Validation("The file was rejected").
				WithField(validationErr.Field, validationErr.Reason)
		}
		return nil, httpx.Internal(err)
	}

	allowed, err := s.limiter.Allow(ctx, s.rules.UploadsPerUser, in.UserID.String())
	if err != nil {
		s.logger.Warn("upload rate limiter unavailable", slog.Any("error", err))
	}
	if !allowed.Allowed {
		return nil, httpx.RateLimited(int(allowed.RetryAfter.Seconds()))
	}

	objectKey := buildObjectKey(in.UserID, in.Kind, in.FileName)
	partSize := s.cfg.PartSize
	totalParts := int((in.Size + partSize - 1) / partSize)
	if totalParts < 1 {
		totalParts = 1
	}

	// S3 multipart uploads cap out at 10,000 parts; grow the part size rather
	// than refuse a large file.
	for totalParts > 10000 {
		partSize *= 2
		totalParts = int((in.Size + partSize - 1) / partSize)
	}

	uploadID, err := s.storage.NewMultipartUpload(ctx, s.storage.MediaBucket(), objectKey,
		ServableContentType(in.MimeType))
	if err != nil {
		return nil, httpx.Internal(err)
	}

	session := &UploadSession{
		UserID:       in.UserID,
		Bucket:       s.storage.MediaBucket(),
		ObjectKey:    objectKey,
		UploadID:     uploadID,
		Kind:         in.Kind,
		DeclaredMIME: in.MimeType,
		FileName:     in.FileName,
		TotalSize:    in.Size,
		PartSize:     partSize,
		TotalParts:   totalParts,
		ExpiresAt:    time.Now().Add(s.cfg.UploadSessionTTL),
	}

	if err := s.repo.CreateUploadSession(ctx, session); err != nil {
		// Do not leave orphaned parts behind if the database write failed.
		_ = s.storage.AbortMultipartUpload(ctx, session.Bucket, objectKey, uploadID)
		return nil, httpx.Internal(err)
	}

	parts, err := s.presignParts(ctx, session)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	return &CreateUploadResult{
		SessionID: session.ID,
		MediaID:   session.MediaID,
		PartSize:  partSize,
		Parts:     parts,
		ExpiresAt: session.ExpiresAt,
	}, nil
}

func (s *Service) presignParts(ctx context.Context, session *UploadSession) ([]PartUpload, error) {
	parts := make([]PartUpload, 0, session.TotalParts)
	remaining := session.TotalSize

	for number := 1; number <= session.TotalParts; number++ {
		url, err := s.storage.PresignPart(ctx, session.Bucket, session.ObjectKey,
			session.UploadID, number, s.cfg.PresignTTL)
		if err != nil {
			return nil, err
		}
		size := session.PartSize
		if remaining < size {
			size = remaining
		}
		remaining -= size
		parts = append(parts, PartUpload{PartNumber: number, URL: url, Size: size})
	}
	return parts, nil
}

// RefreshParts re-presigns the parts of an in-progress upload, so a client that
// paused for longer than the URL lifetime can resume rather than restart.
func (s *Service) RefreshParts(ctx context.Context, sessionID, userID uuid.UUID) (*CreateUploadResult, error) {
	session, err := s.loadActiveSession(ctx, sessionID, userID)
	if err != nil {
		return nil, err
	}

	parts, err := s.presignParts(ctx, session)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	return &CreateUploadResult{
		SessionID: session.ID,
		MediaID:   session.MediaID,
		PartSize:  session.PartSize,
		Parts:     parts,
		ExpiresAt: session.ExpiresAt,
	}, nil
}

// RecordPart notes that a chunk landed. The client reports the ETag storage
// returned; completion verifies the set is whole.
func (s *Service) RecordPart(ctx context.Context, sessionID, userID uuid.UUID, part storage.Part) error {
	session, err := s.loadActiveSession(ctx, sessionID, userID)
	if err != nil {
		return err
	}
	if part.PartNumber < 1 || part.PartNumber > session.TotalParts {
		return httpx.Validation("Part number is out of range").
			WithField("part_number", fmt.Sprintf("must be between 1 and %d", session.TotalParts))
	}
	if strings.TrimSpace(part.ETag) == "" {
		return httpx.Validation("ETag is required").WithField("etag", "required")
	}

	if err := s.repo.RecordPart(ctx, sessionID, part); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// CompleteUpload assembles the object and validates what actually landed.
func (s *Service) CompleteUpload(ctx context.Context, sessionID, userID uuid.UUID) (*Media, error) {
	session, err := s.loadActiveSession(ctx, sessionID, userID)
	if err != nil {
		return nil, err
	}

	parts, err := s.repo.Parts(ctx, sessionID)
	if err != nil {
		return nil, httpx.Internal(err)
	}
	if len(parts) != session.TotalParts {
		return nil, (&httpx.Error{
			Status:  400,
			Code:    httpx.CodeUploadIncomplete,
			Message: "Some parts of this upload are missing",
		}).WithField("parts", fmt.Sprintf("received %d of %d", len(parts), session.TotalParts))
	}

	if err := s.storage.CompleteMultipartUpload(ctx, session.Bucket, session.ObjectKey,
		session.UploadID, parts); err != nil {
		return nil, httpx.Internal(err)
	}

	// Now that the object exists, check what it really is. Everything before
	// this point was the client's word.
	info, err := s.storage.Stat(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	header, err := s.readHeader(ctx, session.Bucket, session.ObjectKey)
	if err != nil {
		return nil, httpx.Internal(err)
	}

	actualMIME, err := s.validator.CheckContent(session.Kind, session.DeclaredMIME, header)
	if err != nil {
		var validationErr *ValidationError
		if errors.As(err, &validationErr) {
			// The bytes are not what was promised: remove them rather than
			// keep an object nobody vouched for.
			_ = s.storage.Remove(ctx, session.Bucket, session.ObjectKey)
			_ = s.repo.SetSessionStatus(ctx, sessionID, "aborted")
			s.metrics.MediaUploads.WithLabelValues(session.Kind, "rejected").Inc()
			return nil, (&httpx.Error{
				Status:  422,
				Code:    httpx.CodeFileRejected,
				Message: "The uploaded file was rejected",
			}).WithField(validationErr.Field, validationErr.Reason)
		}
		return nil, httpx.Internal(err)
	}

	// Scanning happens in the worker; until it reports back, the object is
	// pending rather than clean.
	scanStatus := "skipped"
	if s.cfg.ClamAVAddr != "" {
		scanStatus = "pending"
	}

	if err := s.repo.FinalizeMedia(ctx, session.MediaID, actualMIME, info.Size, nil, scanStatus); err != nil {
		return nil, httpx.Internal(err)
	}
	if err := s.repo.SetSessionStatus(ctx, sessionID, "completed"); err != nil {
		s.logger.Warn("failed to close upload session", slog.Any("error", err))
	}

	if err := s.bus.PublishJob(ctx, bus.SubjectJobMediaProcess, session.MediaID.String(),
		map[string]any{"media_id": session.MediaID, "kind": session.Kind}); err != nil {
		// Processing is best-effort at this point: the object is stored and
		// the sweeper will pick up anything left pending.
		s.logger.Error("failed to enqueue media processing",
			slog.String("media_id", session.MediaID.String()), slog.Any("error", err))
	}

	s.metrics.MediaUploads.WithLabelValues(session.Kind, "accepted").Inc()
	s.metrics.MediaBytes.WithLabelValues(session.Kind).Add(float64(info.Size))

	return s.Get(ctx, session.MediaID)
}

// AbortUpload discards an upload and its parts.
func (s *Service) AbortUpload(ctx context.Context, sessionID, userID uuid.UUID) error {
	session, err := s.repo.UploadSessionByID(ctx, sessionID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Upload session not found")
		}
		return httpx.Internal(err)
	}

	if err := s.storage.AbortMultipartUpload(ctx, session.Bucket, session.ObjectKey, session.UploadID); err != nil {
		s.logger.Warn("failed to abort multipart upload", slog.Any("error", err))
	}
	if err := s.repo.SetSessionStatus(ctx, sessionID, "aborted"); err != nil {
		return httpx.Internal(err)
	}
	return nil
}

// Get returns metadata for an object.
func (s *Service) Get(ctx context.Context, mediaID uuid.UUID) (*Media, error) {
	m, err := s.repo.ByID(ctx, mediaID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Media not found")
		}
		return nil, httpx.Internal(err)
	}
	return m, nil
}

// DownloadURL issues a short-lived URL for an object or one of its variants.
func (s *Service) DownloadURL(ctx context.Context, mediaID uuid.UUID, variant string) (string, error) {
	m, err := s.Get(ctx, mediaID)
	if err != nil {
		return "", err
	}

	if m.ScanStatus == "infected" {
		return "", httpx.Forbidden(httpx.CodeVirusDetected, "This file was found to be malicious")
	}

	objectKey := m.ObjectKey
	if variant != "" && variant != "original" {
		found := false
		for _, v := range m.Variants {
			if v.Variant == variant {
				objectKey = v.ObjectKey
				found = true
				break
			}
		}
		if !found {
			return "", httpx.NotFound(httpx.CodeNotFound, "That variant does not exist")
		}
	}

	url, err := s.storage.PresignGet(ctx, m.Bucket, objectKey, m.FileName, s.cfg.PresignTTL)
	if err != nil {
		return "", httpx.Internal(err)
	}
	return url, nil
}

// Delete removes an object the caller owns.
func (s *Service) Delete(ctx context.Context, mediaID, userID uuid.UUID) error {
	if err := s.repo.SoftDelete(ctx, mediaID, userID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound(httpx.CodeNotFound, "Media not found")
		}
		return httpx.Internal(err)
	}
	return nil
}

func (s *Service) loadActiveSession(ctx context.Context, sessionID, userID uuid.UUID) (*UploadSession, error) {
	session, err := s.repo.UploadSessionByID(ctx, sessionID, userID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, httpx.NotFound(httpx.CodeNotFound, "Upload session not found")
		}
		return nil, httpx.Internal(err)
	}
	if session.Status != "active" {
		return nil, httpx.Conflict(httpx.CodeUploadExpired,
			fmt.Sprintf("This upload session is %s", session.Status))
	}
	if time.Now().After(session.ExpiresAt) {
		_ = s.repo.SetSessionStatus(ctx, sessionID, "expired")
		return nil, httpx.Conflict(httpx.CodeUploadExpired, "This upload session has expired")
	}
	return session, nil
}

func (s *Service) readHeader(ctx context.Context, bucket, key string) ([]byte, error) {
	reader, err := s.storage.GetRange(ctx, bucket, key, 0, headerBytes)
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	header := make([]byte, headerBytes)
	n, err := io.ReadFull(reader, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return header[:n], nil
}

// buildObjectKey spreads objects across date-based prefixes so no single
// storage prefix accumulates every upload, and gives each a random name so a
// key can never be guessed from a filename.
func buildObjectKey(userID uuid.UUID, kind, fileName string) string {
	now := time.Now().UTC()
	extension := strings.ToLower(filepath.Ext(fileName))
	if len(extension) > 10 {
		extension = ""
	}
	return fmt.Sprintf("%s/%04d/%02d/%02d/%s%s",
		kind, now.Year(), now.Month(), now.Day(), uuid.NewString(), extension)
}
