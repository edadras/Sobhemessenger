// Command worker runs the background jobs from §76: media processing, push
// delivery, search indexing, scheduled publishing and maintenance.
//
// It shares the API's configuration and dependencies but serves no HTTP
// traffic, so it can be scaled independently of the request path.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sobh/messenger/backend/internal/bus"
	"github.com/sobh/messenger/backend/internal/cache"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/logging"
	"github.com/sobh/messenger/backend/internal/media"
	"github.com/sobh/messenger/backend/internal/messaging"
	"github.com/sobh/messenger/backend/internal/observability"
	"github.com/sobh/messenger/backend/internal/storage"
	"github.com/sobh/messenger/backend/internal/worker"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("configuration error", slog.Any("error", err))
		os.Exit(1)
	}

	logger := logging.New(cfg.Log, "sobh-worker", cfg.NodeID)
	metrics := observability.New("sobh-worker")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		logger.Error("could not connect to the database", slog.Any("error", err))
		os.Exit(1)
	}
	defer db.Close()

	cacheClient, err := cache.Connect(ctx, cfg.Redis)
	if err != nil {
		logger.Error("could not connect to redis", slog.Any("error", err))
		os.Exit(1)
	}
	defer cacheClient.Close()

	messageBus, err := bus.Connect(cfg.NATS, logger, metrics)
	if err != nil {
		logger.Error("could not connect to nats", slog.Any("error", err))
		os.Exit(1)
	}
	defer messageBus.Close()

	storageClient, err := storage.Connect(ctx, cfg.Storage)
	if err != nil {
		logger.Error("could not connect to object storage", slog.Any("error", err))
		os.Exit(1)
	}

	runner := worker.New(db, cacheClient, messageBus, storageClient,
		messaging.NewRepository(db), media.NewRepository(db), cfg, metrics, logger)
	if err := runner.Start(ctx); err != nil {
		logger.Error("worker failed to start", slog.Any("error", err))
		os.Exit(1)
	}

	logger.Info("worker running")
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	runner.Stop(shutdownCtx)
	logger.Info("worker stopped")
}
