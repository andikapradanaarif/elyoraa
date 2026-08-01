package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all application configuration loaded from the environment.
type Config struct {
	Port                    string
	DatabaseURL             string
	GitHubWebhookSecret     string
	GitHubAppID             int64
	GitHubAppPrivateKeyPath string
	DevinBin                string
	DevinConfigPath         string
	WorkspaceBase           string
	MaxConcurrentReviews    int
	WorktreeTTLHours        time.Duration
	WorkspaceDiskBudgetGB   int
	QueueBackend            string
	QueueFilePath           string
	RedisURL                string
	MaxPatchBytes           int
	MaxChangedFiles         int
	LogLevel                string
	MetricsPath             string
}

// Load reads configuration from environment variables and applies defaults.
func Load() (*Config, error) {
	cfg := &Config{
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DevinBin:              getEnv("DEVIN_BIN", "devin"),
		DevinConfigPath:       getEnv("DEVIN_CONFIG_PATH", "/app/devin-config.json"),
		WorkspaceBase:         getEnv("WORKSPACE_BASE", "/var/lib/elyoraa/workspace"),
		MaxConcurrentReviews:  getInt("MAX_CONCURRENT_REVIEWS", 2),
		WorktreeTTLHours:      time.Duration(getInt("WORKTREE_TTL_HOURS", 24)) * time.Hour,
		WorkspaceDiskBudgetGB: getInt("WORKSPACE_DISK_BUDGET_GB", 40),
		QueueBackend:          getEnv("QUEUE_BACKEND", "memory"),
		QueueFilePath:         getEnv("QUEUE_FILE_PATH", "/var/lib/elyoraa/queue.db"),
		RedisURL:              getEnv("REDIS_URL", ""),
		MaxPatchBytes:         getInt("MAX_PATCH_BYTES", 100_000),
		MaxChangedFiles:       getInt("MAX_CHANGED_FILES", 50),
		LogLevel:              getEnv("LOG_LEVEL", "info"),
		MetricsPath:           getEnv("METRICS_PATH", "/metrics"),
	}

	appID, err := getInt64("GITHUB_APP_ID")
	if err != nil {
		return nil, fmt.Errorf("GITHUB_APP_ID: %w", err)
	}
	cfg.GitHubAppID = appID

	cfg.GitHubWebhookSecret = os.Getenv("GITHUB_WEBHOOK_SECRET")
	if cfg.GitHubWebhookSecret == "" {
		return nil, fmt.Errorf("GITHUB_WEBHOOK_SECRET is required")
	}

	cfg.GitHubAppPrivateKeyPath = os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if cfg.GitHubAppPrivateKeyPath == "" {
		return nil, fmt.Errorf("GITHUB_APP_PRIVATE_KEY is required (path to private key file)")
	}

	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getInt64(key string) (int64, error) {
	v := os.Getenv(key)
	if v == "" {
		return 0, fmt.Errorf("missing")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid: %w", err)
	}
	return n, nil
}
