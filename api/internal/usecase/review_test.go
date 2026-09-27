package usecase

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/service/github"
)

func exitErr() error {
	// Simulate a non-zero exit error.
	return &exec.ExitError{ProcessState: nil}
}

func TestClassifyDevinError(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name      string
		err       error
		out       string
		transient bool
	}{
		{"empty output", exitErr(), "", true},
		{"whitespace output", exitErr(), "   \n", true},
		{"connection refused", exitErr(), "dial tcp: connection refused", true},
		{"unknown model", exitErr(), "Error: Unknown model swe-2-high", true},
		{"empty model list", exitErr(), "no models available", true},
		{"rate limit", exitErr(), "429 too many requests", true},
		{"untrusted workspace", exitErr(), "fatal: untrusted workspace", false},
		{"unauthorized", exitErr(), "401 Unauthorized", false},
		{"bad credentials", exitErr(), "authentication failed: bad credentials", false},
		{"permission denied", exitErr(), "permission denied writing config", false},
		{"actionable output", exitErr(), "panic: nil pointer dereference in reviewer", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr, reason := classifyDevinError(tc.err, ctx, tc.out)
			if tr != tc.transient {
				t.Fatalf("transient = %v, want %v (reason=%q)", tr, tc.transient, reason)
			}
		})
	}
}

func TestClassifyDevinErrorContexts(t *testing.T) {
	// Deadline exceeded → transient.
	dl, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	tr, _ := classifyDevinError(fmt.Errorf("killed"), dl, "")
	if !tr {
		t.Fatal("deadline exceeded should be transient")
	}

	// Canceled → not transient.
	cc, cancel2 := context.WithCancel(context.Background())
	cancel2()
	tr, _ = classifyDevinError(fmt.Errorf("killed"), cc, "")
	if tr {
		t.Fatal("canceled should not be transient")
	}
}

func TestClassifyDevinErrorBinaryMissing(t *testing.T) {
	_, err := exec.LookPath("definitely-not-a-real-binary-xyz")
	if err == nil {
		t.Skip("binary unexpectedly present")
	}
	tr, reason := classifyDevinError(err, context.Background(), "")
	if tr {
		t.Fatalf("missing binary should not be transient, reason=%q", reason)
	}
}

func TestTailString(t *testing.T) {
	s := "short"
	if got := tailString(s, 8192); got != s {
		t.Fatalf("tailString = %q, want %q", got, s)
	}

	long := strings.Repeat("x", 10000) + "\nlastline"
	got := tailString(long, 100)
	if !strings.Contains(got, "lastline") || !strings.HasPrefix(got, "…[truncated") {
		t.Fatalf("tailString = %q", got)
	}
}

func TestRedactArgs(t *testing.T) {
	got := redactArgs("devin", []string{
		"-p",
		"--token", "ghp_secretvalue123",
		"--model", "swe-2-high",
		"--api-key=sk-abcdefgh1234",
		"--prompt-file", "/tmp/x/prompt.md",
	})
	if strings.Contains(got, "ghp_secretvalue123") || strings.Contains(got, "sk-abcdefgh1234") {
		t.Fatalf("secrets leaked in argv: %s", got)
	}
	if !strings.Contains(got, "swe-2-high") || !strings.Contains(got, "/tmp/x/prompt.md") {
		t.Fatalf("non-secret args mangled: %s", got)
	}
}

func TestSleepCtx(t *testing.T) {
	if !sleepCtx(context.Background(), time.Millisecond) {
		t.Fatal("sleepCtx should return true when timer fires")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) {
		t.Fatal("sleepCtx should return false when ctx is done")
	}
}

func TestClassifyDevinErrorWrapped(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", errors.New("some exit error"))
	tr, _ := classifyDevinError(err, context.Background(), "")
	if !tr {
		t.Fatal("empty output should be transient regardless of error type")
	}
}

