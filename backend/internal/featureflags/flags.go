// Package featureflags implements the runtime feature switches from §68, so a
// capability can be turned off without a deploy.
package featureflags

import (
	"context"
	"encoding/json"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/httpx"
)

// Flag is one switch.
type Flag struct {
	Key            string    `json:"key"`
	Enabled        bool      `json:"enabled"`
	RolloutPercent int       `json:"rollout_percent"`
	Description    string    `json:"description"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// refreshInterval bounds how stale a flag can be. Flags are read on nearly
// every request, so they are cached in-process and refreshed in the background
// rather than fetched per call.
const refreshInterval = 30 * time.Second

type Service struct {
	db     *database.DB
	cache  *cache.Client
	logger *slog.Logger

	mu       sync.RWMutex
	flags    map[string]Flag
	loadedAt time.Time
}

func NewService(db *database.DB, cacheClient *cache.Client, logger *slog.Logger) *Service {
	return &Service{db: db, cache: cacheClient, logger: logger, flags: make(map[string]Flag)}
}

// Enabled reports whether a flag is on for a specific user, honouring the
// percentage rollout. Bucketing is by user id, so a user's answer is stable.
func (s *Service) Enabled(ctx context.Context, key string, userID uuid.UUID) bool {
	flag, ok := s.lookup(ctx, key)
	if !ok || !flag.Enabled {
		return false
	}
	if flag.RolloutPercent >= 100 {
		return true
	}
	if flag.RolloutPercent <= 0 {
		return false
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(key))
	_, _ = hasher.Write(userID[:])
	return int(hasher.Sum32()%100) < flag.RolloutPercent
}

// EnabledGlobally ignores rollout bucketing, for background jobs that have no
// user context.
func (s *Service) EnabledGlobally(ctx context.Context, key string) bool {
	flag, ok := s.lookup(ctx, key)
	return ok && flag.Enabled
}

// Require returns a FEATURE_DISABLED error when a flag is off, so a handler can
// gate itself in one line.
func (s *Service) Require(ctx context.Context, key string, userID uuid.UUID) error {
	if !s.Enabled(ctx, key, userID) {
		return httpx.FeatureDisabled(key)
	}
	return nil
}

// All returns the current flag set.
func (s *Service) All(ctx context.Context) []Flag {
	s.ensureFresh(ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Flag, 0, len(s.flags))
	for _, flag := range s.flags {
		out = append(out, flag)
	}
	return out
}

// Set updates a flag and invalidates the cache on every node.
func (s *Service) Set(ctx context.Context, key string, enabled bool, rollout int, actor uuid.UUID) error {
	if rollout < 0 || rollout > 100 {
		return httpx.Validation("rollout_percent must be between 0 and 100").
			WithField("rollout_percent", "0-100")
	}

	_, err := s.db.Pool.Exec(ctx, `
		INSERT INTO feature_flags (key, enabled, rollout_percent, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (key) DO UPDATE
		SET enabled = EXCLUDED.enabled,
		    rollout_percent = EXCLUDED.rollout_percent,
		    updated_by = EXCLUDED.updated_by,
		    updated_at = now()`, key, enabled, rollout, actor)
	if err != nil {
		return httpx.Internal(err)
	}

	// Force a reload on the next read here, and drop the shared cache so the
	// other nodes pick the change up on their next refresh.
	s.mu.Lock()
	s.loadedAt = time.Time{}
	s.mu.Unlock()
	_ = s.cache.Delete(ctx, cacheKey)

	return nil
}

const cacheKey = "feature_flags:all"

func (s *Service) lookup(ctx context.Context, key string) (Flag, bool) {
	s.ensureFresh(ctx)

	s.mu.RLock()
	defer s.mu.RUnlock()
	flag, ok := s.flags[key]
	return flag, ok
}

func (s *Service) ensureFresh(ctx context.Context) {
	s.mu.RLock()
	fresh := time.Since(s.loadedAt) < refreshInterval
	s.mu.RUnlock()
	if fresh {
		return
	}

	flags, err := s.load(ctx)
	if err != nil {
		// Keep serving the previous snapshot: a flag lookup must never fail a
		// request just because the database blipped.
		s.logger.Error("failed to refresh feature flags", slog.Any("error", err))
		return
	}

	s.mu.Lock()
	s.flags = flags
	s.loadedAt = time.Now()
	s.mu.Unlock()
}

func (s *Service) load(ctx context.Context) (map[string]Flag, error) {
	if raw, err := s.cache.Get(ctx, cacheKey); err == nil {
		var cached map[string]Flag
		if json.Unmarshal(raw, &cached) == nil {
			return cached, nil
		}
	}

	rows, err := s.db.Pool.Query(ctx,
		`SELECT key, enabled, rollout_percent, description, updated_at FROM feature_flags`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	flags := make(map[string]Flag)
	for rows.Next() {
		var flag Flag
		if err := rows.Scan(&flag.Key, &flag.Enabled, &flag.RolloutPercent,
			&flag.Description, &flag.UpdatedAt); err != nil {
			return nil, err
		}
		flags[flag.Key] = flag
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if encoded, err := json.Marshal(flags); err == nil {
		_ = s.cache.Set(ctx, cacheKey, encoded, refreshInterval)
	}
	return flags, nil
}

// Handler exposes the flag list to clients so they can hide disabled features
// in the UI rather than letting the user hit an error.
type Handler struct {
	service *Service
}

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	flags := h.service.All(r.Context())

	// Anonymous callers see the global state; a signed-in caller sees their
	// own bucketed answer.
	if principal := httpx.PrincipalFrom(r.Context()); principal != nil {
		for i := range flags {
			flags[i].Enabled = h.service.Enabled(r.Context(), flags[i].Key, principal.UserID)
			flags[i].RolloutPercent = 100
		}
	}
	httpx.JSON(w, r, http.StatusOK, map[string]any{"flags": flags})
}
