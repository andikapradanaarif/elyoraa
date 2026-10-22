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
			Owner:           "o",
			Repo:            "r",
			DurationSeconds: secs,
			StartedAt:       &st,
			FinishedAt:      &doneAt,
			Job:             domain.Review{SizeLines: lines},
		}
	}

	// No history → fallback.
	if got := calibratedLinesPerMinute(nil, 100, "o", "r"); got != 100 {
		t.Fatalf("empty history should use fallback, got %d", got)
	}
	if got := calibratedLinesPerMinute(nil, 0, "o", "r"); got != 100 {
		t.Fatalf("empty history with zero fallback should use default, got %d", got)
	}

	// Fewer than calibrationMinJobs samples → fallback even with data.
	few := []queue.JobRecord{rec(1000, 600, queue.JobStatusCompleted, "")}
	if got := calibratedLinesPerMinute(few, 100, "o", "r"); got != 100 {
		t.Fatalf("too few samples should use fallback, got %d", got)
	}

	// 5 completed reviews at 100 lines/min → calibrated rate 100.
	var recs []queue.JobRecord
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(1000, 600, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 5, "o", "r"); got != 100 {
		t.Fatalf("calibrated rate = %d, want 100", got)
	}

	// Slow deployment: 1000 lines in 20 min → 50 lpm.
	recs = nil
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(1000, 1200, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 100, "o", "r"); got != 50 {
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
	if got := calibratedLinesPerMinute(recs, 5, "o", "r"); got != 100 {
		t.Fatalf("noise should be filtered, got %d", got)
	}

	// Outlier clamp: insanely fast samples clamp to the ceiling.
	recs = nil
	for i := 0; i < 6; i++ {
		recs = append(recs, rec(100000, 1, queue.JobStatusCompleted, ""))
	}
	if got := calibratedLinesPerMinute(recs, 100, "o", "r"); got != calibrationMaxRate {
		t.Fatalf("should clamp to %d, got %d", calibrationMaxRate, got)
	}
}

// The rate is per-repository: a codegen-heavy repo shouldn't skew a normal
// repo's estimate. With too few local samples the global pool is the fallback.
func TestCalibratedLinesPerMinute_PerRepo(t *testing.T) {
	doneAt := time.Now()
	rec := func(owner, repo string, lines int, secs int64) queue.JobRecord {
		st := doneAt.Add(-time.Duration(secs) * time.Second)
		return queue.JobRecord{
			Status: queue.JobStatusCompleted, Type: "review",
			Owner: owner, Repo: repo,
			DurationSeconds: secs, StartedAt: &st, FinishedAt: &doneAt,
			Job: domain.Review{SizeLines: lines},
		}
	}

	var recs []queue.JobRecord
	for i := 0; i < 6; i++ {
		recs = append(recs, rec("o", "gen-repo", 100000, 60)) // codegen repo: ~1667 lpm → clamps at 400
	}
	for i := 0; i < 6; i++ {
		recs = append(recs, rec("o", "app-repo", 1000, 1200)) // normal repo: 50 lpm
	}

	if got := calibratedLinesPerMinute(recs, 100, "o", "app-repo"); got != 50 {
		t.Fatalf("app-repo rate should be 50 lpm, got %d", got)
	}
	if got := calibratedLinesPerMinute(recs, 100, "o", "gen-repo"); got != calibrationMaxRate {
		t.Fatalf("gen-repo rate should clamp to %d, got %d", calibrationMaxRate, got)
	}
	// An unseen repo falls back to the global pool (median of all samples).
	if got := calibratedLinesPerMinute(recs, 100, "o", "new-repo"); got == 0 {
		t.Fatal("unseen repo should fall back to the global pool")
	}
}

