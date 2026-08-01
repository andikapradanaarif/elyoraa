package repository

import (
	"context"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewDB opens a PostgreSQL connection pool when DATABASE_URL is configured.
// If the URL is empty, it returns (nil, nil) so the app can run without a database.
func NewDB(cfg *config.Config) (*pgxpool.Pool, error) {
	if cfg.DatabaseURL == "" {
		return nil, nil
	}
	return pgxpool.New(context.Background(), cfg.DatabaseURL)
}

// Migrate creates the webhook audit log table if it does not exist.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return nil
	}
	_, err := pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS webhook_events (
		delivery_id TEXT PRIMARY KEY,
		event_type TEXT NOT NULL,
		action TEXT,
		owner TEXT NOT NULL,
		repo TEXT NOT NULL,
		pr_number INT,
		payload JSONB,
		status TEXT NOT NULL,
		received_at TIMESTAMPTZ DEFAULT NOW(),
		processed_at TIMESTAMPTZ,
		error TEXT
	)`)
	return err
}
