// Command api runs the SOBH HTTP and WebSocket server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sobh/messenger/backend/internal/app"
	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/logging"
)

func main() {
	// The container image is distroless and has no curl, so the binary
	// probes itself for the Docker/Kubernetes health check.
	healthcheck := flag.Bool("healthcheck", false, "probe the local /health endpoint and exit")
	flag.Parse()
	if *healthcheck {
		os.Exit(probeHealth())
	}

	cfg, err := config.Load()
	if err != nil {
		// The logger is not built yet, so this goes to stderr directly.
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("configuration error", slog.Any("error", err))
		os.Exit(1)
	}

	logger := logging.New(cfg.Log, cfg.ServiceName, cfg.NodeID)
	logger.Info("starting sobh api",
		slog.String("env", string(cfg.Env)),
		slog.String("node", cfg.NodeID))

	// SIGINT/SIGTERM cancel the context, which starts the graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("failed to start", slog.Any("error", err))
		os.Exit(1)
	}

	if err := application.Run(ctx); err != nil {
		logger.Error("server stopped with an error", slog.Any("error", err))
		os.Exit(1)
	}
}

// probeHealth returns a process exit code: 0 when the local server reports
// healthy, 1 otherwise.
func probeHealth() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
