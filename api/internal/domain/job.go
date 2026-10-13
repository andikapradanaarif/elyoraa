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
	Type             string // "review" (default), "issue", or "split"
	TriggerComment   string // the comment that triggered this job, if any
	RepoAvailable    bool   // whether the repo was cloned and available for analysis
	PendingCommentID int64  // issue comment ID for "Review in progress…"
	SplitCreate      bool   // split job: a human accepted the proposal — emit create_issues
	SizeEstMinutes   int    // estimated review effort at enqueue time
	SizeLines        int    // changed lines that fed the estimate (calibration + refine)
	EnqueuedAt       time.Time
}

// Comment is a single inline review comment.
type Comment struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Body     string `json:"body"`
}

// FollowUp is an out-of-scope improvement suggested as a separate GitHub issue.
type FollowUp struct {
	Title       string `json:"title"`
	Description string `json:"description"`
}

// ProposedIssue is a sub-issue the bot creates once a human accepts a
// proposed breakdown in an issue discussion.
type ProposedIssue struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

// PriorFindingStatus records how a finding from the previous bot review
// fared when re-verified against the current diff.
type PriorFindingStatus struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Status string `json:"status"` // "fixed", "still_present", or "unclear"
}

// Output is the parsed Devin review output.
type Output struct {
	Summary           string               `json:"summary"`
	PassesCompleted   []string             `json:"passes_completed"`
	FilesReviewed     []string             `json:"files_reviewed"`
	HighSeveritySweep string               `json:"high_severity_sweep"`
	Comments          []Comment            `json:"comments"`
	FollowUps         []FollowUp           `json:"follow_ups"`
	PriorFindings     []PriorFindingStatus `json:"prior_findings_status"`
}

// IsIssue reports whether this is an issue analysis job (not a PR review).
func (r *Review) IsIssue() bool {
	return r.Type == "issue"
}

// IsSplit reports whether this is a split-proposal job for an oversize PR.
func (r *Review) IsSplit() bool {
	return r.Type == "split"
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
