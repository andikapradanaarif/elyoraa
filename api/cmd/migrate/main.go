package main

import (
	"context"
	"fmt"
	"os"

	"github.com/andikapradanaarif/elyoraa/migration"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required")
		os.Exit(1)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	m := migration.New(pool)
	if err := m.Up(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "migrate up: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("migrations applied")
}
