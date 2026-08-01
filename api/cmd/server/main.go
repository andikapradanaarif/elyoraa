package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	"github.com/andikapradanaarif/elyoraa/internal/handler"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/middleware"
	"github.com/andikapradanaarif/elyoraa/internal/repository"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
	"github.com/andikapradanaarif/elyoraa/internal/startup"
	"github.com/andikapradanaarif/elyoraa/internal/usecase"
	"github.com/andikapradanaarif/elyoraa/migration"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	initLogging(cfg.LogLevel)

	checks := startup.Check(cfg)
	startup.LogResults(checks)
	if startup.HasFailures(checks) {
		// Don't fail hard in case Devin is mounted in later (e.g., inside a container).
		// The /health endpoint will report the failure instead.
		slog.Warn("one or more startup checks failed; see /health for details")
	}

	if err := usecase.WriteDevinConfig(cfg.DevinConfigPath); err != nil {
		slog.Warn("failed to write devin config", "error", err)
	}

	if err := os.MkdirAll(cfg.WorkspaceBase, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "workspace base: %v\n", err)
		os.Exit(1)
	}

	githubClient, err := github.NewClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "github client: %v\n", err)
		os.Exit(1)
	}

	tokenDir := os.Getenv("ELYORAA_TOKEN_DIR")
	if tokenDir == "" {
		tokenDir = "/tmp/elyoraa"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := repository.NewDB(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "database: %v\n", err)
		os.Exit(1)
	}
	if db != nil {
		if err := migration.New(db).Up(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
			os.Exit(1)
		}
		defer db.Close()
	}

	c := cache.New(cfg.WorkspaceBase, "/app/git-askpass.sh", tokenDir)
	d := dedup.New()
	m := metrics.New()
	store := repository.NewWebhookStore(db)

	qBackend, err := newQueueBackend(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "queue backend: %v\n", err)
		os.Exit(1)
	}

	reviewUC := usecase.NewReviewUsecase(cfg, c, githubClient, d, store, m)
	q := queue.NewWithBackend(cfg.MaxConcurrentReviews, reviewUC, m, qBackend)

	webhookUC := usecase.NewWebhookUsecase(cfg, githubClient, d, q, store)
	h := handler.New(cfg, webhookUC, m)

	go q.Start(ctx)
	go runSweeper(ctx, c, cfg.WorktreeTTLHours)
	go runEvictor(ctx, c, cfg.WorkspaceDiskBudgetGB)

	mux := http.NewServeMux()
	h.Register(mux)
	handler := middleware.RequestID(mux)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("server error", "error", err)
			cancel()
		}
	}()

	slog.Info("elyoraa started",
		"addr", srv.Addr,
		"concurrent", cfg.MaxConcurrentReviews,
		"queue_backend", cfg.QueueBackend,
		"queue_file", cfg.QueueFilePath,
		"database", cfg.DatabaseURL != "",
	)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
}

func newQueueBackend(cfg *config.Config) (queue.Backend, error) {
	switch cfg.QueueBackend {
	case "file", "boltdb":
		return queue.OpenBoltDB(cfg.QueueFilePath, 0)
	case "memory", "":
		return queue.NewMemoryBackend(64), nil
	default:
		return nil, fmt.Errorf("unknown queue backend: %s", cfg.QueueBackend)
	}
}

func initLogging(level string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})
	slog.SetDefault(slog.New(h))
}

func runSweeper(ctx context.Context, c *cache.Cache, ttl time.Duration) {
	t := time.NewTicker(30 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.Sweep(ctx, ttl); err != nil {
				slog.Warn("sweeper failed", "error", err)
			}
		}
	}
}

func runEvictor(ctx context.Context, c *cache.Cache, budgetGB int) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.EvictLRU(ctx, budgetGB); err != nil {
				slog.Warn("evictor failed", "error", err)
			}
		}
	}
}