func TestParseIssueActions(t *testing.T) {
	// Reply with a trailing action block.
	out := "Here is the breakdown you approved.\n\n```json\n{\"create_issues\": [{\"title\": \"Split queue\", \"body\": \"Do it\", \"labels\": [\"refactor\"]}]}\n```\n"
	acts, cleaned := parseIssueActions(out)
	if len(acts.CreateIssues) != 1 || acts.CreateIssues[0].Title != "Split queue" {
		t.Fatalf("unexpected actions: %+v", acts)
	}
	if strings.Contains(cleaned, "```json") || !strings.Contains(cleaned, "breakdown you approved") {
		t.Fatalf("bad cleaned output: %q", cleaned)
	}

	// Reply without an action block stays untouched.
	out = "Just analysis, no actions."
	acts, cleaned = parseIssueActions(out)
	if len(acts.CreateIssues) != 0 || cleaned != out {
		t.Fatalf("expected passthrough, got %+v %q", acts, cleaned)
	}

	// A non-action json block (no create_issues) is left visible.
	out = "Example:\n\n```json\n{\"foo\": 1}\n```\n"
	acts, cleaned = parseIssueActions(out)
	if len(acts.CreateIssues) != 0 || cleaned != out {
		t.Fatalf("non-action block should pass through, got %+v %q", acts, cleaned)
	}
}

func TestRenderIssuePrompt(t *testing.T) {
	r := &ReviewUsecase{cfg: &config.Config{PromptsDir: "../../prompts", BotHandle: "elyoraa"}}
	j := &domain.Review{
		Owner:          "o",
		Repo:           "r",
		PRNumber:       7,
		Title:          "Big refactor request",
		Author:         "alice",
		Body:           "Please rework the whole queue system",
		TriggerComment: "@elyoraa what do you think?",
		RepoAvailable:  true,
	}
	thread := []github.IssueComment{
		{Author: "alice", Body: "@elyoraa what do you think?", CreatedAt: time.Now()},
		{Author: "elyoraa[bot]", Body: "This is too big; proposed breakdown…", CreatedAt: time.Now(), IsBot: true},
		{Author: "alice", Body: "go ahead", CreatedAt: time.Now()},
	}

	out, err := r.renderIssuePrompt(j, thread)
	if err != nil {
		t.Fatalf("renderIssuePrompt: %v", err)
	}
	for _, want := range []string{"Big refactor request", "go ahead", "elyoraa[bot]", "breakdown", "Discussion Thread"} {
		if !strings.Contains(out, want) {
			t.Fatalf("prompt missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "<no value>") {
		t.Fatalf("prompt has unresolved template keys:\n%s", out)
	}
}

func TestRenderReconcilePrompt(t *testing.T) {
	r := &ReviewUsecase{cfg: &config.Config{PromptsDir: "../../prompts"}}
	j := &domain.Review{Owner: "o", Repo: "r", PRNumber: 3, Title: "t", Author: "alice", HeadSHA: "abc"}
	prev := &github.PreviousReview{
		State: "CHANGES_REQUESTED",
		Body:  "Found 2 issues",
		Findings: []github.PriorFinding{
			{Path: "a.go", Line: 10, Body: "nil dereference", Outdated: true,
				Replies: []github.PriorReply{{Author: "alice", Body: "fixed in next push"}}},
		},
	}
	review := &domain.Output{Comments: []domain.Comment{{Path: "b.go", Line: 5, Severity: "medium", Body: "swallowed error"}}}

	out, err := r.renderReconcilePrompt(j, prev, review)
	if err != nil {
		t.Fatalf("renderReconcilePrompt: %v", err)
	}
	for _, want := range []string{"nil dereference", "outdated", "fixed in next push", "swallowed error", "prior_findings_status", "additional_comments"} {
		if !strings.Contains(out, want) {
			t.Fatalf("reconcile prompt missing %q", want)
		}
	}
	if strings.Contains(out, "<no value>") {
		t.Fatalf("prompt has unresolved template keys")
	}
}

func TestParseReconcileOutput(t *testing.T) {
	out := "Analysis done.\n\n```json\n{\"prior_findings_status\": [{\"path\": \"a.go\", \"line\": 10, \"status\": \"still_present\"}], \"additional_comments\": [{\"path\": \"a.go\", \"line\": 12, \"severity\": \"high\", \"body\": \"still broken\"}]}\n```\n"
	rec, err := parseReconcileOutput(out)
	if err != nil {
		t.Fatalf("parseReconcileOutput: %v", err)
	}
	if len(rec.PriorFindings) != 1 || rec.PriorFindings[0].Status != "still_present" {
		t.Fatalf("bad prior findings: %+v", rec.PriorFindings)
	}
	if len(rec.AdditionalComments) != 1 || rec.AdditionalComments[0].Line != 12 {
		t.Fatalf("bad additional comments: %+v", rec.AdditionalComments)
	}
}
