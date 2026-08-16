// Package bus is the messaging fabric. It carries two very different kinds of
// traffic on one connection:
//
//   - Core NATS pub/sub for realtime fan-out between WebSocket nodes (§75).
//     These are at-most-once and intentionally cheap: if a node is down its
//     clients resynchronise from the event log by cursor instead (§9).
//   - JetStream for durable background work — media processing, push, search
//     indexing, analytics, AI (§76). These must survive a restart.
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/observability"
)

// Subjects for realtime fan-out and durable jobs.
const (
	SubjectUserEventPrefix = "sobh.rt.user." // + user id
	SubjectChatEventPrefix = "sobh.rt.chat." // + chat id
	SubjectCallPrefix      = "sobh.rt.call." // + call id

	SubjectJobMediaProcess = "SOBH.jobs.media.process"
	SubjectJobPushSend     = "SOBH.jobs.push.send"
	SubjectJobSearchIndex  = "SOBH.jobs.search.index"
	SubjectJobNewsPublish  = "SOBH.jobs.news.publish"
	SubjectJobAnalytics    = "SOBH.jobs.analytics.record"
	SubjectJobAI           = "SOBH.jobs.ai.process"
	SubjectJobDataRequest  = "SOBH.jobs.data.request"
	SubjectJobCleanup      = "SOBH.jobs.maintenance.cleanup"
)

// jobSubjectFilter is the wildcard the JetStream stream binds to.
const jobSubjectFilter = "SOBH.jobs.>"

type Bus struct {
	conn    *nats.Conn
	js      nats.JetStreamContext
	logger  *slog.Logger
	metrics *observability.Metrics
	subs    []*nats.Subscription
}

func Connect(cfg config.NATS, logger *slog.Logger, metrics *observability.Metrics) (*Bus, error) {
	opts := []nats.Option{
		nats.Name("sobh"),
		nats.MaxReconnects(cfg.MaxReconnects),
		nats.ReconnectWait(cfg.ReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("nats disconnected", slog.Any("error", err))
		}),
		nats.ReconnectHandler(func(c *nats.Conn) {
			logger.Info("nats reconnected", slog.String("url", c.ConnectedUrl()))
		}),
	}

	conn, err := nats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("bus: connect: %w", err)
	}

	js, err := conn.JetStream(nats.PublishAsyncMaxPending(512))
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("bus: jetstream context: %w", err)
	}

	b := &Bus{conn: conn, js: js, logger: logger, metrics: metrics}
	if err := b.ensureStream(cfg.StreamName); err != nil {
		conn.Close()
		return nil, err
	}
	return b, nil
}

// ensureStream creates or updates the durable job stream. WorkQueue retention
// keeps a job only until one consumer acknowledges it.
func (b *Bus) ensureStream(name string) error {
	cfg := &nats.StreamConfig{
		Name:       name,
		Subjects:   []string{jobSubjectFilter},
		Retention:  nats.WorkQueuePolicy,
		Storage:    nats.FileStorage,
		Discard:    nats.DiscardOld,
		MaxAge:     7 * 24 * time.Hour,
		Duplicates: 5 * time.Minute,
		Replicas:   1,
	}

	if _, err := b.js.StreamInfo(name); err != nil {
		if _, err := b.js.AddStream(cfg); err != nil {
			return fmt.Errorf("bus: create stream %q: %w", name, err)
		}
		return nil
	}
	if _, err := b.js.UpdateStream(cfg); err != nil {
		return fmt.Errorf("bus: update stream %q: %w", name, err)
	}
	return nil
}

func (b *Bus) Close() {
	for _, sub := range b.subs {
		_ = sub.Drain()
	}
	if b.conn != nil {
		_ = b.conn.Drain()
		b.conn.Close()
	}
}

func (b *Bus) Healthy() bool { return b.conn != nil && b.conn.IsConnected() }

// PublishRealtime fans an event out to whichever node currently holds the
// recipient's socket. Delivery is best-effort by design.
func (b *Bus) PublishRealtime(subject string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bus: marshal realtime payload: %w", err)
	}
	if err := b.conn.Publish(subject, data); err != nil {
		return fmt.Errorf("bus: publish %s: %w", subject, err)
	}
	b.metrics.QueuePublished.WithLabelValues("realtime").Inc()
	return nil
}

// SubscribeRealtime registers a process-lifetime handler for a realtime
// subject. The subscription is drained at shutdown.
func (b *Bus) SubscribeRealtime(subject string, handler func([]byte)) error {
	sub, err := b.SubscribeRealtimeSubscription(subject, handler)
	if err != nil {
		return err
	}
	b.subs = append(b.subs, sub)
	return nil
}

