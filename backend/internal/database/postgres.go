// Package database owns the PostgreSQL connection pool, transaction helpers
// and the migration runner (§84 rule 12: every schema change is a migration).
package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sobh/messenger/backend/internal/config"
)

// DB wraps the pool so callers depend on a small surface rather than pgx types
// spreading through every service.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens the pool and verifies it before returning, so a bad DSN fails
// at boot rather than on the first request.
func Connect(ctx context.Context, cfg config.Postgres) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: parse DSN: %w", err)
	}

	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	poolCfg.HealthCheckPeriod = 30 * time.Second
	if !cfg.StatementCache {
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}

	return &DB{Pool: pool}, nil
}

func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
}

func (db *DB) Ping(ctx context.Context) error {
	return db.Pool.Ping(ctx)
}

// InTx runs fn inside a transaction, rolling back on error or panic. Nested
// use is not supported by design: services compose at the repository layer.
func (db *DB) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return db.InTxOptions(ctx, pgx.TxOptions{}, fn)
}

// InTxSerializable runs fn at SERIALIZABLE isolation, retrying the documented
// serialization failures a bounded number of times.
func (db *DB) InTxSerializable(ctx context.Context, fn func(pgx.Tx) error) error {
	const maxAttempts = 3
	var err error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err = db.InTxOptions(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if err == nil || !IsSerializationFailure(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 10 * time.Millisecond):
		}
	}
	return err
}

func (db *DB) InTxOptions(ctx context.Context, opts pgx.TxOptions, fn func(pgx.Tx) error) error {
	tx, err := db.Pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}

	committed := false
	defer func() {
		if !committed {
			// Use a detached context so cleanup still runs when ctx is cancelled.
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	committed = true
	return nil
}

// IsUniqueViolation reports whether err is a unique-constraint failure,
// optionally narrowed to a specific constraint name.
func IsUniqueViolation(err error, constraint ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		return false
	}
	if len(constraint) == 0 {
		return true
	}
	for _, name := range constraint {
		if pgErr.ConstraintName == name {
			return true
		}
	}
	return false
}

func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func IsCheckViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514"
}

func IsSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "40001" || pgErr.Code == "40P01"
}

// IsNoRows is the canonical "not found" check for single-row queries.
func IsNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
