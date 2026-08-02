package domain

import (
	"strings"
	"time"
)

// Review represents a PR review or issue analysis job.
type Review struct {
	JobKey           string
	DeliveryID       string
	Owner            string
	Repo             string
	PRNumber         int
	HeadSHA          string
	BaseSHA          string
	InstallationID   int64
	Author           string
	Title            string
	Body             string
	Type             string // "review" (default) or "issue"
	TriggerComment   string // the comment that triggered this job, if any
	RepoAvailable    bool   // whether the repo was cloned and available for analysis
	PendingCommentID int64 // issue comment ID for "Review in progress…"
	EnqueuedAt       time.Time
}

// Comment is a single inline review comment.
type Comment struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
}

// Output is the parsed Devin review output.
type Output struct {
	Summary  string    `json:"summary"`
	Comments []Comment `json:"comments"`
}

// IsIssue reports whether this is an issue analysis job (not a PR review).
func (r *Review) IsIssue() bool {
	return r.Type == "issue"
}

// IsBot returns true if the author looks like a bot.
func (r *Review) IsBot() bool {
	if r.Author == "" {
		return false
	}
	lower := strings.ToLower(r.Author)
	return strings.HasSuffix(lower, "[bot]") ||
		lower == "dependabot" ||
		lower == "dependabot[bot]" ||
		strings.HasPrefix(lower, "github-actions")
}
