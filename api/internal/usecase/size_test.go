package usecase

import (
	"strings"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/queue"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
)

func TestEstimatePRSize(t *testing.T) {
	info := &github.PRInfo{
		Files: []*github.FileInfo{
			{Path: "a.go", Additions: 100, Deletions: 50},
			{Path: "b.go", Additions: 200, Deletions: 0},
		},
	}
	s := estimatePRSize(info, 100)
	if s.Files != 2 || s.Additions != 300 || s.Deletions != 50 {
		t.Fatalf("bad totals: %+v", s)
	}
	// 350 lines / 100 lpm = 3.5 -> 4 min (ceiling)
	if s.EstMinutes != 4 {
		t.Fatalf("EstMinutes = %d, want 4", s.EstMinutes)
	}
}

func TestEstimatePRSizeExcludesGenerated(t *testing.T) {
	info := &github.PRInfo{
		Files: []*github.FileInfo{
			{Path: "main.go", Additions: 10, Deletions: 5},
			{Path: "go.sum", Additions: 9000, Deletions: 9000},
			{Path: "api.pb.go", Additions: 4000, Deletions: 0},
			{Path: "vendor/foo/bar.go", Additions: 5000, Deletions: 0},
		},
	}
	s := estimatePRSize(info, 100)
	if s.GeneratedFiles != 3 {
		t.Fatalf("GeneratedFiles = %d, want 3", s.GeneratedFiles)
	}
	if s.Additions != 10 || s.Deletions != 5 {
		t.Fatalf("generated lines leaked into estimate: %+v", s)
	}
	if s.EstMinutes != 1 {
		t.Fatalf("EstMinutes = %d, want 1", s.EstMinutes)
	}
}

func TestEstimateFromCounts(t *testing.T) {
	// Webhook-payload counters: no per-file data, generated files included.
	s := estimateFromCounts(1200, 400, 12, 100, 50)
	if s.Files != 12 || s.EstMinutes != 16 {
		t.Fatalf("bad estimate: %+v", s)
	}
	if !s.tooBig(15) {
		t.Fatal("1600 lines should exceed 15min budget")
	}

	// File count over the fetch cap counts as truncated even with few lines.
	s = estimateFromCounts(200, 100, 60, 100, 50)
	if !s.Truncated || !s.tooBig(15) {
		t.Fatalf("60 files > 50 cap should be too big: %+v", s)
	}
}

func TestPRSizeTooBig(t *testing.T) {
	s := prSize{EstMinutes: 20}
	if !s.tooBig(15) {
		t.Fatal("20min > 15min budget should be too big")
	}
	s.EstMinutes = 15
	if s.tooBig(15) {
		t.Fatal("15min == 15min budget should not be too big")
	}
	s.EstMinutes = 3
	s.Truncated = true
	if !s.tooBig(15) {
		t.Fatal("truncated file list should always be too big")
	}
	s.Truncated = true
	if s.tooBig(0) {
		t.Fatal("zero budget disables the gate")
	}
}

func TestClassifyOversize(t *testing.T) {
	cases := []struct {
		name    string
		trigger string
		want    oversizeAction
	}{
		{"empty (auto-review)", "", oversizePropose},
		{"plain mention", "@elyoraa what do you think?", oversizePropose},
		{"create issues", "@elyoraa create the issues", oversizeCreate},
		{"break it down", "yes, break it down please", oversizeCreate},
		{"go ahead", "go ahead", oversizeCreate},
		{"review anyway", "@elyoraa review anyway", oversizeReview},
		{"rereview", "@elyoraa rereview", oversizeReview},
		{"re-review", "@elyoraa re-review", oversizeReview},
		{"full review", "please do a full review", oversizeReview},
		{"review wins over create", "don't split it, just review anyway", oversizeReview},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyOversize(tc.trigger); got != tc.want {
				t.Fatalf("classifyOversize(%q) = %v, want %v", tc.trigger, got, tc.want)
			}
		})
	}
}

