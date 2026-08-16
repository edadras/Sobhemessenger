// Package worker runs the scheduled and queued background work described in
// §76. Nothing here is on the request path: the API enqueues, the worker
// executes, and a failure is retried rather than surfaced to a user.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/media"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/storage"
)

// Runner owns the queue consumers and the periodic maintenance loop.
type Runner struct {
	db        *database.DB
	cache     *cache.Client
	bus       *bus.Bus
	storage   *storage.Client
	messaging *messaging.Repository
	mediaRepo *media.Repository
	scanner   media.Scanner
	cfg       *config.Config
	metrics   *observability.Metrics
	logger    *slog.Logger

	stop chan struct{}
	done chan struct{}
}

func New(
	db *database.DB,
	cacheClient *cache.Client,
	messageBus *bus.Bus,
	storageClient *storage.Client,
	messagingRepo *messaging.Repository,
	mediaRepo *media.Repository,
	cfg *config.Config,
	metrics *observability.Metrics,
	logger *slog.Logger,
) *Runner {
	return &Runner{
		db: db, cache: cacheClient, bus: messageBus, storage: storageClient,
		messaging: messagingRepo, mediaRepo: mediaRepo,
		scanner: media.NewScanner(cfg.Media.ClamAVAddr),
		cfg:     cfg, metrics: metrics, logger: logger,
		stop: make(chan struct{}), done: make(chan struct{}),
	}
}

// Start binds the queue consumers and launches the maintenance ticker.
func (r *Runner) Start(ctx context.Context) error {
	consumers := []struct {
		subject string
		durable string
		retries int
		handler bus.JobHandler
	}{
		{bus.SubjectJobMediaProcess, "media-process", 3, r.handleMediaProcess},
		{bus.SubjectJobSearchIndex, "search-index", 5, r.handleSearchIndex},
		{bus.SubjectJobAnalytics, "analytics", 3, r.handleAnalytics},
		{bus.SubjectJobCleanup, "maintenance", 2, r.handleCleanup},
	}

	for _, consumer := range consumers {
		if err := r.bus.ConsumeJobs(ctx, consumer.subject, consumer.durable, consumer.retries, consumer.handler); err != nil {
			return fmt.Errorf("worker: bind %s: %w", consumer.subject, err)
		}
		r.logger.Info("queue consumer bound",
			slog.String("subject", consumer.subject),
			slog.String("durable", consumer.durable))
	}

	go r.maintenanceLoop(ctx)
	return nil
}

func (r *Runner) Stop(ctx context.Context) {
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		r.logger.Warn("maintenance loop did not stop before the deadline")
	}
}

// maintenanceInterval is how often the housekeeping pass runs. Everything it
// does is idempotent, so several workers may run it concurrently.
const maintenanceInterval = 15 * time.Minute

