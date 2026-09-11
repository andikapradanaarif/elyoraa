package usecase

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
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
