package domain

import (
	"strings"
	"time"
)

// Review represents a PR review or issue analysis job.
type Review struct {
	JobKey             string
	DeliveryID         string
	ParentKey          string // JobKey of the respond job that dispatched this one, if any (audit trail)
	Owner              string
	Repo               string
	PRNumber           int
	HeadSHA            string
	BaseSHA            string
	InstallationID     int64
	Author             string
	Title              string
	Body               string
	Type               string   // "review" (default), "issue", "split", "respond", or "gate"
	TriggerComment     string   // the comment that triggered this job, if any
	TriggerAuthor      string   // login of the user whose comment/review/issue triggered this job
	TriggerAssociation string   // webhook author_association of the trigger ("MEMBER", "NONE", …)
	RepoAvailable      bool     // whether the repo was cloned and available for analysis
	PendingCommentID   int64    // issue comment ID for "Review in progress…"
	SplitCreate        bool     // split job: a human accepted the proposal — emit create_issues
	Origin             string   // Origin* constant: how this job's intent was established
	SizeEstMinutes     int      // estimated review effort at enqueue time
	SizeLines          int      // changed lines that fed the estimate (calibration + refine)
	SizeOversize       bool     // enqueue-time verdict: estimate or file-cap exceeded budget
	BudgetMinutes      int      // effective review budget at enqueue (repo config or global)
	PRLabels           []string // labels on the PR/issue at enqueue (skip-label check)
	IgnoredPaths       []string // repo-config path patterns excluded from refined sizing
	EnqueuedAt         time.Time
}

// RepoConfig is the per-repository `.elyoraa.yml` read from the default
// branch of the bare clone. Every field is optional; unset means the global
// config default applies. Read from the DEFAULT branch — never the PR head —
// so a hostile PR can't loosen its own review.
type RepoConfig struct {
	Enabled        *bool    `yaml:"enabled"`          // default true; false drops all jobs silently
	BudgetMinutes  int      `yaml:"budget_minutes"`   // override PR_REVIEW_BUDGET_MINUTES
	LinesPerMinute int      `yaml:"lines_per_minute"` // override PR_LINES_PER_MINUTE (pre-calibration)
	IgnoredPaths   []string `yaml:"ignored_paths"`    // extra glob patterns excluded from sizing
	MentionRouter  string   `yaml:"mention_router"`   // "keywords" | "agent"
	SizeGateMode   string   `yaml:"size_gate_mode"`   // "deterministic" | "agent"
	SkipLabel      string   `yaml:"skip_label"`       // opt-out label, default "elyoraa:skip"
}

// Job origins, ordered by intent strength. Dedup uses the rank to decide
// same-key supersession: explicit human intent beats automated work, and
// automated work never supersedes.
const (
	// OriginAuto is automated work: PR-opened reviews, size-gate splits and
	// gate nominations. Empty Origin ranks the same.
	OriginAuto = "auto"
	// OriginMentionKeyword is intent inferred from a fuzzy keyword match in
	// a mention (keywords router mode).
	OriginMentionKeyword = "mention_keyword"
	// OriginDispatched is intent declared by the respond agent or an exact
	// slash command (/review, /split) — a deliberate instruction.
	OriginDispatched = "dispatched"
	// OriginAccepted is an explicit acceptance of an outstanding proposal
	// (acceptance keywords, /accept, or an agent-declared create). Strongest.
	OriginAccepted = "accepted"
)

// IntentRank returns the job's origin strength: auto(0) < mention_keyword(1)
// < dispatched(2) < accepted(3).
func (r *Review) IntentRank() int {
	switch r.Origin {
	case OriginAccepted:
		return 3
	case OriginDispatched:
		return 2
	case OriginMentionKeyword:
		return 1
	default:
		return 0
	}
}

// HasExplicitIntent reports whether a human deliberately chose this job's
// type (dispatched or accepted). Such jobs are never demoted by size
// re-measurement — the reason nexus#486 lost an acceptance.
func (r *Review) HasExplicitIntent() bool {
	return r.IntentRank() >= 2
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

// IsReview reports whether this is a normal code-review job.
func (r *Review) IsReview() bool {
	return r.Type == "" || r.Type == "review"
}

// IsRespond reports whether this is a mention-routing (respond) job.
func (r *Review) IsRespond() bool {
	return r.Type == "respond"
}

// IsGate reports whether this is a size-triage (gate) job.
func (r *Review) IsGate() bool {
	return r.Type == "gate"
}

// IsPR reports whether this job is about a pull request rather than an issue.
func (r *Review) IsPR() bool {
	return r.HeadSHA != ""
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
