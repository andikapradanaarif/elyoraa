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
	// QueueJobLease bounds how long a Postgres queue claim may run before
	// another replica may reclaim it. 0 = ReviewTimeout + 5m.
	QueueJobLease         time.Duration
	MaxPatchBytes         int
	MaxChangedFiles       int
	PRSizeGate            bool
	PRReviewBudgetMinutes int
	PRLinesPerMinute      int
	LogLevel              string
	MetricsPath           string
	ReviewTimeout         time.Duration
	CommentTimeout        time.Duration
	HTTPReadTimeout       time.Duration
	HTTPWriteTimeout      time.Duration
	DevinMaxRetries       int
	DevinRetryBackoffs    []time.Duration
	DevinAttemptTimeout   time.Duration
	DevinPreflightCheck   bool
	MentionRouter         string        // "keywords" (default) or "agent"
	SizeGateMode          string        // "deterministic" (default) or "agent"
	RespondModel          string        // default DevinModel
	RespondTimeout        time.Duration // default ReviewTimeout
	RespondMaxActions     int
	GateModel             string        // default RespondModel, then DevinModel
	GateTimeout           time.Duration // triage-pass budget only; the job itself gets ReviewTimeout
	// DevinSandbox runs every Devin invocation under the OS-level sandbox
	// (--sandbox / autonomous mode), which enforces Read(...) deny rules for
	// credentials even against shell commands. Requires bubblewrap+socat on
	// Linux; Devin fails closed when the sandbox can't be set up.
	DevinSandbox bool
	// AdminToken protects /jobs* and the metrics path. When empty, those
	// endpoints return 404.
	AdminToken string
	// MentionRateLimit caps mention-triggered jobs per author per repo per
	// MentionRateWindow; 0 disables.
	MentionRateLimit  int
	MentionRateWindow time.Duration
	// ShutdownDrainTimeout bounds how long workers finish in-flight jobs on
	// shutdown; <=0 falls back to ReviewTimeout.
	ShutdownDrainTimeout time.Duration
	// WebhookMaxBytes caps inbound webhook payload size.
	WebhookMaxBytes int64
	// AuditRetentionDays bounds webhook_events retention; <=0 disables the
	// daily cleanup.
	AuditRetentionDays int
	// DailyJobCapPerRepo bounds started jobs per repo per UTC day; 0 disables.
	DailyJobCapPerRepo int
	// Paused drops all webhook processing while leaving the server healthy —
	// a global kill switch.
	Paused bool
	// PRSkipLabel is the per-PR opt-out label — auto-reviews are suppressed
	// while it's present; explicit mentions still run. Empty disables.
	PRSkipLabel string
	// GitBaseURL prefixes clone-on-demand URLs (default https://github.com).
	// Override for GitHub Enterprise or file:// in tests.
	GitBaseURL string
	// GitAskpassPath is the GIT_ASKPASS helper script used for credentialed
	// git operations — by the server's fetches and by the agent's repo-sync.
	GitAskpassPath string
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
		QueueJobLease:         getDuration("QUEUE_JOB_LEASE", 0),
		MaxPatchBytes:         getInt("MAX_PATCH_BYTES", 100_000),
		MaxChangedFiles:       getInt("MAX_CHANGED_FILES", 50),
		PRSizeGate:            getBool("PR_SIZE_GATE", true),
		PRReviewBudgetMinutes: getInt("PR_REVIEW_BUDGET_MINUTES", 15),
		PRLinesPerMinute:      getInt("PR_LINES_PER_MINUTE", 100),
		LogLevel:              getEnv("LOG_LEVEL", "info"),
		MetricsPath:           getEnv("METRICS_PATH", "/metrics"),
		ReviewTimeout:         getDuration("REVIEW_TIMEOUT", 30*time.Minute),
		CommentTimeout:        getDuration("COMMENT_TIMEOUT", time.Minute),
		HTTPReadTimeout:       getDuration("HTTP_READ_TIMEOUT", 10*time.Second),
		HTTPWriteTimeout:      getDuration("HTTP_WRITE_TIMEOUT", 10*time.Second),
		DevinMaxRetries:       getInt("DEVIN_MAX_RETRIES", 3),
		DevinRetryBackoffs:    getDurationList("DEVIN_RETRY_BACKOFFS", []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}),
		DevinAttemptTimeout:   getDuration("DEVIN_ATTEMPT_TIMEOUT", 0),
		DevinPreflightCheck:   getBool("DEVIN_PREFLIGHT_CHECK", true),
		MentionRouter:         getEnv("MENTION_ROUTER", "keywords"),
		SizeGateMode:          getEnv("SIZE_GATE_MODE", "deterministic"),
		RespondModel:          getEnv("RESPOND_MODEL", ""),
		RespondTimeout:        getDuration("RESPOND_TIMEOUT", 0),
		RespondMaxActions:     getInt("RESPOND_MAX_ACTIONS", 4),
		GateModel:             getEnv("GATE_MODEL", ""),
		GateTimeout:           getDuration("GATE_TIMEOUT", 5*time.Minute),
		DevinSandbox:          getBool("DEVIN_SANDBOX", true),
		AdminToken:            os.Getenv("ADMIN_TOKEN"),
		MentionRateLimit:      getInt("MENTION_RATE_LIMIT", 20),
		MentionRateWindow:     getDuration("MENTION_RATE_WINDOW", time.Hour),
		ShutdownDrainTimeout:  getDuration("SHUTDOWN_DRAIN_TIMEOUT", 0),
		WebhookMaxBytes:       int64(getInt("WEBHOOK_MAX_BYTES", 5<<20)),
		AuditRetentionDays:    getInt("AUDIT_RETENTION_DAYS", 30),
		DailyJobCapPerRepo:    getInt("DAILY_JOB_CAP_PER_REPO", 0),
		Paused:                getBool("ELYORAA_PAUSED", false),
		PRSkipLabel:           getEnv("PR_SKIP_LABEL", "elyoraa:skip"),
		GitBaseURL:            getEnv("GIT_BASE_URL", ""),
		GitAskpassPath:        getEnv("GIT_ASKPASS_PATH", "/app/git-askpass.sh"),
	}

	if cfg.RespondTimeout <= 0 {
		cfg.RespondTimeout = cfg.ReviewTimeout
	}
	if cfg.ShutdownDrainTimeout <= 0 {
		cfg.ShutdownDrainTimeout = cfg.ReviewTimeout
	}
	if budget := time.Duration(cfg.PRReviewBudgetMinutes) * time.Minute; cfg.ReviewTimeout < budget {
		fmt.Fprintf(os.Stderr, "warning: REVIEW_TIMEOUT (%s) < PR_REVIEW_BUDGET_MINUTES (%s) — budget-sized reviews will be killed mid-run; keep REVIEW_TIMEOUT >= the budget\n", cfg.ReviewTimeout, budget)
	}
	if cfg.MentionRateLimit < 0 {
		return nil, fmt.Errorf("MENTION_RATE_LIMIT must be >= 0, got %d", cfg.MentionRateLimit)
	}
	if cfg.MentionRateWindow <= 0 {
		return nil, fmt.Errorf("MENTION_RATE_WINDOW must be > 0, got %s", cfg.MentionRateWindow)
	}
	if cfg.WebhookMaxBytes <= 0 {
		return nil, fmt.Errorf("WEBHOOK_MAX_BYTES must be > 0, got %d", cfg.WebhookMaxBytes)
	}
	if cfg.DailyJobCapPerRepo < 0 {
		return nil, fmt.Errorf("DAILY_JOB_CAP_PER_REPO must be >= 0, got %d", cfg.DailyJobCapPerRepo)
	}
	if cfg.AdminToken == "" {
		fmt.Fprintln(os.Stderr, "warning: ADMIN_TOKEN is unset — /jobs* and the metrics endpoint will return 404")
	}
	if cfg.RespondMaxActions < 1 {
		return nil, fmt.Errorf("RESPOND_MAX_ACTIONS must be >= 1, got %d", cfg.RespondMaxActions)
	}

	switch cfg.MentionRouter {
	case "agent", "keywords":
	default:
		return nil, fmt.Errorf("MENTION_ROUTER must be 'agent' or 'keywords', got %q", cfg.MentionRouter)
	}
	switch cfg.SizeGateMode {
	case "agent", "deterministic":
	default:
		return nil, fmt.Errorf("SIZE_GATE_MODE must be 'agent' or 'deterministic', got %q", cfg.SizeGateMode)
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
