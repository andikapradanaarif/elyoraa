package usecase

import (
	"path"
	"sort"
	"strings"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
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
	return estimatePRSizeWith(info, linesPerMin, nil)
}

// estimatePRSizeWith additionally honors a repo-level generated matcher —
// the .gitattributes linguist flags — when the caller has the worktree.
func estimatePRSizeWith(info *github.PRInfo, linesPerMin int, gen func(string) bool) prSize {
	if linesPerMin <= 0 {
		linesPerMin = 100
	}
	s := prSize{Files: len(info.Files), Truncated: info.FilesTruncated}
	lines := 0
	for _, f := range info.Files {
		if isGeneratedPath(f.Path) || (gen != nil && gen(f.Path)) {
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

// estimateFromCounts computes a coarse estimate from the size counters GitHub
// embeds in webhook payloads (no per-file data, so generated files are
// included). changedFiles > maxFiles marks the estimate as truncated — a
// file list that large can't be fully fetched or reviewed anyway.
func estimateFromCounts(additions, deletions, changedFiles, linesPerMin, maxFiles int) prSize {
	if linesPerMin <= 0 {
		linesPerMin = 100
	}
	lines := additions + deletions
	return prSize{
		Files:      changedFiles,
		Additions:  additions,
		Deletions:  deletions,
		Truncated:  maxFiles > 0 && changedFiles > maxFiles,
		EstMinutes: (lines + linesPerMin - 1) / linesPerMin,
	}
}

// refinedEstimate recomputes the enqueue-time estimate with per-file data.
// Since enqueueEst = enqueueLines/rate, multiplying by refined/enqueued
// yields refined/rate — the same estimate expressed in the enqueue-time
// rate's units, with no need to plumb the rate into the worker.
func refinedEstimate(info *github.PRInfo, j *domain.Review, fallbackLinesPerMin int, gen func(string) bool) prSize {
	s := estimatePRSizeWith(info, fallbackLinesPerMin, gen)
	lines := s.Additions + s.Deletions
	if j.SizeLines > 0 && j.SizeEstMinutes > 0 {
		s.EstMinutes = (j.SizeEstMinutes*lines + j.SizeLines - 1) / j.SizeLines
	}
	return s
}

// Calibration constants for the empirical review rate.
const (
	calibrationWindow  = 30 // recent completed reviews to sample
	calibrationMinJobs = 5  // fewer samples than this → use the configured rate
	calibrationMinRate = 30 // lines/min sanity floor (very slow reviewer)
	calibrationMaxRate = 400
)

// calibratedLinesPerMinute derives the lines-per-minute rate from observed
// durations of recently completed review jobs, so PR_REVIEW_BUDGET_MINUTES
// tracks real execution time on this deployment rather than a guessed
// constant. Job records are most-recently-updated first.
//
// The rate is calibrated per repository first — codegen-heavy repos skew a
// shared median — falling back to the global pool, then to the configured
// fallback, when there aren't enough samples.
func calibratedLinesPerMinute(records []queue.JobRecord, fallback int, owner, repo string) int {
	if r, ok := medianRate(records, func(rec *queue.JobRecord) bool {
		return rec.Owner == owner && rec.Repo == repo
	}); ok {
		return r
	}
	if r, ok := medianRate(records, nil); ok {
		return r
	}
	if fallback <= 0 {
		fallback = 100
	}
	return fallback
}

// medianRate samples the lines-per-minute of completed review jobs matching
// filter (nil = all). It prefers AttemptSeconds — the final successful
// attempt's duration — over pickup→finish wall time, which includes retry
// backoff and queue-adjacent GitHub calls.
func medianRate(records []queue.JobRecord, filter func(*queue.JobRecord) bool) (int, bool) {
	rates := make([]int, 0, calibrationWindow)
	for i := range records {
		rec := &records[i]
		if len(rates) >= calibrationWindow {
			break
		}
		if filter != nil && !filter(rec) {
			continue
		}
		// Only completed full reviews calibrate the rate: split/issue/gate
		// jobs have a different cost profile and failed runs skew it.
		lines := rec.Job.SizeLines
		if rec.RefinedLines > 0 {
			lines = rec.RefinedLines
		}
		seconds := rec.DurationSeconds
		if rec.AttemptSeconds > 0 {
			seconds = rec.AttemptSeconds
		}
		if rec.Status != queue.JobStatusCompleted ||
			(rec.Type != "" && rec.Type != "review") ||
			seconds <= 0 || lines <= 0 {
			continue
		}
		if rate := int(float64(lines) / (float64(seconds) / 60)); rate > 0 {
			rates = append(rates, rate)
		}
	}
	if len(rates) < calibrationMinJobs {
		return 0, false
	}
	sort.Ints(rates)
	med := rates[len(rates)/2]
	return min(max(med, calibrationMinRate), calibrationMaxRate), true
}

// gitattrMatcher parses a repo's .gitattributes for linguist-generated /
// linguist-vendored marks — GitHub's own classification signal. It catches
// generated code the path heuristics miss (e.g. contract/openapi.yaml).
// Returns nil when no marks exist.
func gitattrMatcher(data []byte) func(string) bool {
	var patterns []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, attr := range fields[1:] {
			name := strings.TrimPrefix(attr, "-")
			name = strings.SplitN(name, "=", 2)[0]
			if name == "linguist-generated" || name == "linguist-vendored" {
				patterns = append(patterns, strings.TrimPrefix(fields[0], "/"))
				break
			}
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	return func(p string) bool {
		for _, pat := range patterns {
			if gitattrMatch(pat, p) {
				return true
			}
		}
		return false
	}
}

// gitattrMatch matches a .gitattributes pattern (gitignore-style globs) to a
// repo-relative path. Patterns without a slash match any basename; patterns
// with a slash match the full path, where `**` spans zero or more segments.
func gitattrMatch(pattern, p string) bool {
	if !strings.Contains(pattern, "/") {
		ok, _ := path.Match(pattern, path.Base(p))
		return ok
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		for i := 0; i <= len(segs); i++ {
			if matchSegments(pat[1:], segs[i:]) {
				return true
			}
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
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
