// Package app wires the modular monolith together.
//
// Each domain is a self-contained module with its own repository, service and
// handler, and they only ever talk to each other through those service
// interfaces — never by reaching into another module's tables. That is what
// makes it possible to lift a module out into its own service later without
// rewriting its callers (§4).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/sobh/messenger/backend/internal/auth"
	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/calls"
	"github.com/sobh/messenger/backend/internal/communities"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/featureflags"
	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/media"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/polls"
	"github.com/sobh/messenger/backend/internal/presence"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/realtime"
	"github.com/sobh/messenger/backend/internal/storage"
	"github.com/sobh/messenger/backend/internal/stories"
)

// App holds every long-lived dependency and the servers built on top of them.
type App struct {
	cfg     *config.Config
	logger  *slog.Logger
	metrics *observability.Metrics

	DB      *database.DB
	Cache   *cache.Client
	Bus     *bus.Bus
	Storage *storage.Client

	Auth        *auth.Service
	AuthRepo    *auth.Repository
	Messaging   *messaging.Service
	Media       *media.Service
	Groups      *groups.Service
	Communities *communities.Service
	Stories     *stories.Service
	Polls       *polls.Service
	Calls       *calls.Service
	Presence    *presence.Service
	Flags       *featureflags.Service
	Hub         *realtime.Hub

	httpServer    *http.Server
	metricsServer *http.Server
}

// New builds the application. Any dependency that fails to connect aborts
// startup: a half-connected process would only fail later, less clearly.
func New(ctx context.Context, cfg *config.Config, logger *slog.Logger) (*App, error) {
	metrics := observability.New(cfg.ServiceName)

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		return nil, err
	}

	cacheClient, err := cache.Connect(ctx, cfg.Redis)
	if err != nil {
		db.Close()
		return nil, err
	}

	messageBus, err := bus.Connect(cfg.NATS, logger, metrics)
	if err != nil {
		db.Close()
		_ = cacheClient.Close()
		return nil, err
	}

	storageClient, err := storage.Connect(ctx, cfg.Storage)
	if err != nil {
		db.Close()
		_ = cacheClient.Close()
		messageBus.Close()
		return nil, err
	}
	if err := storageClient.EnsureBuckets(ctx); err != nil {
		db.Close()
		_ = cacheClient.Close()
		messageBus.Close()
		return nil, err
	}

	limiter := ratelimit.New(cacheClient, metrics)
	rules := ratelimit.NewRules(cfg.RateLimits)

	tokens, err := auth.NewTokenService(cfg.Auth)
	if err != nil {
		return nil, err
	}
	smsSender, err := auth.NewSMSSender(cfg.SMS, logger)
	if err != nil {
		return nil, err
	}

	authRepo := auth.NewRepository(db)
	authService := auth.NewService(authRepo, tokens, limiter, rules, smsSender,
		cacheClient, cfg.Auth, cfg.SMS, metrics, logger)
	authMiddleware := auth.NewMiddleware(tokens, authRepo, cacheClient, logger)

	flags := featureflags.NewService(db, cacheClient, logger)
	presenceService := presence.NewService(cacheClient, messageBus, logger)

	messagingRepo := messaging.NewRepository(db)
	messagingService := messaging.NewService(messagingRepo, messageBus, limiter, rules, metrics, logger)

	mediaService := media.NewService(media.NewRepository(db), storageClient, messageBus,
		limiter, rules, cfg.Media, metrics, logger)

	groupsService := groups.NewService(groups.NewRepository(db), messagingRepo,
		messageBus, cfg, logger)

	communitiesService := communities.NewService(communities.NewRepository(db), groupsService)

	storiesService := stories.NewService(stories.NewRepository(db))
	pollsService := polls.NewService(polls.NewRepository(db), messagingService)
	callsService := calls.NewService(calls.NewRepository(db), messagingRepo,
		messageBus, cfg.Calls, logger)

	hub := realtime.NewHub(cfg.NodeID, messageBus, metrics, logger)

	app := &App{
		cfg: cfg, logger: logger, metrics: metrics,
		DB: db, Cache: cacheClient, Bus: messageBus, Storage: storageClient,
		Auth: authService, AuthRepo: authRepo, Messaging: messagingService,
		Media: mediaService, Groups: groupsService, Communities: communitiesService,
		Stories: storiesService, Polls: pollsService, Calls: callsService,
		Presence: presenceService, Flags: flags, Hub: hub,
	}

	router := app.buildRouter(authMiddleware, authService, messagingService, presenceService)

	app.httpServer = &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: router,
		// WebSocket connections live far longer than a request, so the write
		// timeout is left off and per-frame deadlines are used instead.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))
	app.metricsServer = &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	return app, nil
}

