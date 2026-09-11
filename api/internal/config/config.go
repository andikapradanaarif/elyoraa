package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all application configuration loaded from the environment.
type Config struct {
	Port                    string
	DatabaseURL             string
	GitHubWebhookSecret     string
	GitHubAppID             int64
	GitHubAppPrivateKeyPath string
	DevinBin                string
	DevinConfigDir          string
	DevinModel              string
	DevinPermissionMode     string
	PromptsDir              string
	BotHandle               string
	TokenDir                string
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
	ReviewTimeout           time.Duration
	CommentTimeout          time.Duration
	HTTPReadTimeout         time.Duration
	HTTPWriteTimeout        time.Duration
	DevinMaxRetries         int
	DevinRetryBackoffs      []time.Duration
	DevinAttemptTimeout     time.Duration
	DevinPreflightCheck     bool
}

// Load reads configuration from environment variables and applies defaults.
// It first loads a .env file (or the file pointed to by ENV_FILE) if one exists,
// so values can be kept out of the shell environment during local development.
func Load() (*Config, error) {
	envFile := os.Getenv("ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	_ = godotenv.Load(envFile)

	cfg := &Config{
		Port:                  getEnv("PORT", "8080"),
		DatabaseURL:           os.Getenv("DATABASE_URL"),
		DevinBin:              getEnv("DEVIN_BIN", "devin"),
		DevinConfigDir:        getEnv("DEVIN_CONFIG_DIR", "/var/lib/elyoraa/devin-config"),
		DevinModel:            getEnv("DEVIN_MODEL", "swe-2-high"),
		DevinPermissionMode:   getEnv("DEVIN_PERMISSION_MODE", "dangerous"),
		PromptsDir:            getEnv("PROMPTS_DIR", "/app/prompts"),
		BotHandle:             getEnv("BOT_HANDLE", "elyoraa"),
		TokenDir:              getEnv("ELYORAA_TOKEN_DIR", "/var/lib/elyoraa/tokens"),
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
		ReviewTimeout:         getDuration("REVIEW_TIMEOUT", 10*time.Minute),
		CommentTimeout:        getDuration("COMMENT_TIMEOUT", 10*time.Second),
		HTTPReadTimeout:       getDuration("HTTP_READ_TIMEOUT", 10*time.Second),
		HTTPWriteTimeout:      getDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		DevinMaxRetries:       getInt("DEVIN_MAX_RETRIES", 3),
		DevinRetryBackoffs:    getDurationList("DEVIN_RETRY_BACKOFFS", []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}),
		DevinAttemptTimeout:   getDuration("DEVIN_ATTEMPT_TIMEOUT", 0),
		DevinPreflightCheck:   getBool("DEVIN_PREFLIGHT_CHECK", true),
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

func getDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

func getDurationList(key string, def []time.Duration) []time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	parts := strings.Split(v, ",")
	out := make([]time.Duration, 0, len(parts))
	for _, p := range parts {
		d, err := time.ParseDuration(strings.TrimSpace(p))
		if err != nil {
			return def
		}
		out = append(out, d)
	}
	if len(out) == 0 {
		return def
	}
	return out
}

func getBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