// SubscribeRealtimeSubscription returns the subscription handle so the caller
// can unsubscribe. The WebSocket hub uses this to open a subject per connected
// user and drop it the moment their last socket on this node closes.
func (b *Bus) SubscribeRealtimeSubscription(subject string, handler func([]byte)) (*nats.Subscription, error) {
	sub, err := b.conn.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe %s: %w", subject, err)
	}
	// Realtime fan-out is bursty; a large buffer avoids slow-consumer drops.
	if err := sub.SetPendingLimits(65536, 128*1024*1024); err != nil {
		_ = sub.Unsubscribe()
		return nil, fmt.Errorf("bus: set pending limits: %w", err)
	}
	return sub, nil
}

// Job is the envelope every durable background task travels in.
type Job struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	Attempt   int             `json:"attempt"`
	CreatedAt time.Time       `json:"created_at"`
}

// PublishJob enqueues durable work. dedupeID makes the publish idempotent
// inside the stream's duplicate window, so a retried API call cannot enqueue
// the same media-processing job twice.
func (b *Bus) PublishJob(ctx context.Context, subject, dedupeID string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("bus: marshal job payload: %w", err)
	}
	job := Job{ID: dedupeID, Type: subject, Payload: body, CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("bus: marshal job: %w", err)
	}

	opts := []nats.PubOpt{nats.Context(ctx)}
	if dedupeID != "" {
		opts = append(opts, nats.MsgId(dedupeID))
	}
	if _, err := b.js.Publish(subject, data, opts...); err != nil {
		b.metrics.QueueFailures.WithLabelValues(subject).Inc()
		return fmt.Errorf("bus: publish job %s: %w", subject, err)
	}
	b.metrics.QueuePublished.WithLabelValues(subject).Inc()
	return nil
}

// JobHandler processes one job. Returning an error triggers a redelivery with
// backoff; returning nil acknowledges the job.
type JobHandler func(ctx context.Context, job Job) error

// ConsumeJobs binds a durable pull-free push consumer to a subject. maxRetries
// bounds redelivery before a job is terminated to stop a poison message from
// looping forever.
func (b *Bus) ConsumeJobs(ctx context.Context, subject, durable string, maxRetries int, handler JobHandler) error {
	sub, err := b.js.QueueSubscribe(subject, durable, func(msg *nats.Msg) {
		b.handleJob(ctx, subject, maxRetries, handler, msg)
	},
		nats.Durable(durable),
		nats.ManualAck(),
		nats.AckWait(2*time.Minute),
		nats.MaxDeliver(maxRetries+1),
		nats.DeliverAll(),
	)
	if err != nil {
		return fmt.Errorf("bus: consume %s: %w", subject, err)
	}
	b.subs = append(b.subs, sub)
	return nil
}

func (b *Bus) handleJob(ctx context.Context, subject string, maxRetries int, handler JobHandler, msg *nats.Msg) {
	defer func() {
		if rec := recover(); rec != nil {
			b.logger.Error("job handler panicked",
				slog.String("subject", subject),
				slog.Any("panic", rec))
			_ = msg.Nak()
		}
	}()

	var job Job
	if err := json.Unmarshal(msg.Data, &job); err != nil {
		// A malformed job can never succeed; drop it rather than loop.
		b.logger.Error("discarding malformed job",
			slog.String("subject", subject),
			slog.Any("error", err))
		_ = msg.Term()
		return
	}

	if meta, err := msg.Metadata(); err == nil {
		job.Attempt = int(meta.NumDelivered)
	}

	if err := handler(ctx, job); err != nil {
		b.metrics.QueueFailures.WithLabelValues(subject).Inc()
		if job.Attempt > maxRetries {
			b.logger.Error("job exhausted retries, terminating",
				slog.String("subject", subject),
				slog.String("job_id", job.ID),
				slog.Int("attempt", job.Attempt),
				slog.Any("error", err))
			_ = msg.Term()
			return
		}
		b.logger.Warn("job failed, will retry",
			slog.String("subject", subject),
			slog.String("job_id", job.ID),
			slog.Int("attempt", job.Attempt),
			slog.Any("error", err))
		// Exponential backoff, capped so a stuck dependency does not stall
		// the whole consumer.
		delay := time.Duration(1<<min(job.Attempt, 6)) * time.Second
		_ = msg.NakWithDelay(delay)
		return
	}

	b.metrics.QueueConsumed.WithLabelValues(subject).Inc()
	if err := msg.Ack(); err != nil {
		b.logger.Warn("failed to ack job",
			slog.String("subject", subject),
			slog.Any("error", err))
	}
}

func UserSubject(userID string) string { return SubjectUserEventPrefix + userID }
func ChatSubject(chatID string) string { return SubjectChatEventPrefix + chatID }
func CallSubject(callID string) string { return SubjectCallPrefix + callID }
