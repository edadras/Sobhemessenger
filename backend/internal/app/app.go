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

	"github.com/sobh/messenger/backend/internal/admin"
	"github.com/sobh/messenger/backend/internal/antispam"
	"github.com/sobh/messenger/backend/internal/auth"
	"github.com/sobh/messenger/backend/internal/bots"
	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/calls"
	"github.com/sobh/messenger/backend/internal/communities"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/contacts"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/featureflags"
	"github.com/sobh/messenger/backend/internal/groups"
	"github.com/sobh/messenger/backend/internal/httpx"
	"github.com/sobh/messenger/backend/internal/media"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/news"
	"github.com/sobh/messenger/backend/internal/notifications"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/polls"
	"github.com/sobh/messenger/backend/internal/presence"
	"github.com/sobh/messenger/backend/internal/ratelimit"
	"github.com/sobh/messenger/backend/internal/realtime"
	"github.com/sobh/messenger/backend/internal/search"
	"github.com/sobh/messenger/backend/internal/secretchat"
	"github.com/sobh/messenger/backend/internal/stickers"
	"github.com/sobh/messenger/backend/internal/storage"
	"github.com/sobh/messenger/backend/internal/stories"
	"github.com/sobh/messenger/backend/internal/users"
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

	Auth          *auth.Service
	AuthRepo      *auth.Repository
	Users         *users.Service
	Bots          *bots.Service
	Stickers      *stickers.Service
	Contacts      *contacts.Service
	SecretChat    *secretchat.Service
	Messaging     *messaging.Service
	Media         *media.Service
	Groups        *groups.Service
	Communities   *communities.Service
	Stories       *stories.Service
	Polls         *polls.Service
	Calls         *calls.Service
	News          *news.Service
	Notifications *notifications.Service
	Search        *search.Service
	Admin         *admin.Service
	Antispam      *antispam.Service
	Presence      *presence.Service
	Flags         *featureflags.Service
	Hub           *realtime.Hub

	httpServer    *http.Server
	metricsServer *http.Server
}

// Dependencies are the external systems the application runs on.
//
// New connects them from configuration; Assemble takes them already built.
// Splitting the two is what lets the end-to-end test drive the real router
// against a real database, cache and message bus rather than a stand-in.
type Dependencies struct {
	DB    *database.DB
	Cache *cache.Client
	Bus   *bus.Bus
	// Storage may be nil. The media module is then not mounted and readiness
	// reports object storage as unconfigured — production config validation
	// requires credentials, so this only happens deliberately.
	Storage *storage.Client
	Metrics *observability.Metrics
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

	return Assemble(ctx, cfg, logger, Dependencies{
		DB: db, Cache: cacheClient, Bus: messageBus,
		Storage: storageClient, Metrics: metrics,
	})
}

