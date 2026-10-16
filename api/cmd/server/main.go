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
	"github.com/jackc/pgx/v5/pgxpool"
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
		fmt.Fprintln(os.Stderr, "startup checks failed; fix the issues above or set the missing dependencies")
		os.Exit(1)
	}

	if err := usecase.WriteDevinConfig(cfg.DevinConfigDir, cfg.TokenDir); err != nil {
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

	tokenDir := cfg.TokenDir

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

	c := cache.New(cfg.WorkspaceBase, cfg.GitAskpassPath, tokenDir, cfg.GitBaseURL)
	m := metrics.New()
	store := repository.NewWebhookStore(db)

	// instance identifies this replica — Postgres queue claims, dedup
	// claims, and job records all carry it.
	hostname, _ := os.Hostname()
	instance := fmt.Sprintf("%s-%d-%d", hostname, os.Getpid(), time.Now().UnixNano())

	var d dedup.Interface
	if cfg.QueueBackend == "postgres" {
		d = dedup.NewPostgres(ctx, db, instance)
	} else {
		d = dedup.New()
	}

	qBackend, err := newQueueBackend(cfg, db, instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "queue backend: %v\n", err)
		os.Exit(1)
	}

	reviewUC := usecase.NewReviewUsecase(cfg, c, githubClient, d, store, m)
	q := queue.NewWithBackend(cfg.MaxConcurrentReviews, reviewUC, m, qBackend)
	// The queue needs the usecase as its processor, so the usecase gets the
	// queue back via setter: respond-job dispatch + prior-status lookup.
	reviewUC.SetQueue(q)

	webhookUC := usecase.NewWebhookUsecase(cfg, githubClient, d, q, store, c, m)
	h := handler.New(cfg, webhookUC, m)

	// Jobs recorded as "processing" at boot were killed mid-flight by the
	// last shutdown — mark them failed (retryable via POST /jobs/retry) and
	// fix their pending comments.
	reviewUC.ReconcileStuckJobs(ctx)

	go q.Start(ctx)
	go runSweeper(ctx, c, cfg.WorktreeTTLHours)
	go runEvictor(ctx, c, cfg.WorkspaceDiskBudgetGB)
	if db != nil && cfg.AuditRetentionDays > 0 {
		go runRetention(ctx, store, cfg.AuditRetentionDays)
	}

	mux := http.NewServeMux()
	h.Register(mux)
	handler := middleware.RequestID(mux)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadTimeout:       cfg.HTTPReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTPWriteTimeout,
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

	// Stop accepting HTTP first, then drain workers: in-flight jobs keep
	// their (root) context while we wait, so a clean shutdown lets a running
	// review finish and post. Anything still running after the drain timeout
	// is cancelled below.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	_ = srv.Shutdown(shutdownCtx)
	shutdownCancel()

	drainCtx, drainCancel := context.WithTimeout(context.Background(), cfg.ShutdownDrainTimeout)
	q.Stop(drainCtx)
	drainCancel()

	cancel()
}

func newQueueBackend(cfg *config.Config, db *pgxpool.Pool, instance string) (queue.Backend, error) {
	switch cfg.QueueBackend {
	case "postgres":
		if db == nil {
			return nil, fmt.Errorf("QUEUE_BACKEND=postgres requires DATABASE_URL")
		}
		lease := cfg.QueueJobLease
		if lease <= 0 {
			// A job can't legitimately outlive its timeout; the lease covers
			// that plus slack for cleanup so only a dead worker's row lapses.
			lease = cfg.ReviewTimeout + 5*time.Minute
		}
		return queue.NewPostgresBackend(db, instance, lease), nil
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

// runRetention prunes the webhook audit log daily — the events table stores
// full payloads, so it grows without bound otherwise.
func runRetention(ctx context.Context, store repository.WebhookStore, days int) {
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
			n, err := store.DeleteOlderThan(ctx, cutoff)
			if err != nil {
				slog.Warn("webhook retention cleanup failed", "error", err)
			} else if n > 0 {
				slog.Info("webhook retention cleanup", "deleted", n, "older_than_days", days)
			}
		}
	}
}