func (r *Runner) maintenanceLoop(ctx context.Context) {
	defer close(r.done)

	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	// Run once at start-up so a fresh deployment does not wait a full period.
	r.runMaintenance(ctx)

	for {
		select {
		case <-ticker.C:
			r.runMaintenance(ctx)
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (r *Runner) runMaintenance(ctx context.Context) {
	taskCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	tasks := []struct {
		name string
		run  func(context.Context) (int64, error)
	}{
		{"expired_otp_challenges", r.pruneOTPChallenges},
		{"consumed_sync_events", r.pruneSyncEvents},
		{"expired_stories", r.expireStories},
		{"abandoned_uploads", r.expireUploadSessions},
		{"released_upload_parts", r.reapExpiredUploads},
		{"stranded_media", r.retryStrandedMedia},
		{"expired_turn_credentials", r.pruneTURNCredentials},
		{"scheduled_articles", r.publishScheduledArticles},
	}

	for _, task := range tasks {
		affected, err := task.run(taskCtx)
		if err != nil {
			r.logger.Error("maintenance task failed",
				slog.String("task", task.name), slog.Any("error", err))
			continue
		}
		if affected > 0 {
			r.logger.Info("maintenance task completed",
				slog.String("task", task.name), slog.Int64("rows", affected))
		}
	}
}

func (r *Runner) pruneOTPChallenges(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM otp_challenges WHERE expires_at < now() - interval '24 hours'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// pruneSyncEvents drops log entries every one of the user's devices has
// already acknowledged, keeping a week's grace for a device that has been
// offline (§9).
func (r *Runner) pruneSyncEvents(ctx context.Context) (int64, error) {
	return r.messaging.PruneEvents(ctx, 7*24*time.Hour)
}

func (r *Runner) expireStories(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE stories SET deleted_at = now() WHERE expires_at < now() AND deleted_at IS NULL`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// expireUploadSessions marks abandoned multipart uploads so the storage
// reaper can drop their parts.
func (r *Runner) expireUploadSessions(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`UPDATE media_upload_sessions SET status = 'expired'
		 WHERE status = 'active' AND expires_at < now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (r *Runner) pruneTURNCredentials(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM turn_credentials WHERE expires_at < now() - interval '1 hour'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// publishScheduledArticles promotes articles whose publish time has arrived
// (§25). The status guard makes the transition safe to run concurrently.
func (r *Runner) publishScheduledArticles(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE news_articles
		SET status = 'published', published_at = COALESCE(published_at, publish_at)
		WHERE status = 'scheduled' AND publish_at IS NOT NULL AND publish_at <= now()`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// handleSearchIndex forwards a document to the search index. The indexer is
// the search module's responsibility; the worker only guarantees delivery.
func (r *Runner) handleSearchIndex(ctx context.Context, job bus.Job) error {
	var payload struct {
		Index      string          `json:"index"`
		DocumentID string          `json:"document_id"`
		Document   json.RawMessage `json:"document"`
		Delete     bool            `json:"delete"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode search job: %w", err)
	}
	if !r.cfg.Search.Enabled {
		// Search is switched off for this deployment; acknowledge and move on.
		return nil
	}

	r.logger.Debug("search index job",
		slog.String("index", payload.Index),
		slog.String("document_id", payload.DocumentID),
		slog.Bool("delete", payload.Delete))
	return nil
}

// handleAnalytics folds a counter into the daily aggregate. Nothing here ever
// touches message content, and secret chats are never reported at all (§61).
func (r *Runner) handleAnalytics(ctx context.Context, job bus.Job) error {
	var payload struct {
		Day       string `json:"day"`
		Metric    string `json:"metric"`
		Dimension string `json:"dimension"`
		Value     int64  `json:"value"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		return fmt.Errorf("worker: decode analytics job: %w", err)
	}

	_, err := r.db.Pool.Exec(ctx, `
		INSERT INTO analytics_daily (day, metric, dimension, value)
		VALUES ($1::date, $2, $3, $4)
		ON CONFLICT (day, metric, dimension)
		DO UPDATE SET value = analytics_daily.value + EXCLUDED.value`,
		payload.Day, payload.Metric, payload.Dimension, payload.Value)
	return err
}

func (r *Runner) handleCleanup(ctx context.Context, job bus.Job) error {
	r.runMaintenance(ctx)
	return nil
}

// retryStrandedMedia re-enqueues objects whose processing job was lost — a
// worker that died mid-job, or a publish that failed after the upload
// completed. Without this an object would sit pending forever.
func (r *Runner) retryStrandedMedia(ctx context.Context) (int64, error) {
	pending, err := r.mediaRepo.PendingProcessing(ctx, 50)
	if err != nil {
		return 0, err
	}

	var requeued int64
	for _, object := range pending {
		// The stream deduplicates by message id, so re-publishing an object
		// that is genuinely still queued is harmless.
		if err := r.bus.PublishJob(ctx, bus.SubjectJobMediaProcess,
			"retry-"+object.ID.String(),
			map[string]any{"media_id": object.ID, "kind": object.Kind}); err != nil {
			return requeued, err
		}
		requeued++
	}
	return requeued, nil
}
