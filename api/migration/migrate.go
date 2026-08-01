package migration

import (
	"context"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations
var migrationsFS embed.FS

// Migrator applies versioned SQL migrations.
type Migrator struct {
	pool *pgxpool.Pool
}

// New creates a Migrator.
func New(pool *pgxpool.Pool) *Migrator {
	return &Migrator{pool: pool}
}

// Up applies all unapplied up migrations in order.
func (m *Migrator) Up(ctx context.Context) error {
	if m.pool == nil {
		return nil
	}

	if err := m.ensureSchemaMigrations(ctx); err != nil {
		return fmt.Errorf("schema migrations table: %w", err)
	}

	files, err := migrationFiles(".up.sql")
	if err != nil {
		return err
	}

	for _, f := range files {
		version, err := parseVersion(f)
		if err != nil {
			return err
		}

		applied, err := m.isApplied(ctx, version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}

		sql, err := migrationsFS.ReadFile(filepath.Join("migrations", f))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", f, err)
		}

		if err := m.apply(ctx, version, string(sql)); err != nil {
			return fmt.Errorf("apply migration %s: %w", f, err)
		}
	}

	return nil
}

// Down reverts the last applied up migration.
func (m *Migrator) Down(ctx context.Context) error {
	if m.pool == nil {
		return nil
	}

	var version int
	err := m.pool.QueryRow(ctx, `
		SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1
	`).Scan(&version)
	if err != nil {
		return fmt.Errorf("find last migration: %w", err)
	}

	downFile := fmt.Sprintf("%03d_%s.down.sql", version, migrationName(version))
	sql, err := migrationsFS.ReadFile(filepath.Join("migrations", downFile))
	if err != nil {
		return fmt.Errorf("read down migration %s: %w", downFile, err)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, version); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (m *Migrator) ensureSchemaMigrations(ctx context.Context) error {
	_, err := m.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INT PRIMARY KEY,
		applied_at TIMESTAMPTZ DEFAULT NOW()
	)`)
	return err
}

func (m *Migrator) isApplied(ctx context.Context, version int) (bool, error) {
	var exists bool
	err := m.pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)
	`, version).Scan(&exists)
	return exists, err
}

func (m *Migrator) apply(ctx context.Context, version int, sql string) error {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, version); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func migrationFiles(suffix string) ([]string, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, err
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		files = append(files, e.Name())
	}

	sort.Strings(files)
	return files, nil
}

func parseVersion(name string) (int, error) {
	parts := strings.SplitN(name, "_", 2)
	if len(parts) < 2 {
		return 0, fmt.Errorf("invalid migration filename: %s", name)
	}
	v, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, fmt.Errorf("invalid migration version in %s: %w", name, err)
	}
	return v, nil
}

func migrationName(version int) string {
	// The down file name is derived from the up file name. In a real system
	// we'd parse the actual up file, but for now we look it up at runtime.
	files, _ := migrationFiles(".up.sql")
	for _, f := range files {
		v, _ := parseVersion(f)
		if v == version {
			return strings.SplitN(strings.TrimSuffix(f, ".up.sql"), "_", 2)[1]
		}
	}
	return ""
}
