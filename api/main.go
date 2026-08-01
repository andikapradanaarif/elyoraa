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

	"github.com/andikapradanaarif/elyoraa/internal/cache"
	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/dedup"
	gh "github.com/andikapradanaarif/elyoraa/internal/github"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
	"github.com/andikapradanaarif/elyoraa/internal/middleware"
	"github.com/andikapradanaarif/elyoraa/internal/queue"
	"github.com/andikapradanaarif/elyoraa/internal/reviewer"
	"github.com/andikapradanaarif/elyoraa/internal/webhook"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	initLogging(cfg.LogLevel)

	if err := reviewer.WriteDevinConfig(cfg.DevinConfigPath); err != nil {
		slog.Warn("failed to write devin config", "error", err)
	}

	if err := os.MkdirAll(cfg.WorkspaceBase, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "workspace base: %v\n", err)
		os.Exit(1)
	}

	ghClient, err := gh.NewClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "github client: %v\n", err)
		os.Exit(1)
	}

	tokenDir := os.Getenv("ELYORAA_TOKEN_DIR")
	if tokenDir == "" {
		tokenDir = "/tmp/elyoraa"
	}

	c := cache.New(cfg.WorkspaceBase, "/app/git-askpass.sh", tokenDir)
	d := dedup.New()
	m := metrics.New()
	r := reviewer.New(cfg, c, ghClient, d)
	q := queue.New(cfg.MaxConcurrentReviews, r, m, 64)
	wh := webhook.New(cfg, ghClient, q, d)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go q.Start(ctx)
	go runSweeper(ctx, c, cfg.WorktreeTTLHours)
	go runEvictor(ctx, c, cfg.WorkspaceDiskBudgetGB)

	mux := http.NewServeMux()
	wh.Register(mux, webhook.Health, m.Handler)
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

	slog.Info("elyoraa started", "addr", srv.Addr, "concurrent", cfg.MaxConcurrentReviews)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
	cancel()
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
