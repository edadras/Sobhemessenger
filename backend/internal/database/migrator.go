package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// advisoryLockKey serialises migrations across every instance that boots at
// once, so a rolling deploy cannot apply the same file twice.
const advisoryLockKey int64 = 8_913_224_517_001

// Migration is one versioned schema change loaded from disk.
type Migration struct {
	Version  int
	Name     string
	UpSQL    string
	DownSQL  string
	Checksum string
}

// AppliedMigration is a row of schema_migrations.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// Migrator applies migrations from an fs.FS. Files are named
// <version>_<name>.up.sql with a matching .down.sql.
type Migrator struct {
	db     *DB
	fsys   fs.FS
	logger *slog.Logger
}

func NewMigrator(db *DB, fsys fs.FS, logger *slog.Logger) *Migrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Migrator{db: db, fsys: fsys, logger: logger}
}

// Up applies every pending migration in version order.
func (m *Migrator) Up(ctx context.Context) error {
	migrations, err := m.load()
	if err != nil {
		return err
	}
	if err := m.ensureTable(ctx); err != nil {
		return err
	}

	conn, err := m.db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("migrate: acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockKey); err != nil {
		return fmt.Errorf("migrate: acquire advisory lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockKey)
	}()

	applied, err := m.appliedMap(ctx)
	if err != nil {
		return err
	}

	// A changed checksum means an already-applied file was edited in place,
	// which would silently diverge environments. Refuse rather than guess.
	for _, migration := range migrations {
		if prior, ok := applied[migration.Version]; ok && prior.Checksum != migration.Checksum {
			return fmt.Errorf(
				"migrate: migration %04d_%s was modified after being applied (recorded %s, found %s); "+
					"create a new migration instead of editing an applied one",
				migration.Version, migration.Name, prior.Checksum[:12], migration.Checksum[:12])
		}
	}

	pending := 0
	for _, migration := range migrations {
		if _, ok := applied[migration.Version]; ok {
			continue
		}
		pending++
		start := time.Now()

		// Each migration is its own transaction: a failure leaves earlier
		// migrations applied and recorded, so a re-run resumes where it stopped.
		err := m.db.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.UpSQL); err != nil {
				return fmt.Errorf("migrate: apply %04d_%s: %w", migration.Version, migration.Name, err)
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
				migration.Version, migration.Name, migration.Checksum)
			return err
		})
		if err != nil {
			return err
		}

		m.logger.Info("migration applied",
			slog.Int("version", migration.Version),
			slog.String("name", migration.Name),
			slog.Duration("duration", time.Since(start)),
		)
	}

	if pending == 0 {
		m.logger.Info("database schema is up to date", slog.Int("version", maxVersion(migrations)))
	}
	return nil
}

// Down rolls back the most recent `steps` migrations.
func (m *Migrator) Down(ctx context.Context, steps int) error {
	if steps <= 0 {
		return fmt.Errorf("migrate: steps must be positive")
	}
	migrations, err := m.load()
	if err != nil {
		return err
	}
	if err := m.ensureTable(ctx); err != nil {
		return err
	}

	byVersion := make(map[int]Migration, len(migrations))
	for _, migration := range migrations {
		byVersion[migration.Version] = migration
	}

	applied, err := m.Applied(ctx)
	if err != nil {
		return err
	}
	sort.Slice(applied, func(i, j int) bool { return applied[i].Version > applied[j].Version })

	for i, record := range applied {
		if i >= steps {
			break
		}
		migration, ok := byVersion[record.Version]
		if !ok {
			return fmt.Errorf("migrate: no file found for applied version %d", record.Version)
		}
		if strings.TrimSpace(migration.DownSQL) == "" {
			return fmt.Errorf("migrate: migration %04d_%s has no down file", migration.Version, migration.Name)
		}

		err := m.db.InTx(ctx, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, migration.DownSQL); err != nil {
				return fmt.Errorf("migrate: revert %04d_%s: %w", migration.Version, migration.Name, err)
			}
			_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, migration.Version)
			return err
		})
		if err != nil {
			return err
		}
		m.logger.Info("migration reverted",
			slog.Int("version", migration.Version),
			slog.String("name", migration.Name))
	}
	return nil
}

// Pending lists migrations that have not been applied yet.
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	migrations, err := m.load()
	if err != nil {
		return nil, err
	}
	if err := m.ensureTable(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedMap(ctx)
	if err != nil {
		return nil, err
	}

	var pending []Migration
	for _, migration := range migrations {
		if _, ok := applied[migration.Version]; !ok {
			pending = append(pending, migration)
		}
	}
	return pending, nil
}

func (m *Migrator) Applied(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := m.db.Pool.Query(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("migrate: read schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var record AppliedMigration
		if err := rows.Scan(&record.Version, &record.Name, &record.Checksum, &record.AppliedAt); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (m *Migrator) ensureTable(ctx context.Context) error {
	_, err := m.db.Pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INT PRIMARY KEY,
			name       TEXT        NOT NULL,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`)
	if err != nil {
		return fmt.Errorf("migrate: create schema_migrations: %w", err)
	}
	return nil
}

func (m *Migrator) appliedMap(ctx context.Context) (map[int]AppliedMigration, error) {
	applied, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int]AppliedMigration, len(applied))
	for _, record := range applied {
		out[record.Version] = record
	}
	return out, nil
}

func (m *Migrator) load() ([]Migration, error) {
	entries, err := fs.ReadDir(m.fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("migrate: read migrations directory: %w", err)
	}

	byVersion := make(map[int]*Migration)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}

		isDown := strings.HasSuffix(name, ".down.sql")
		trimmed := strings.TrimSuffix(strings.TrimSuffix(name, ".down.sql"), ".up.sql")
		versionPart, namePart, ok := strings.Cut(trimmed, "_")
		if !ok {
			return nil, fmt.Errorf("migrate: %q does not match <version>_<name>.(up|down).sql", name)
		}
		version, err := strconv.Atoi(versionPart)
		if err != nil {
			return nil, fmt.Errorf("migrate: %q has a non-numeric version: %w", name, err)
		}

		content, err := fs.ReadFile(m.fsys, name)
		if err != nil {
			return nil, fmt.Errorf("migrate: read %q: %w", name, err)
		}

		migration, ok := byVersion[version]
		if !ok {
			migration = &Migration{Version: version, Name: namePart}
			byVersion[version] = migration
		}
		if isDown {
			migration.DownSQL = string(content)
		} else {
			migration.UpSQL = string(content)
			sum := sha256.Sum256(content)
			migration.Checksum = hex.EncodeToString(sum[:])
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, migration := range byVersion {
		if strings.TrimSpace(migration.UpSQL) == "" {
			return nil, fmt.Errorf("migrate: version %d has no .up.sql file", migration.Version)
		}
		out = append(out, *migration)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func maxVersion(migrations []Migration) int {
	highest := 0
	for _, migration := range migrations {
		if migration.Version > highest {
			highest = migration.Version
		}
	}
	return highest
}