// Assemble wires the domain modules onto already-connected dependencies.
func Assemble(ctx context.Context, cfg *config.Config, logger *slog.Logger, deps Dependencies) (*App, error) {
	db := deps.DB
	cacheClient := deps.Cache
	messageBus := deps.Bus
	storageClient := deps.Storage

	metrics := deps.Metrics
	if metrics == nil {
		metrics = observability.New(cfg.ServiceName)
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
	emailSender, err := auth.NewEmailSender(cfg.Email, logger)
	if err != nil {
		return nil, err
	}

	authService := auth.NewService(authRepo, tokens, limiter, rules, smsSender,
		cacheClient, cfg.Auth, cfg.SMS, metrics, logger)
	authService.SetEmailSender(emailSender, cfg.Email.EchoCodes)
	authMiddleware := auth.NewMiddleware(tokens, authRepo, cacheClient, logger)

	flags := featureflags.NewService(db, cacheClient, logger)
	presenceService := presence.NewService(cacheClient, messageBus, logger)

	messagingRepo := messaging.NewRepository(db)
	messagingService := messaging.NewService(messagingRepo, messageBus, limiter, rules, metrics, logger)

	usersService := users.NewService(users.NewRepository(db))
	// Bots post through the messaging service, so they are bound by the same
	// membership, permission and rate-limit rules as anyone else.
	botsRepo := bots.NewRepository(db)
	botsService := bots.NewService(botsRepo, messagingService, logger)

	// Bots hear about messages through an observer rather than by messaging
	// importing them, which would be a cycle: bots already depends on messaging
	// to send. Without this a bot could talk and never listen.
	messagingService.AddObserver(botsService)

	// BotFather runs inside the server. It can create a bot for any user, so a
	// token for it would be a credential nobody should hold — the database
	// refuses to issue one.
	botFatherID, err := botsService.EnsureBotFather(ctx)
	if err != nil {
		return nil, fmt.Errorf("assemble: provision botfather: %w", err)
	}
	botsService.RegisterInternal(botFatherID,
		bots.NewBotFather(botsRepo, botsService, db, botFatherID, logger))

	stickersService := stickers.NewService(stickers.NewRepository(db))

	contactsService := contacts.NewService(contacts.NewRepository(db), limiter, rules, cfg.Auth)
	secretChatService := secretchat.NewService(secretchat.NewRepository(db), messagingRepo,
		messageBus, logger)

	var mediaService *media.Service
	if storageClient != nil {
		mediaService = media.NewService(media.NewRepository(db), storageClient, messageBus,
			limiter, rules, cfg.Media, metrics, logger)
	} else {
		logger.Warn("object storage is not configured; the media module is disabled")
	}

	groupsService := groups.NewService(groups.NewRepository(db), messagingRepo,
		messagingService, messageBus, cfg, logger)
	// Channel posts are mirrored into the linked discussion group as they go
	// out, which is what makes comments possible on a post nobody has opened
	// yet.
	messagingService.AddObserver(groupsService)

	communitiesService := communities.NewService(communities.NewRepository(db), groupsService)

	storiesService := stories.NewService(stories.NewRepository(db))
	pollsService := polls.NewService(polls.NewRepository(db), messagingService)
	callsService := calls.NewService(calls.NewRepository(db), messagingRepo,
		messageBus, cfg.Calls, logger)

	newsService := news.NewService(news.NewRepository(db), messageBus, logger)

	notificationsService := notifications.NewService(notifications.NewRepository(db), messageBus)

	searchClient := search.New(cfg.Search)
	if err := searchClient.EnsureIndices(ctx); err != nil {
		// A search cluster that is not ready must not stop the messenger from
		// starting; queries degrade to empty results until it recovers.
		logger.Warn("could not prepare search indices", slog.Any("error", err))
	}
	searchService := search.NewService(searchClient, db, limiter, rules, logger)

	// The admin module drops cached auth state after a ban or role change; it
	// receives the invalidator as a function so it does not depend on auth.
	// Anti-spam scoring (§34). Every producer of a signal and the one consumer
	// of the verdict are wired here rather than through constructors, so a
	// module that records a signal does not have to depend on the one that
	// weighs it.
	antispamService := antispam.NewService(antispam.NewRepository(db))
	messagingService.SetSpamGuard(antispamService)
	contactsService.SetSpamRecorder(antispamService)

	adminService := admin.NewService(admin.NewRepository(db), flags,
		authMiddleware.InvalidateUserCache, logger)
	adminService.SetSpamRecorder(antispamService)

	hub := realtime.NewHub(cfg.NodeID, messageBus, metrics, logger)

	app := &App{
		cfg: cfg, logger: logger, metrics: metrics,
		DB: db, Cache: cacheClient, Bus: messageBus, Storage: storageClient,
		Auth: authService, AuthRepo: authRepo, Messaging: messagingService,
		Users: usersService, Bots: botsService, Stickers: stickersService,
		Contacts: contactsService, SecretChat: secretChatService,
		Media: mediaService, Groups: groupsService, Communities: communitiesService,
		Stories: storiesService, Polls: pollsService, Calls: callsService,
		News: newsService, Notifications: notificationsService, Search: searchService,
		Admin:    adminService,
		Antispam: antispamService,
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
	usersHandler := users.NewHandler(a.Users)
	botsHandler := bots.NewHandler(a.Bots)
	stickersHandler := stickers.NewHandler(a.Stickers)
	contactsHandler := contacts.NewHandler(a.Contacts)
	secretChatHandler := secretchat.NewHandler(a.SecretChat)
	messagingHandler := messaging.NewHandler(messagingService)
	var mediaHandler *media.Handler
	if a.Media != nil {
		mediaHandler = media.NewHandler(a.Media)
	}
	groupsHandler := groups.NewHandler(a.Groups)
	communitiesHandler := communities.NewHandler(a.Communities)
	storiesHandler := stories.NewHandler(a.Stories)
	pollsHandler := polls.NewHandler(a.Polls)
	callsHandler := calls.NewHandler(a.Calls)
	newsHandler := news.NewHandler(a.News)
	notificationsHandler := notifications.NewHandler(a.Notifications)
	searchHandler := search.NewHandler(a.Search)
	adminHandler := admin.NewHandler(a.Admin, authMiddleware.RequirePermission)
	antispamHandler := antispam.NewHandler(a.Antispam, authMiddleware.RequirePermission)
	flagsHandler := featureflags.NewHandler(a.Flags)

	wsHandler := realtime.NewHandler(a.Hub, authMiddleware, a.AuthRepo,
		messagingService, presenceService, corsOrigins, a.logger)
	r.Handle("/ws", wsHandler)

	r.Route("/api/v1", func(api chi.Router) {
		// Requests are bounded even before authentication, so an unauthenticated
		// flood cannot reach the database (§33).
		api.Use(httpx.Timeout(30 * time.Second))

		api.Mount("/auth", authHandler.Routes(authMiddleware.RequireAuth))

		// The Bot API carries its own authentication — a bot token, not a
		// session — so it is mounted outside the signed-in group (§20).
		api.Mount("/bot", botsHandler.APIRoutes())
		api.Get("/feature-flags", flagsHandler.List)

		// The news feed is readable without an account; OptionalAuth fills in
		// bookmark and follow state for readers who do have one (§25).
		api.Group(func(public chi.Router) {
			public.Use(authMiddleware.OptionalAuth)
			public.Mount("/news", newsHandler.PublicRoutes())
		})

		api.Group(func(private chi.Router) {
			private.Use(authMiddleware.RequireAuth)

			private.Mount("/users", usersHandler.Routes())
			private.Mount("/bots", botsHandler.ManagementRoutes())
			private.Mount("/stickers", stickersHandler.Routes())
			// What a person's client calls to use a bot inline or tap a button,
			// as distinct from /bots (managing your own) and /bot (being one).
			private.Mount("/inline", botsHandler.InlineRoutes())
			private.Mount("/contacts", contactsHandler.Routes())
			// The server's half of end-to-end encryption: a key directory and a
			// mailbox for ciphertext it cannot read (§24).
			private.Mount("/secret", secretChatHandler.Routes())
			private.Route("/chats", func(chats chi.Router) {
				messagingHandler.RegisterChatRoutes(chats)
				messagingHandler.RegisterOrganiseRoutes(chats)
				groupsHandler.RegisterRoutes(chats)
			})
			private.Route("/messages", func(messages chi.Router) {
				messagingHandler.RegisterMessageRoutes(messages)
				messagingHandler.RegisterPinRoute(messages)
			})
			private.Mount("/sync", messagingHandler.SyncRoutes())
			if mediaHandler != nil {
				private.Mount("/media", mediaHandler.Routes())
			}
			private.Mount("/communities", communitiesHandler.Routes())
			private.Mount("/stories", storiesHandler.Routes())
			private.Mount("/polls", pollsHandler.Routes())
			private.Mount("/calls", callsHandler.Routes())
			private.Mount("/news-reader", newsHandler.ReaderRoutes())
			private.Mount("/notifications", notificationsHandler.Routes())
			private.Mount("/search", searchHandler.Routes())
			private.Mount("/reports", adminHandler.ReportRoutes())

			// Every admin route carries its own permission check; users.read is
			// the floor for reaching the surface at all (§32).
			private.Group(func(operator chi.Router) {
				operator.Use(authMiddleware.RequirePermission(admin.PermUsersRead))
				operator.Mount("/admin", adminHandler.Routes())
				operator.Mount("/admin/search", searchHandler.AdminRoutes())
				operator.Mount("/admin/spam-scores", antispamHandler.Routes())
			})

			private.Group(func(editorial chi.Router) {
				editorial.Use(authMiddleware.RequirePermission(news.PermRead))
				editorial.Mount("/editorial", newsHandler.EditorialRoutes())
			})
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

// Handler exposes the assembled router.
//
// It exists so the end-to-end test drives the same handler chain production
// serves — middleware, routing and all — rather than a second wiring that
// could drift from it.
func (a *App) Handler() http.Handler { return a.httpServer.Handler }

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
