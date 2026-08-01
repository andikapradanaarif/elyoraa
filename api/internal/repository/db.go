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
