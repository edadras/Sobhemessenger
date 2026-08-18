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

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/antispam"
	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/datarights"
	"github.com/sobh/messenger/backend/internal/media"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/notifications"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/search"
	"github.com/sobh/messenger/backend/internal/storage"
)

// Runner owns the queue consumers and the periodic maintenance loop.
type Runner struct {
	db        *database.DB
	cache     *cache.Client
	bus       *bus.Bus
	storage   *storage.Client
	messaging *messaging.Repository
	// messagingSvc publishes scheduled messages, which needs the broadcast
	// the service owns rather than the repository alone.
	messagingSvc *messaging.Service
	mediaRepo    *media.Repository
	scanner      media.Scanner
	search       *search.Client

	notificationsRepo *notifications.Repository
	notificationsSvc  *notifications.Service
	pushSenders       map[string]notifications.Sender

	cfg     *config.Config
	metrics *observability.Metrics
	logger  *slog.Logger

	stop chan struct{}
	done chan struct{}
	// schedulerDone is separate from done because the two loops stop
	// independently and Stop waits for both.
	schedulerDone chan struct{}
}

func New(
	db *database.DB,
	cacheClient *cache.Client,
	messageBus *bus.Bus,
	storageClient *storage.Client,
	messagingRepo *messaging.Repository,
	messagingSvc *messaging.Service,
	mediaRepo *media.Repository,
	notificationsRepo *notifications.Repository,
	notificationsSvc *notifications.Service,
	searchClient *search.Client,
	cfg *config.Config,
	metrics *observability.Metrics,
	logger *slog.Logger,
) *Runner {
	return &Runner{
		db: db, cache: cacheClient, bus: messageBus, storage: storageClient,
		messaging: messagingRepo, messagingSvc: messagingSvc, mediaRepo: mediaRepo,
		scanner: media.NewScanner(cfg.Media.ClamAVAddr),
		search:  searchClient,

		notificationsRepo: notificationsRepo,
		notificationsSvc:  notificationsSvc,
		pushSenders:       notifications.NewSenders(cfg.Push),

		cfg: cfg, metrics: metrics, logger: logger,
		stop: make(chan struct{}), done: make(chan struct{}),
		schedulerDone: make(chan struct{}),
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
		{bus.SubjectJobPushSend, "push-send", 4, r.handlePushSend},
		{bus.SubjectJobNewsPublish, "news-publish", 3, r.handleNewsPublish},
		{bus.SubjectJobSearchIndex, "search-index", 5, r.handleSearchIndexing},
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
	go r.schedulerLoop(ctx)
	return nil
}

func (r *Runner) Stop(ctx context.Context) {
	close(r.stop)

	// Both loops are waited on: returning while the publisher is mid-batch
	// would cut a transaction short during a deploy.
	for _, loop := range []struct {
		name string
		done <-chan struct{}
	}{
		{"maintenance", r.done},
		{"scheduler", r.schedulerDone},
	} {
		select {
		case <-loop.done:
		case <-ctx.Done():
			r.logger.Warn("a worker loop did not stop before the deadline",
				slog.String("loop", loop.name))
			return
		}
	}
}

// maintenanceInterval is how often the housekeeping pass runs. Everything it
// does is idempotent, so several workers may run it concurrently.
const maintenanceInterval = 15 * time.Minute

// autoDeleteBatch bounds how many expired messages one maintenance run clears.
//
// A chat with a short timer and a long history could otherwise fill a whole
// tick by itself and starve every other task. The next tick continues from
// where this one stopped, so the bound delays the sweep rather than skipping
// anything.
const autoDeleteBatch = 5000

// schedulerInterval is how often due messages are published.
//
// It is far shorter than the maintenance pass because the delay is visible to
// a user: a message scheduled for 10:00 that arrives at 10:14 is late, while
// housekeeping that runs a quarter of an hour after it could have is not.
const schedulerInterval = 15 * time.Second

// schedulerBatch bounds one pass, so a large backlog is drained steadily
// instead of in a single long transaction.
const schedulerBatch = 200

// schedulerLoop publishes scheduled messages when their time comes.
//
// Several workers may run this at once: the claim uses FOR UPDATE SKIP LOCKED,
// so each message is published exactly once no matter how many are polling.
func (r *Runner) schedulerLoop(ctx context.Context) {
	defer close(r.schedulerDone)

	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			published, err := r.messagingSvc.PublishDue(ctx, schedulerBatch)
			if err != nil {
				r.logger.Error("scheduled message publisher failed", slog.Any("error", err))
				continue
			}
			if published > 0 {
				r.logger.Info("published scheduled messages", slog.Int("count", published))
			}
		case <-r.stop:
			return
		case <-ctx.Done():
			return
		}
	}
}

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
		{"expired_email_challenges", r.pruneEmailChallenges},
		{"consumed_sync_events", r.pruneSyncEvents},
		{"expired_stories", r.expireStories},
		{"auto_deleted_messages", r.autoDeleteMessages},
		{"abandoned_uploads", r.expireUploadSessions},
		{"released_upload_parts", r.reapExpiredUploads},
		{"stranded_media", r.retryStrandedMedia},
		{"expired_turn_credentials", r.pruneTURNCredentials},
		{"decayed_spam_scores", r.decaySpamScores},
		{"data_requests", r.carryOutDataRequests},
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

