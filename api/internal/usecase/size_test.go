package usecase

import (
	"strings"
	"testing"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
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