func TestCalibratedLinesPerMinute(t *testing.T) {
	doneAt := time.Now()
	rec := func(lines int, secs int64, status, typ string) queue.JobRecord {
		st := doneAt.Add(-time.Duration(secs) * time.Second)
		return queue.JobRecord{
			Status:          status,
			Type:            typ,
			DurationSeconds: secs,
			StartedAt:       &st,
			FinishedAt:      &doneAt,
			Job:             domain.Review{SizeLines: lines},
		}
	}

	// No history → fallback.
	if got := calibratedLinesPerMinute(nil, 100); got != 100 {
		t.Fatalf("empty history should use fallback, got %d", got)
	}
	if got := calibratedLinesPerMinute(nil, 0); got != 100 {
		t.Fatalf("empty history with zero fallback should use default, got %d", got)
	}

	// Fewer than calibrationMinJobs samples → fallback even with data.
	few := []queue.JobRecord{rec(1000, 600, queue.JobStatusCompleted, "")}
	if got := calibratedLinesPerMinute(few, 100); got != 100 {
		t.Fatalf("too few samples should use fallback, got %d", got)
	}

	// 5 completed reviews at 100 lines/min → calibrated rate 100.
	var recs []queue.JobRecord
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(1000, 600, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 5); got != 100 {
		t.Fatalf("calibrated rate = %d, want 100", got)
	}

	// Slow deployment: 1000 lines in 20 min → 50 lpm.
	recs = nil
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(1000, 1200, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 100); got != 50 {
		t.Fatalf("calibrated rate = %d, want 50", got)
	}

	// Noise is filtered: split/issue/failed jobs and zero-line payloads.
	recs = nil
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(1000, 600, queue.JobStatusCompleted, ""))
	}
	recs = append(recs,
		rec(9000, 30, queue.JobStatusCompleted, "split"),
		rec(9000, 30, queue.JobStatusCompleted, "issue"),
		rec(9000, 30, queue.JobStatusFailed, ""),
		rec(0, 600, queue.JobStatusCompleted, ""),
	)
	if got := calibratedLinesPerMinute(recs, 5); got != 100 {
		t.Fatalf("noise should be filtered, got %d", got)
	}

	// Outlier clamp: insanely fast samples clamp to the ceiling.
	recs = nil
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(100000, 1, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 100); got != calibrationMaxRate {
		t.Fatalf("should clamp to %d, got %d", calibrationMaxRate, got)
	}
}

func TestRefinedEstimate(t *testing.T) {
	// Enqueue estimate came from raw webhook counters (generated included);
	// per-file data refines it by the same ratio.
	j := &domain.Review{SizeEstMinutes: 20, SizeLines: 2000}
	info := &github.PRInfo{Files: []*github.FileInfo{
		{Path: "a.go", Additions: 100, Deletions: 100}, // real: 200 lines
		{Path: "go.sum", Additions: 900, Deletions: 900},
	}}
	s := refinedEstimate(info, j, 100)
	// 20min * 200/2000 = 2min — generated lines dominated the raw count.
	if s.EstMinutes != 2 {
		t.Fatalf("refined EstMinutes = %d, want 2", s.EstMinutes)
	}
	if s.tooBig(15) {
		t.Fatal("should demote: refined estimate under budget")
	}

	// No enqueue stats → keep the fallback-rate estimate.
	j2 := &domain.Review{}
	s = refinedEstimate(info, j2, 100)
	if s.EstMinutes != 2 {
		t.Fatalf("fallback EstMinutes = %d, want 2", s.EstMinutes)
	}
}

func TestIsGeneratedPath(t *testing.T) {
	for _, p := range []string{
		"vendor/github.com/x/y.go",
		"go.sum",
		"pkg/api.pb.go",
		"web/app.min.js",
		"sub/PACKAGE-LOCK.JSON",
		"ui/node_modules/lib/index.js",
	} {
		if !isGeneratedPath(p) {
			t.Fatalf("%q should be generated", p)
		}
	}
	for _, p := range []string{"main.go", "internal/usecase/review.go", "README.md", "go.mod"} {
		if isGeneratedPath(p) {
			t.Fatalf("%q should not be generated", p)
		}
	}
}

func TestRenderSplitPrompt(t *testing.T) {
	r := &ReviewUsecase{cfg: &config.Config{
		PromptsDir:            "../../prompts",
		BotHandle:             "elyoraa",
		PRReviewBudgetMinutes: 15,
	}}
	j := &domain.Review{
		Owner:    "o",
		Repo:     "r",
		PRNumber: 12,
		Title:    "Huge refactor",
		Author:   "alice",
		Body:     "moves everything",
		HeadSHA:  "abc123",
		BaseSHA:  "def456",
	}
	prInfo := &github.PRInfo{
		Files: []*github.FileInfo{
			{Path: "a.go", Additions: 800, Deletions: 900, Status: "modified"},
		},
		PatchTooBig: true,
	}
	size := prSize{Files: 42, Additions: 800, Deletions: 900, EstMinutes: 17, Truncated: true}

	out, err := r.renderSplitPrompt(j, prInfo, size, false, nil)
	if err != nil {
		t.Fatalf("renderSplitPrompt: %v", err)
	}
	for _, want := range []string{
		"Huge refactor", "~17 min", "15 min", "42", "a.go",
		"git diff def456...abc123", "truncated",
		"create the issues", "review anyway",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("split prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<no value>") {
		t.Fatalf("prompt has unresolved template keys:\n%s", out)
	}
	// Propose mode must not instruct issue creation.
	if strings.Contains(out, "create_issues") {
		t.Fatal("propose mode should not mention create_issues block")
	}

	// Create mode: thread + create_issues instructions present.
	thread := []github.IssueComment{
		{Author: "elyoraa[bot]", Body: "proposed split into 3 PRs", IsBot: true},
		{Author: "alice", Body: "@elyoraa create the issues"},
	}
	out, err = r.renderSplitPrompt(j, prInfo, size, true, thread)
	if err != nil {
		t.Fatalf("renderSplitPrompt create mode: %v", err)
	}
	for _, want := range []string{"create_issues", "accepted", "proposed split into 3 PRs"} {
		if !strings.Contains(out, want) {
			t.Fatalf("create-mode prompt missing %q:\n%s", want, out)
		}
	}
}
