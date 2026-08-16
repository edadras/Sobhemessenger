// Command migrate applies, reverts and inspects database migrations.
//
//	migrate up            apply every pending migration
//	migrate down [steps]   revert the most recent migrations (default 1)
//	migrate status         list applied and pending migrations
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/sobh/messenger/backend/internal/config"
	"github.com/sobh/messenger/backend/internal/database"
	"github.com/sobh/messenger/backend/internal/logging"
)

func main() {
	dir := flag.String("dir", envOr("MIGRATIONS_DIR", "../database/migrations"), "migrations directory")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		command = "up"
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.Log, "sobh-migrate", cfg.NodeID)

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.Postgres)
	if err != nil {
		logger.Error("could not connect to the database", slog.Any("error", err))
		os.Exit(1)
	}
	defer db.Close()

	migrator := database.NewMigrator(db, os.DirFS(*dir), logger)

	switch command {
	case "up":
		if err := migrator.Up(ctx); err != nil {
			logger.Error("migration failed", slog.Any("error", err))
			os.Exit(1)
		}

	case "down":
		steps := 1
		if arg := flag.Arg(1); arg != "" {
			parsed, convErr := strconv.Atoi(arg)
			if convErr != nil {
				fmt.Fprintln(os.Stderr, "steps must be a number")
				os.Exit(1)
			}
			steps = parsed
		}
		if err := migrator.Down(ctx, steps); err != nil {
			logger.Error("rollback failed", slog.Any("error", err))
			os.Exit(1)
		}

	case "status":
		applied, err := migrator.Applied(ctx)
		if err != nil {
			logger.Error("could not read migration status", slog.Any("error", err))
			os.Exit(1)
		}
		pending, err := migrator.Pending(ctx)
		if err != nil {
			logger.Error("could not read pending migrations", slog.Any("error", err))
			os.Exit(1)
		}
		fmt.Printf("applied: %d\n", len(applied))
		for _, record := range applied {
			fmt.Printf("  %04d %-32s %s\n", record.Version, record.Name, record.AppliedAt.Format("2006-01-02 15:04:05"))
		}
		fmt.Printf("pending: %d\n", len(pending))
		for _, migration := range pending {
			fmt.Printf("  %04d %s\n", migration.Version, migration.Name)
		}

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (want up, down or status)\n", command)
		os.Exit(1)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
