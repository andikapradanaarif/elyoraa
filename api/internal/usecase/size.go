package usecase

import (
	"strings"

	"github.com/andikapradanaarif/elyoraa/internal/service/github"
)

// prSize estimates how much work reviewing a PR is. Lines is the estimate
// input: changed lines excluding generated/vendored files, which inflate
// diff size without adding review effort.
type prSize struct {
	Files          int
	Additions      int
	Deletions      int
	GeneratedFiles int
	Truncated      bool // file list hit MAX_CHANGED_FILES — real size is larger
	EstMinutes     int
}

// estimatePRSize computes review-effort stats from the fetched PR info.
// linesPerMin <= 0 falls back to a sane default.
func estimatePRSize(info *github.PRInfo, linesPerMin int) prSize {
	if linesPerMin <= 0 {
		linesPerMin = 100
	}
	s := prSize{Files: len(info.Files), Truncated: info.FilesTruncated}
	lines := 0
	for _, f := range info.Files {
		if isGeneratedPath(f.Path) {
			s.GeneratedFiles++
			continue
		}
		s.Additions += f.Additions
		s.Deletions += f.Deletions
		lines += f.Additions + f.Deletions
	}
	// Ceiling division: any partial minute still costs a minute.
	s.EstMinutes = (lines + linesPerMin - 1) / linesPerMin
	return s
}

// tooBig reports whether the estimated review effort exceeds the budget.
// A truncated file list always counts as too big — the true size is unknown
// and already past the fetch cap.
func (s prSize) tooBig(budgetMin int) bool {
	if budgetMin <= 0 {
		return false
	}
	return s.EstMinutes > budgetMin || s.Truncated
}

// generatedPathMarkers are path fragments that mark files as generated or
// vendored. Their line counts are excluded from the review-time estimate.
var generatedPathMarkers = []string{
	"vendor/",
	"node_modules/",
	"dist/",
	"third_party/",
	"package-lock.json",
	"yarn.lock",
	"pnpm-lock.yaml",
	"composer.lock",
	"cargo.lock",
	"gemfile.lock",
	"poetry.lock",
	"go.sum",
	"go.work.sum",
	".pb.go",
	".gen.go",
	"_generated.",
	".min.js",
	".min.css",
	".snap",
}

func isGeneratedPath(path string) bool {
	p := strings.ToLower(path)
	for _, m := range generatedPathMarkers {
		if strings.Contains(p, m) {
			return true
		}
	}
	return false
}

// oversizeAction is what to do with a PR that exceeds the review-time
// budget, based on the comment that triggered the job.
type oversizeAction int

const (
	// oversizePropose posts a split proposal instead of a review.
	oversizePropose oversizeAction = iota
	// oversizeReview bypasses the gate: the trigger explicitly asked for a review.
	oversizeReview
	// oversizeCreate accepts a previous split proposal: create the issues.
	oversizeCreate
)

// oversizeReviewKeywords let a human force a full review of an oversize PR.
// Includes the rereview keywords — an explicit rereview request is an
// explicit review request.
var oversizeReviewKeywords = []string{
	"anyway",
	"force review",
	"force a review",
	"full review",
	"review regardless",
	"skip the split",
	"rereview",
	"re-review",
	"review again",
	"recheck",
	"check again",
}

// oversizeCreateKeywords signal a human accepted the split proposal and
// wants the breakdown filed as issues.
var oversizeCreateKeywords = []string{
	"create issue",
	"create the issue",
	"create them",
	"file issue",
	"file them",
	"make issue",
	"make them",
	"break it down",
	"breakdown",
	"split it",
	"split into",
	"go ahead",
	"into issues",
}

// classifyOversize picks the action for an oversize PR. An explicit
// review request wins over acceptance keywords ("don't split it, review
// anyway" contains both "split it" and "anyway" — the human wants a review).
func classifyOversize(triggerComment string) oversizeAction {
	body := strings.ToLower(triggerComment)
	for _, kw := range oversizeReviewKeywords {
		if strings.Contains(body, kw) {
			return oversizeReview
		}
	}
	for _, kw := range oversizeCreateKeywords {
		if strings.Contains(body, kw) {
			return oversizeCreate
		}
	}
	return oversizePropose
}
