package usecase

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/repository/cache"
)

func TestParseRepoConfig(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want domain.RepoConfig
	}{
		{"empty", "", domain.RepoConfig{}},
		{"valid full", `
enabled: false
budget_minutes: 30
lines_per_minute: 60
ignored_paths: ["vendor/", "*.pb.go"]
mention_router: agent
size_gate_mode: agent
skip_label: "custom:skip"
`, domain.RepoConfig{
			Enabled:        boolPtr(false),
			BudgetMinutes:  30,
			LinesPerMinute: 60,
			IgnoredPaths:   []string{"vendor/", "*.pb.go"},
			MentionRouter:  "agent",
			SizeGateMode:   "agent",
			SkipLabel:      "custom:skip",
		}},
		{"invalid yaml", "enabled: [not yaml", domain.RepoConfig{}},
		{"bad router", "mention_router: rogue\nsize_gate_mode: agent\n",
			domain.RepoConfig{SizeGateMode: "agent"}},
		{"bad gate mode", "size_gate_mode: paranoia\nmention_router: agent\n",
			domain.RepoConfig{MentionRouter: "agent"}},
		{"enabled true", "enabled: true\n", domain.RepoConfig{Enabled: boolPtr(true)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRepoConfig([]byte(tc.yaml), "o/r")
			if got.BudgetMinutes != tc.want.BudgetMinutes ||
				got.LinesPerMinute != tc.want.LinesPerMinute ||
				got.MentionRouter != tc.want.MentionRouter ||
				got.SizeGateMode != tc.want.SizeGateMode ||
				got.SkipLabel != tc.want.SkipLabel ||
				len(got.IgnoredPaths) != len(tc.want.IgnoredPaths) ||
				(got.Enabled == nil) != (tc.want.Enabled == nil) ||
				(got.Enabled != nil && *got.Enabled != *tc.want.Enabled) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestRepoResolvers(t *testing.T) {
	cfg := &config.Config{
		PRReviewBudgetMinutes: 15,
		PRLinesPerMinute:      100,
		MentionRouter:         "keywords",
		SizeGateMode:          "deterministic",
		PRSkipLabel:           "elyoraa:skip",
	}
	rc := domain.RepoConfig{
		BudgetMinutes:  30,
		LinesPerMinute: 50,
		MentionRouter:  "agent",
		SizeGateMode:   "agent",
		SkipLabel:      "repo:skip",
	}
	if got := budgetMinutesFor(cfg, rc); got != 30 {
		t.Fatalf("budget override: %d", got)
	}
	if got := budgetMinutesFor(cfg, domain.RepoConfig{}); got != 15 {
		t.Fatalf("budget fallback: %d", got)
	}
	if got := linesPerMinuteFor(cfg, rc); got != 50 {
		t.Fatalf("lpm override: %d", got)
	}
	if got := mentionRouterFor(cfg, rc); got != "agent" {
		t.Fatalf("router override: %s", got)
	}
	if got := mentionRouterFor(cfg, domain.RepoConfig{}); got != "keywords" {
		t.Fatalf("router fallback: %s", got)
	}
	if got := sizeGateModeFor(cfg, rc); got != "agent" {
		t.Fatalf("gate override: %s", got)
	}
	if got := skipLabelFor(cfg, rc); got != "repo:skip" {
		t.Fatalf("label override: %s", got)
	}
	if got := skipLabelFor(cfg, domain.RepoConfig{}); got != "elyoraa:skip" {
		t.Fatalf("label fallback: %s", got)
	}

	// jobBudget prefers the value persisted on the job.
	if got := jobBudget(cfg, domain.Review{BudgetMinutes: 7}); got != 7 {
		t.Fatalf("jobBudget job value: %d", got)
	}
	if got := jobBudget(cfg, domain.Review{}); got != 15 {
		t.Fatalf("jobBudget fallback: %d", got)
	}
}

func TestIgnoredPath(t *testing.T) {
	patterns := []string{"vendor/", "*.pb.go", "docs/**", "", "  "}
	cases := []struct {
		path string
		want bool
	}{
		{"vendor/foo/bar.go", true}, // subtree
		{"src/vendor/foo.go", true}, // bare "vendor" is unanchored, like .gitignore
		{"api/gen.pb.go", true},     // basename glob matches at any depth
		{"gen.pb.go", true},         // basename glob at root
		{"docs/a/b/c.md", true},     // ** spans segments
		{"docs/x.md", true},         // ** spans zero segments
		{"docs.md", false},          // docs/** needs a docs/ prefix
		{"main.go", false},
	}
	for _, tc := range cases {
		if got := ignoredPath(patterns, tc.path); got != tc.want {
			t.Errorf("ignoredPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

// The repo config must come from the default branch — a PR head containing
// its own (hostile) .elyoraa.yml must never be consulted.
func TestLoadRepoConfig_ReadsDefaultBranchOnly(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	run := func(d string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", args, err, out)
		}
	}

	src := filepath.Join(dir, "src")
	run(dir, "init", "-b", "main", "src")
	run(src, "config", "user.email", "t@t")
	run(src, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(src, repoConfigFile), []byte("enabled: true\nbudget_minutes: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", ".")
	run(src, "commit", "-m", "main config")

	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "o"), 0o755); err != nil {
		t.Fatal(err)
	}
	run(dir, "clone", "--bare", src, filepath.Join(base, "o", "r.git"))

	// Simulate a PR branch pushing a hostile config to refs/pull/1/head.
	run(src, "checkout", "-b", "pr-head")
	if err := os.WriteFile(filepath.Join(src, repoConfigFile), []byte("enabled: false\nbudget_minutes: 99999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "commit", "-am", "hostile config")
	run(src, "push", filepath.Join(base, "o", "r.git"), "HEAD:refs/pull/1/head")

	c := cache.New(base, "", t.TempDir(), "")
	rc := loadRepoConfig(context.Background(), c, "o", "r")
	if !repoEnabled(rc) {
		t.Fatal("PR-head config leaked into repo config — must read HEAD only")
	}
	if rc.BudgetMinutes != 99 {
		t.Fatalf("expected main-branch budget 99, got %d", rc.BudgetMinutes)
	}
}

func TestLoadRepoConfig_NoCache(t *testing.T) {
	rc := loadRepoConfig(context.Background(), nil, "o", "r")
	if !repoEnabled(rc) || rc.BudgetMinutes != 0 {
		t.Fatalf("nil cache should yield zero config, got %+v", rc)
	}
}