func (a *App) buildRouter(
	authMiddleware *auth.Middleware,
	authService *auth.Service,
	messagingService *messaging.Service,
	presenceService *presence.Service,
) http.Handler {
	corsOrigins := allowedOrigins(a.cfg)

	r := chi.NewRouter()
	r.Use(httpx.RequestID)
	r.Use(httpx.RealIP(a.cfg.TrustedProxies))
	r.Use(httpx.Logger(a.logger))
	r.Use(httpx.Recover)
	r.Use(httpx.SecurityHeaders(a.cfg.IsProduction()))
	r.Use(httpx.CORS(corsOrigins))
	r.Use(httpx.Metrics(a.metrics))

	// Probes stay outside the API version prefix and outside auth (§36).
	health := NewHealthHandler(a.DB, a.Cache, a.Bus, a.Storage, a.cfg.ServiceName)
	r.Get("/health", health.Live)
	r.Get("/ready", health.Ready)

	authHandler := auth.NewHandler(authService)
	messagingHandler := messaging.NewHandler(messagingService)
	mediaHandler := media.NewHandler(a.Media)
	groupsHandler := groups.NewHandler(a.Groups)
	communitiesHandler := communities.NewHandler(a.Communities)
	storiesHandler := stories.NewHandler(a.Stories)
	pollsHandler := polls.NewHandler(a.Polls)
	callsHandler := calls.NewHandler(a.Calls)
	flagsHandler := featureflags.NewHandler(a.Flags)

	wsHandler := realtime.NewHandler(a.Hub, authMiddleware, a.AuthRepo,
		messagingService, presenceService, corsOrigins, a.logger)
	r.Handle("/ws", wsHandler)

	r.Route("/api/v1", func(api chi.Router) {
		// Requests are bounded even before authentication, so an unauthenticated
		// flood cannot reach the database (§33).
		api.Use(httpx.Timeout(30 * time.Second))

		api.Mount("/auth", authHandler.Routes())
		api.Get("/feature-flags", flagsHandler.List)

		api.Group(func(private chi.Router) {
			private.Use(authMiddleware.RequireAuth)

			private.Mount("/auth", authHandler.AuthenticatedRoutes())
			private.Route("/chats", func(chats chi.Router) {
				messagingHandler.RegisterChatRoutes(chats)
				groupsHandler.RegisterRoutes(chats)
			})
			private.Mount("/messages", messagingHandler.MessageRoutes())
			private.Mount("/sync", messagingHandler.SyncRoutes())
			private.Mount("/media", mediaHandler.Routes())
			private.Mount("/communities", communitiesHandler.Routes())
			private.Mount("/stories", storiesHandler.Routes())
			private.Mount("/polls", pollsHandler.Routes())
			private.Mount("/calls", callsHandler.Routes())
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, httpx.NotFound(httpx.CodeNotFound, "No such endpoint"))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		httpx.Fail(w, r, httpx.BadRequest("Method not allowed for this endpoint"))
	})

	return r
}

// Run starts both servers and blocks until ctx is cancelled.
func (a *App) Run(ctx context.Context) error {
	errs := make(chan error, 2)

	go func() {
		a.logger.Info("http server listening", slog.String("addr", a.cfg.HTTPAddr))
		if err := a.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("http server: %w", err)
		}
	}()

	go func() {
		a.logger.Info("metrics server listening", slog.String("addr", a.cfg.MetricsAddr))
		if err := a.metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- fmt.Errorf("metrics server: %w", err)
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		return a.Shutdown()
	}
}

// Shutdown drains connections, then releases every dependency.
func (a *App) Shutdown() error {
	a.logger.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), a.cfg.ShutdownTimeout)
	defer cancel()

	// Close sockets first so clients reconnect elsewhere while the HTTP
	// listener drains its in-flight requests.
	a.Hub.Shutdown()

	var shutdownErr error
	if err := a.httpServer.Shutdown(ctx); err != nil {
		shutdownErr = fmt.Errorf("http shutdown: %w", err)
	}
	if err := a.metricsServer.Shutdown(ctx); err != nil && shutdownErr == nil {
		shutdownErr = fmt.Errorf("metrics shutdown: %w", err)
	}

	a.Bus.Close()
	if err := a.Cache.Close(); err != nil && shutdownErr == nil {
		shutdownErr = fmt.Errorf("cache close: %w", err)
	}
	a.DB.Close()

	a.logger.Info("shutdown complete")
	return shutdownErr
}

// allowedOrigins resolves the browser origins permitted for CORS and the
// WebSocket handshake.
func allowedOrigins(cfg *config.Config) []string {
	if raw := os.Getenv("CORS_ALLOWED_ORIGINS"); raw != "" {
		var origins []string
		for _, origin := range strings.Split(raw, ",") {
			if trimmed := strings.TrimSpace(origin); trimmed != "" {
				origins = append(origins, trimmed)
			}
		}
		return origins
	}
	if cfg.IsProduction() {
		// Production must be explicit; defaulting to "*" here would be a
		// silent downgrade.
		return nil
	}
	return []string{"*"}
}
