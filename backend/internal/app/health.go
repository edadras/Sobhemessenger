package app

import (
	"context"
	"net/http"
	"time"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/storage"
)

// HealthHandler implements the two probes from §36.
//
// /health answers "is this process alive" — it must not touch a dependency, or
// a database blip would have the orchestrator restart healthy pods.
// /ready answers "should this process receive traffic", and does check them.
type HealthHandler struct {
	db      *database.DB
	cache   *cache.Client
	bus     *bus.Bus
	storage *storage.Client
	service string
	started time.Time
}

func NewHealthHandler(db *database.DB, cacheClient *cache.Client, messageBus *bus.Bus, storageClient *storage.Client, service string) *HealthHandler {
	return &HealthHandler{
		db: db, cache: cacheClient, bus: messageBus, storage: storageClient,
		service: service, started: time.Now(),
	}
}

func (h *HealthHandler) Live(w http.ResponseWriter, r *http.Request) {
	httpx.JSON(w, r, http.StatusOK, map[string]any{
		"status":         "ok",
		"service":        h.service,
		"uptime_seconds": int(time.Since(h.started).Seconds()),
	})
}

type dependencyStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Latency int64  `json:"latency_ms"`
}

func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := []dependencyStatus{
		h.check(ctx, "postgres", func(ctx context.Context) error { return h.db.Ping(ctx) }),
		h.check(ctx, "redis", func(ctx context.Context) error { return h.cache.Ping(ctx) }),
		h.check(ctx, "nats", func(context.Context) error {
			if !h.bus.Healthy() {
				return errNotConnected
			}
			return nil
		}),
		h.check(ctx, "minio", func(ctx context.Context) error { return h.storage.Healthy(ctx) }),
	}

	ready := true
	for _, check := range checks {
		if check.Status != "ok" {
			ready = false
		}
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}
	httpx.JSON(w, r, status, map[string]any{
		"ready":        ready,
		"dependencies": checks,
	})
}

func (h *HealthHandler) check(ctx context.Context, name string, probe func(context.Context) error) dependencyStatus {
	start := time.Now()
	err := probe(ctx)
	result := dependencyStatus{
		Name:    name,
		Status:  "ok",
		Latency: time.Since(start).Milliseconds(),
	}
	if err != nil {
		result.Status = "error"
		result.Detail = err.Error()
	}
	return result
}

type notConnectedError struct{}

func (notConnectedError) Error() string { return "not connected" }

var errNotConnected = notConnectedError{}