// AttemptSeconds — the final successful attempt — is preferred over
// pickup→finish duration so retry backoff doesn't depress the rate.
func TestCalibratedLinesPerMinute_AttemptPreferred(t *testing.T) {
	doneAt := time.Now()
	var recs []queue.JobRecord
	for i := 0; i < 6; i++ {
		st := doneAt.Add(-30 * time.Minute)
		recs = append(recs, queue.JobRecord{
			Status: queue.JobStatusCompleted, Type: "review", Owner: "o", Repo: "r",
			DurationSeconds: 1800, // 30 min wall incl. retry backoff
			AttemptSeconds:  600,  // the successful attempt took 10 min
			StartedAt:       &st, FinishedAt: &doneAt,
			Job: domain.Review{SizeLines: 1000},
		})
	}
	// 1000 lines / 10min attempt = 100 lpm — not 1000/30 = 33.
	if got := calibratedLinesPerMinute(recs, 100, "o", "r"); got != 100 {
		t.Fatalf("should calibrate on attempt time (100), got %d", got)
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
	s := refinedEstimate(info, j, 100, nil)
	// 20min * 200/2000 = 2min — generated lines dominated the raw count.
	if s.EstMinutes != 2 {
		t.Fatalf("refined EstMinutes = %d, want 2", s.EstMinutes)
	}
	if s.tooBig(15) {
		t.Fatal("should demote: refined estimate under budget")
	}

	// No enqueue stats → keep the fallback-rate estimate.
	j2 := &domain.Review{}
	s = refinedEstimate(info, j2, 100, nil)
	if s.EstMinutes != 2 {
		t.Fatalf("fallback EstMinutes = %d, want 2", s.EstMinutes)
	}
}

// The repo's .gitattributes linguist marks join the path heuristics when the
// worktree is readable — generated files the markers miss are still excluded.
func TestGitattributesGenerated(t *testing.T) {
	attrs := `
# comment
*.pb.go linguist-generated
contract/openapi.yaml linguist-generated=true
web/dist/** linguist-vendored
**/fixtures/** linguist-generated
docs/* linguist-documentation
`
	m := gitattrMatcher([]byte(attrs))
	if m == nil {
		t.Fatal("expected a matcher")
	}
	for _, p := range []string{
		"api.pb.go",
		"pkg/api.pb.go",
		"contract/openapi.yaml",
		"web/dist/app.js",
		"web/dist/sub/app.js",
		"test/fixtures/big.json",
		"a/b/fixtures/deep/x.json",
	} {
		if !m(p) {
			t.Fatalf("%q should match linguist patterns", p)
		}
	}
	for _, p := range []string{"main.go", "docs/guide.md", "web/src/app.js", "contract/api.yaml"} {
		if m(p) {
			t.Fatalf("%q must not match", p)
		}
	}

	// The matcher combines with the built-in heuristics in estimation.
	info := &github.PRInfo{Files: []*github.FileInfo{
		{Path: "main.go", Additions: 100, Deletions: 0},
		{Path: "contract/openapi.yaml", Additions: 5000, Deletions: 0},
	}}
	s := estimatePRSizeWith(info, 100, m)
	if s.GeneratedFiles != 1 || s.Additions != 100 {
		t.Fatalf("gitattributes-marked file not excluded: %+v", s)
	}

	if gitattrMatcher([]byte("*.go\n")) != nil {
		t.Fatal("no linguist marks → nil matcher")
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
		// nexus#553: a lockfile-heavy PR must not be measured by raw lines.
		"bun.lock",
		"packages/api/bun.lockb",
		"uv.lock",
		".terraform.lock.hcl",
		"backoffice/src/routeTree.gen.ts",
		"proto/gen/api.pb.cc",
		"src/generated/client.go",
		"app/__generated__/Query.graphql.ts",
		"Package.resolved",
	} {
		if !isGeneratedPath(p) {
			t.Fatalf("%q should be generated", p)
		}
	}
	for _, p := range []string{
		"main.go",
		"internal/usecase/review.go",
		"README.md",
		"go.mod",
		"docs/deadlock.md",  // "lock" alone is not a lockfile
		"src/blocklist.go",  // idem
		"scripts/gen.ts",    // *.gen.* needs a leading dot segment
		"docs/generated.md", // a file named generated*, not a generated/ dir
	} {
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
	thread := []ConversationEntry{
		{Author: "elyoraa[bot]", Body: "proposed split into 3 PRs", IsBot: true, Kind: "comment"},
		{Author: "alice", Body: "@elyoraa create the issues", Kind: "comment"},
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