// pruneEmailChallenges clears spent recovery codes. They are hashed, but a
// hash of a six-digit code is not much of a secret, and a row nobody can use
// is not worth keeping.
func (r *Runner) pruneEmailChallenges(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx,
		`DELETE FROM email_challenges WHERE expires_at < now() - interval '24 hours'`)
	if err != nil {
		return 0, fmt.Errorf("worker: prune email challenges: %w", err)
	}
	return tag.RowsAffected(), nil
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

// autoDeleteMessages enforces a chat's self-destruct timer (§14).
//
// `chat_settings.auto_delete_seconds` is a promise: someone turns it on
// believing that what they say stops existing after that long. Storing the
// number without ever acting on it is worse than not offering the setting,
// because it is a privacy guarantee that quietly is not kept.
//
// The row is tombstoned rather than removed, exactly as an ordinary deletion
// is: `seq` has to stay contiguous or every client's cursor arithmetic breaks,
// and a gap would be a worse lie than a marker. What actually goes is the
// content — that is the part the promise was about.
//
// Bounded per run so one chat with a short timer and a long history cannot
// monopolise a maintenance tick; the next tick continues where this stopped.
// dataRequestBatch bounds one pass. An export is heavy — a dozen queries and
// an upload — so a backlog is drained steadily rather than all at once.
const dataRequestBatch = 20

// carryOutDataRequests performs the exports and deletions whose time has come
// (§56).
//
// A deletion's time is the end of its cancellation window; an export's is
// immediately. Both are claimed with FOR UPDATE SKIP LOCKED, so running
// several workers does not carry the same request out twice.
//
// A failure returns the request to pending with the reason recorded rather
// than abandoning it: an export that failed because the object store was
// briefly unreachable is worth retrying, and a person who asked for their data
// should not have to ask again because of it.
func (r *Runner) carryOutDataRequests(ctx context.Context) (int64, error) {
	if r.storage == nil {
		// Without object storage there is nowhere to put an export. Deletions
		// would still work, but a worker that silently did half the job is
		// worse than one that says it cannot.
		return 0, nil
	}

	repo := datarights.NewRepository(r.db)
	due, err := repo.ClaimDue(ctx, dataRequestBatch)
	if err != nil {
		return 0, err
	}

	var done int64
	for _, request := range due {
		var runErr error
		switch request.Type {
		case datarights.TypeExport:
			var mediaID uuid.UUID
			mediaID, runErr = repo.Export(ctx, r.storage, request.UserID)
			if runErr == nil {
				runErr = repo.MarkReady(ctx, request.ID, mediaID)
			}
		case datarights.TypeDelete:
			runErr = repo.Delete(ctx, request.UserID)
			if runErr == nil {
				runErr = repo.MarkCompleted(ctx, request.ID)
			}
		}

		if runErr != nil {
			r.logger.Error("could not carry out a data request",
				slog.String("request_id", request.ID.String()),
				slog.String("type", request.Type),
				slog.Any("error", runErr))
			if markErr := repo.MarkFailed(ctx, request.ID, runErr.Error()); markErr != nil {
				r.logger.Error("could not record a data-request failure",
					slog.String("request_id", request.ID.String()), slog.Any("error", markErr))
			}
			continue
		}
		done++
	}
	return done, nil
}

// decaySpamScores sheds anti-spam score with the passage of time (§34).
//
// Without it the score is a ratchet: every long-lived account eventually
// crosses the threshold, and an account that has behaved for a month goes on
// paying for a bad week. Elapsed time is measured from each row's own
// updated_at, so the outcome does not depend on how often this runs.
func (r *Runner) decaySpamScores(ctx context.Context) (int64, error) {
	return antispam.NewRepository(r.db).Decay(ctx, antispam.DecayPerDay)
}

func (r *Runner) autoDeleteMessages(ctx context.Context) (int64, error) {
	tag, err := r.db.Pool.Exec(ctx, `
		UPDATE messages m
		SET deleted_at = now(), content = '', entities = '[]'::jsonb,
		    payload = '{}'::jsonb, reply_markup = NULL
		FROM chat_settings s
		WHERE s.chat_id = m.chat_id
		  AND s.auto_delete_seconds > 0
		  AND m.deleted_at IS NULL
		  AND m.created_at < now() - make_interval(secs => s.auto_delete_seconds)
		  AND m.id IN (
		      SELECT m2.id
		      FROM messages m2
		      JOIN chat_settings s2 ON s2.chat_id = m2.chat_id
		      WHERE s2.auto_delete_seconds > 0
		        AND m2.deleted_at IS NULL
		        AND m2.created_at < now() - make_interval(secs => s2.auto_delete_seconds)
		      ORDER BY m2.created_at
		      LIMIT $1)`, autoDeleteBatch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
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
