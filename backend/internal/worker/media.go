package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/media"
)

// maxInMemoryBytes bounds what the worker will decode without spilling to
// disk. Images comfortably fit; video always goes through a temporary file.
const maxInMemoryBytes = 64 << 20

// handleMediaProcess produces the derived variants for one uploaded object
// (§20, §21, §22).
//
// The pipeline is: scan → download → probe → derive → upload variants → mark
// ready. A failure at any point marks the media failed rather than leaving it
// pending forever, so the UI can show the user something definite.
func (r *Runner) handleMediaProcess(ctx context.Context, job bus.Job) error {
	var payload struct {
		MediaID uuid.UUID `json:"media_id"`
		Kind    string    `json:"kind"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode media job: %w", err)
	}

	object, err := r.mediaRepo.ByID(ctx, payload.MediaID)
	if err != nil {
		// The object was deleted between enqueue and execution; nothing to do.
		return nil
	}
	if object.ProcessStatus == "ready" {
		return nil
	}

	if err := r.mediaRepo.MarkProcessing(ctx, object.ID); err != nil {
		return err
	}

	start := time.Now()
	if err := r.processMedia(ctx, object); err != nil {
		r.logger.Error("media processing failed",
			slog.String("media_id", object.ID.String()),
			slog.String("kind", object.Kind),
			slog.Any("error", err))
		if markErr := r.mediaRepo.MarkFailed(ctx, object.ID, err.Error()); markErr != nil {
			r.logger.Error("could not record media failure", slog.Any("error", markErr))
		}
		// The job is not retried: a file that cannot be decoded once will not
		// decode on a second attempt, and the failure is now visible.
		return nil
	}

	r.metrics.MediaProcessing.WithLabelValues(object.Kind).Observe(time.Since(start).Seconds())
	return nil
}

func (r *Runner) processMedia(ctx context.Context, object *media.Media) error {
	if err := r.scanObject(ctx, object); err != nil {
		return err
	}

	switch object.Kind {
	case media.KindImage, media.KindAvatar, media.KindSticker:
		return r.processImage(ctx, object)
	case media.KindVideo, media.KindGIF:
		return r.processVideo(ctx, object)
	case media.KindVoice:
		return r.processVoice(ctx, object)
	case media.KindAudio:
		return r.processAudio(ctx, object)
	default:
		// Documents need no derivation; recording the digest is enough.
		digest, err := r.digest(ctx, object)
		if err != nil {
			return err
		}
		if err := r.mediaRepo.FinalizeMedia(ctx, object.ID, object.MimeType,
			object.SizeBytes, digest, "clean"); err != nil {
			return err
		}
		return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, nil, nil, nil)
	}
}

// scanObject streams the object past the malware scanner before anything else
// touches it (§72).
func (r *Runner) scanObject(ctx context.Context, object *media.Media) error {
	if !r.scanner.Enabled() {
		return nil
	}

	reader, err := r.storage.Get(ctx, object.Bucket, object.ObjectKey)
	if err != nil {
		return fmt.Errorf("worker: fetch for scan: %w", err)
	}
	defer reader.Close()

	result, err := r.scanner.Scan(ctx, reader)
	if err != nil {
		if r.cfg.Media.VirusScanRequired {
			// Scanning is mandatory here, so a scanner outage must not let an
			// unscanned file through.
			return fmt.Errorf("worker: scan failed and scanning is required: %w", err)
		}
		r.logger.Warn("virus scan failed, continuing",
			slog.String("media_id", object.ID.String()), slog.Any("error", err))
		return r.mediaRepo.SetScanResult(ctx, object.ID, "failed", err.Error())
	}

	if result.Status == "infected" {
		r.logger.Warn("infected upload removed",
			slog.String("media_id", object.ID.String()),
			slog.String("signature", result.Detail))
		if err := r.storage.Remove(ctx, object.Bucket, object.ObjectKey); err != nil {
			r.logger.Error("could not remove infected object", slog.Any("error", err))
		}
		// The signature is recorded, not just logged: a log line is gone in a
		// week, and the person who uploaded the file will ask what was wrong
		// with it long after that.
		if err := r.mediaRepo.SetScanResult(ctx, object.ID, "infected", result.Detail); err != nil {
			return err
		}
		return fmt.Errorf("worker: object is infected: %s", result.Detail)
	}

	return r.mediaRepo.SetScanResult(ctx, object.ID, result.Status, result.Detail)
}

func (r *Runner) processImage(ctx context.Context, object *media.Media) error {
	if object.SizeBytes > maxInMemoryBytes {
		return fmt.Errorf("worker: image is too large to process (%d bytes)", object.SizeBytes)
	}

	data, digest, err := r.download(ctx, object)
	if err != nil {
		return err
	}

	variants, width, height, blurhash, err := media.ProcessImage(data)
	if err != nil {
		return err
	}

	for _, variant := range variants {
		key := media.VariantKey(object.ObjectKey, variant.Name, ".jpg")
		if err := r.storage.Put(ctx, object.Bucket, key,
			bytes.NewReader(variant.Data), int64(len(variant.Data)), variant.MimeType); err != nil {
			return fmt.Errorf("worker: store %s variant: %w", variant.Name, err)
		}
		if err := r.mediaRepo.AddVariant(ctx, object.ID, media.Variant{
			Variant:   variant.Name,
			ObjectKey: key,
			MimeType:  variant.MimeType,
			SizeBytes: int64(len(variant.Data)),
			Width:     &variant.Width,
			Height:    &variant.Height,
		}); err != nil {
			return err
		}
	}

	if err := r.mediaRepo.FinalizeMedia(ctx, object.ID, object.MimeType,
		object.SizeBytes, digest, "clean"); err != nil {
		return err
	}
	return r.mediaRepo.MarkReady(ctx, object.ID, &width, &height, nil, nil, &blurhash)
}

func (r *Runner) processVideo(ctx context.Context, object *media.Media) error {
	if !media.FFmpegAvailable() {
		// Without the toolchain the original is still perfectly playable; only
		// the alternate resolutions are missing.
		r.logger.Warn("ffmpeg is not installed; storing the original without variants",
			slog.String("media_id", object.ID.String()))
		return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, nil, nil, nil)
	}

	dir, cleanup, err := media.TempDir()
	if err != nil {
		return err
	}
	defer cleanup()

	sourcePath := filepath.Join(dir, "source"+filepath.Ext(object.ObjectKey))
	digest, err := r.downloadToFile(ctx, object, sourcePath)
	if err != nil {
		return err
	}

	info, err := media.Probe(ctx, sourcePath)
	if err != nil {
		return err
	}

	// Poster frame: one second in, or the midpoint of a very short clip, so
	// the thumbnail is not a black opening frame.
	posterAt := 1000
	if info.DurationMs > 0 && info.DurationMs < 2000 {
		posterAt = info.DurationMs / 2
	}
	posterPath := filepath.Join(dir, "poster.jpg")
	if err := media.ExtractPoster(ctx, sourcePath, posterPath, posterAt); err == nil {
		if err := r.storeImageVariants(ctx, object, posterPath); err != nil {
			return err
		}
	} else {
		r.logger.Warn("could not extract a poster frame",
			slog.String("media_id", object.ID.String()), slog.Any("error", err))
	}

	for _, rendition := range media.RenditionsFor(info.Height) {
		targetPath := filepath.Join(dir, rendition.Name+".mp4")
		if err := media.TranscodeVideo(ctx, sourcePath, targetPath, rendition); err != nil {
			r.logger.Warn("rendition failed, skipping",
				slog.String("media_id", object.ID.String()),
				slog.String("rendition", rendition.Name),
				slog.Any("error", err))
			continue
		}

		key := media.VariantKey(object.ObjectKey, rendition.Name, ".mp4")
		if err := r.uploadFile(ctx, object.Bucket, key, targetPath, "video/mp4"); err != nil {
			return err
		}

		stat, err := os.Stat(targetPath)
		if err != nil {
			return fmt.Errorf("worker: stat rendition: %w", err)
		}
		height := rendition.Height
		if err := r.mediaRepo.AddVariant(ctx, object.ID, media.Variant{
			Variant:   rendition.Name,
			ObjectKey: key,
			MimeType:  "video/mp4",
			SizeBytes: stat.Size(),
			Height:    &height,
		}); err != nil {
			return err
		}
	}

	if err := r.mediaRepo.FinalizeMedia(ctx, object.ID, object.MimeType,
		object.SizeBytes, digest, "clean"); err != nil {
		return err
	}
	return r.mediaRepo.MarkReady(ctx, object.ID, &info.Width, &info.Height, &info.DurationMs, nil, nil)
}

func (r *Runner) processVoice(ctx context.Context, object *media.Media) error {
	if !media.FFmpegAvailable() {
		r.logger.Warn("ffmpeg is not installed; voice message stored without a waveform",
			slog.String("media_id", object.ID.String()))
		return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, nil, nil, nil)
	}

	dir, cleanup, err := media.TempDir()
	if err != nil {
		return err
	}
	defer cleanup()

	sourcePath := filepath.Join(dir, "voice-source")
	digest, err := r.downloadToFile(ctx, object, sourcePath)
	if err != nil {
		return err
	}

	info, err := media.Probe(ctx, sourcePath)
	if err != nil {
		return err
	}

	waveform, err := media.ExtractWaveform(ctx, sourcePath, 64)
	if err != nil {
		r.logger.Warn("could not extract a waveform",
			slog.String("media_id", object.ID.String()), slog.Any("error", err))
	}

	// Normalise to Opus unless the upload already is: re-encoding Opus to
	// Opus only loses quality.
	if info.AudioCodec != "opus" {
		opusPath := filepath.Join(dir, "voice.ogg")
		if err := media.TranscodeVoice(ctx, sourcePath, opusPath); err == nil {
			key := media.VariantKey(object.ObjectKey, "opus", ".ogg")
			if err := r.uploadFile(ctx, object.Bucket, key, opusPath, "audio/ogg"); err != nil {
				return err
			}
			if stat, err := os.Stat(opusPath); err == nil {
				if err := r.mediaRepo.AddVariant(ctx, object.ID, media.Variant{
					Variant:   "opus",
					ObjectKey: key,
					MimeType:  "audio/ogg",
					SizeBytes: stat.Size(),
				}); err != nil {
					return err
				}
			}
		}
	}

	if err := r.mediaRepo.FinalizeMedia(ctx, object.ID, object.MimeType,
		object.SizeBytes, digest, "clean"); err != nil {
		return err
	}
	return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, &info.DurationMs, waveform, nil)
}

func (r *Runner) processAudio(ctx context.Context, object *media.Media) error {
	if !media.FFmpegAvailable() {
		return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, nil, nil, nil)
	}

	dir, cleanup, err := media.TempDir()
	if err != nil {
		return err
	}
	defer cleanup()

	sourcePath := filepath.Join(dir, "audio-source")
	digest, err := r.downloadToFile(ctx, object, sourcePath)
	if err != nil {
		return err
	}

	info, err := media.Probe(ctx, sourcePath)
	if err != nil {
		return err
	}

	if err := r.mediaRepo.FinalizeMedia(ctx, object.ID, object.MimeType,
		object.SizeBytes, digest, "clean"); err != nil {
		return err
	}
	return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, &info.DurationMs, nil, nil)
}

// storeImageVariants derives the thumbnail set from a local image file, used
// for video poster frames.
func (r *Runner) storeImageVariants(ctx context.Context, object *media.Media, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("worker: read poster: %w", err)
	}

	variants, _, _, blurhash, err := media.ProcessImage(data)
	if err != nil {
		return err
	}

	for _, variant := range variants {
		key := media.VariantKey(object.ObjectKey, variant.Name, ".jpg")
		if err := r.storage.Put(ctx, object.Bucket, key,
			bytes.NewReader(variant.Data), int64(len(variant.Data)), variant.MimeType); err != nil {
			return fmt.Errorf("worker: store poster variant: %w", err)
		}
		if err := r.mediaRepo.AddVariant(ctx, object.ID, media.Variant{
			Variant:   variant.Name,
			ObjectKey: key,
			MimeType:  variant.MimeType,
			SizeBytes: int64(len(variant.Data)),
			Width:     &variant.Width,
			Height:    &variant.Height,
		}); err != nil {
			return err
		}
	}

	if blurhash != "" {
		return r.mediaRepo.MarkReady(ctx, object.ID, nil, nil, nil, nil, &blurhash)
	}
	return nil
}

// download fetches an object into memory and computes its digest in one pass.
func (r *Runner) download(ctx context.Context, object *media.Media) ([]byte, []byte, error) {
	reader, err := r.storage.Get(ctx, object.Bucket, object.ObjectKey)
	if err != nil {
		return nil, nil, fmt.Errorf("worker: fetch object: %w", err)
	}
	defer reader.Close()

	hasher := sha256.New()
	var buffer bytes.Buffer
	if _, err := io.Copy(io.MultiWriter(&buffer, hasher), io.LimitReader(reader, maxInMemoryBytes)); err != nil {
		return nil, nil, fmt.Errorf("worker: read object: %w", err)
	}
	return buffer.Bytes(), hasher.Sum(nil), nil
}

// downloadToFile streams an object to disk, hashing as it goes so a large
// video is never held in memory.
func (r *Runner) downloadToFile(ctx context.Context, object *media.Media, path string) ([]byte, error) {
	reader, err := r.storage.Get(ctx, object.Bucket, object.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("worker: fetch object: %w", err)
	}
	defer reader.Close()

	file, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("worker: create temp file: %w", err)
	}
	defer file.Close()

	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hasher), reader); err != nil {
		return nil, fmt.Errorf("worker: write temp file: %w", err)
	}
	return hasher.Sum(nil), nil
}

func (r *Runner) uploadFile(ctx context.Context, bucket, key, path, contentType string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("worker: open rendition: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("worker: stat rendition: %w", err)
	}
	return r.storage.Put(ctx, bucket, key, file, stat.Size(), contentType)
}

// digest computes a SHA-256 over an object without keeping it in memory.
func (r *Runner) digest(ctx context.Context, object *media.Media) ([]byte, error) {
	reader, err := r.storage.Get(ctx, object.Bucket, object.ObjectKey)
	if err != nil {
		return nil, fmt.Errorf("worker: fetch for digest: %w", err)
	}
	defer reader.Close()

	hasher := sha256.New()
	if _, err := io.Copy(hasher, reader); err != nil {
		return nil, fmt.Errorf("worker: hash object: %w", err)
	}
	return hasher.Sum(nil), nil
}

// reapExpiredUploads releases storage held by abandoned multipart uploads.
func (r *Runner) reapExpiredUploads(ctx context.Context) (int64, error) {
	sessions, err := r.mediaRepo.ExpiredSessions(ctx, 100)
	if err != nil {
		return 0, err
	}

	var released int64
	for _, session := range sessions {
		if err := r.storage.AbortMultipartUpload(ctx, session.Bucket, session.ObjectKey, session.UploadID); err != nil {
			r.logger.Warn("could not abort expired upload",
				slog.String("session_id", session.ID.String()), slog.Any("error", err))
		}
		if err := r.mediaRepo.DeleteSession(ctx, session.ID); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}
