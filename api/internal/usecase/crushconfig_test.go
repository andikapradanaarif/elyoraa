package usecase

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/config"
	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

func TestCrushRepositoryBoundary(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{"code.go": "package example\n", ".env": "SECRET=private"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, ".env"), filepath.Join(dir, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	repo, err := startCrushRepo(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, path := range []string{".env", "../secret", "/etc/passwd", "alias", "outside/key", ".git/config"} {
		if out, err := repo.read(path, 0); err == nil {
			t.Fatalf("read %q succeeded: %s", path, out)
		}
	}
	if out, err := repo.read("code.go", 0); err != nil || !strings.Contains(out, "package example") {
		t.Fatalf("read code: %q, %v", out, err)
	}
	for _, name := range []string{"list_files", "search"} {
		out, err := repo.call(context.Background(), name, json.RawMessage(`{"pattern":"."}`))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "private") || strings.Contains(out, ".env") || strings.Contains(out, "alias") {
			t.Fatalf("%s exposed protected files: %s", name, out)
		}
	}
	for _, args := range [][]string{{"push"}, {"config", "--list"}, {"show", "--output=/tmp/leak"}, {"diff", "--ext-diff"}, {"show", "HEAD:.env"}, {"show", "HEAD:../secret"}, {"-c", "alias.x=!evil"}} {
		if out, err := repo.git(context.Background(), args); err == nil {
			t.Fatalf("unsafe git %v succeeded: %s", args, out)
		}
	}
}

func TestCrushRepositoryGitAndMCP(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "-b", "main")
	runGit(t, dir, "config", "user.name", "test")
	runGit(t, dir, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(dir, "code.txt"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "code.txt")
	runGit(t, dir, "commit", "-m", "initial")
	repo, err := startCrushRepo(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	for _, args := range [][]string{{"show", "HEAD:code.txt"}, {"log", "--oneline", "-n1"}, {"diff", "HEAD", "--", "code.txt"}, {"status", "--short"}} {
		if out, err := repo.git(context.Background(), args); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	client := &http.Client{Timeout: time.Second}
	request := func(token, body string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, repo.url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, _ := io.ReadAll(res.Body)
		return res, string(b)
	}
	if res, _ := request("wrong", `{"id":1,"method":"tools/list"}`); res.StatusCode != 401 {
		t.Fatalf("unauthenticated status %d", res.StatusCode)
	}
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"code.txt"}}}`,
	} {
		res, out := request(repo.token, body)
		if res.StatusCode != 200 || !strings.Contains(out, `"result"`) {
			t.Fatalf("MCP response %d: %s", res.StatusCode, out)
		}
	}
}

func TestRunCrushContractAndRetries(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "crush")
	// Check argv, prompt input, configuration isolation and separate attempt
	// state. Stderr contains a decoy action block that must not reach callers.
	script := `#!/bin/sh
set -eu
if [ "$1" = --version ]; then echo crush-test; exit 0; fi
[ "$1" = run ] && [ "$2" = --quiet ]
[ "$3" = --model ] && [ "$4" = provider/model ]
[ "$PWD" != "$REPO_DIR" ]
[ "$CRUSH_GLOBAL_CONFIG" = "$EXPECTED_CONFIG" ]
[ "$CRUSH_GLOBAL_DATA" = "$EXPECTED_DATA" ]
[ "$CRUSH_CLIENT_SERVER" = false ]
[ -f "$PWD/crush.json" ]
grep -q 'test prompt' || exit 1
if [ ! -f "$ATTEMPT_MARKER" ]; then touch "$ATTEMPT_MARKER"; echo '503 overloaded' >&2; exit 1; fi
echo 'decoy stderr' >&2
printf 'agent output'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	t.Setenv("REPO_DIR", worktree)
	t.Setenv("EXPECTED_CONFIG", dir)
	t.Setenv("EXPECTED_DATA", dir)
	t.Setenv("ATTEMPT_MARKER", filepath.Join(dir, "attempted"))
	r := &ReviewUsecase{cfg: &config.Config{CrushBin: bin, CrushConfigDir: dir, CrushDataDir: dir, CrushModel: "provider/model", CrushMaxRetries: 1, CrushPreflightCheck: true, CrushRetryBackoffs: []time.Duration{time.Millisecond}, ReviewTimeout: time.Second}}
	out, attempts, _, err := r.runCrush(context.Background(), &domain.Review{}, worktree, "test prompt", nil)
	if err != nil || out != "agent output" || attempts != 2 {
		t.Fatalf("run: %q, attempts %d, error %v", out, attempts, err)
	}
	// An empty model delegates selection to the user's Crush settings.
	r.cfg.CrushModel = ""
	if got := r.jobModel(&domain.Review{}); got != "" {
		t.Fatalf("default model: %q", got)
	}
}

func TestCrushPolicy(t *testing.T) {
	dir := t.TempDir()
	repo, err := startCrushRepo(context.Background(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	if err := writeCrushConfig(dir, repo); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "crush.json"))
	if err != nil {
		t.Fatal(err)
	}
	var policy struct {
		Options struct {
			DisabledTools []string `json:"disabled_tools"`
		} `json:"options"`
		Hooks map[string][]struct {
			Command string `json:"command"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &policy); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bash", "view", "edit", "fetch", "agent", "map", "recall"} {
		if !strings.Contains(strings.Join(policy.Options.DisabledTools, ","), name) {
			t.Fatalf("native tool %s enabled", name)
		}
	}
	for _, tool := range []string{"bash", "new_future_tool", "mcp_github_push", "mcp_elyoraa_read_file", "mcp_elyoraa_git"} {
		cmd := exec.Command("sh", "-c", policy.Hooks["PreToolUse"][0].Command)
		cmd.Env = append(os.Environ(), "CRUSH_TOOL_NAME="+tool)
		err := cmd.Run()
		if strings.HasPrefix(tool, "mcp_elyoraa_") != (err == nil) {
			t.Fatalf("tool guard %s: %v", tool, err)
		}
	}
}

// Opt-in smoke test against the installed fork and real provider configuration.
// Standard tests remain offline and never consume model API credits.
func TestInstalledCrush(t *testing.T) {
	if os.Getenv("ELYORAA_LIVE_CRUSH_TEST") != "1" {
		t.Skip("set ELYORAA_LIVE_CRUSH_TEST=1 for the installed-fork smoke test")
	}
	bin, err := exec.LookPath("crush")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	const probe = "elyoraa-crush-tool-smoke-7d914f6e"
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), []byte(probe), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CrushBin: bin, CrushConfigDir: filepath.Join(home, ".config", "crush"), CrushDataDir: filepath.Join(home, ".local", "share", "crush"), ReviewTimeout: 90 * time.Second}
	r := &ReviewUsecase{cfg: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, _, _, err := r.runCrush(ctx, &domain.Review{}, dir, "Call mcp_elyoraa_read_file for probe.txt, then reply with only the exact file contents. Do not use other tools.", nil)
	if err != nil {
		t.Fatalf("installed Crush failed: %v (%s)", err, tailString(out, 2000))
	}
	if !strings.Contains(out, probe) {
		t.Fatalf("installed Crush did not read the probe: %q", out)
	}
}

// runGit runs a git command that must succeed.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// TestGitAskpass_HostLock runs the deployed askpass script against credential
// prompts for allowed and hostile hosts.
func TestGitAskpass_HostLock(t *testing.T) {
	script := filepath.Join("..", "..", "deploy", "git-askpass.sh")
	if _, err := os.Stat(script); err != nil {
		t.Skip("deploy/git-askpass.sh not found")
	}
	tokenFile := filepath.Join(t.TempDir(), "tok")
	// WriteTokenFile stores the raw token with no trailing newline; the
	// askpass emits it via cat, so expected output has no newline either.
	if err := os.WriteFile(tokenFile, []byte("tok-value"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		prompt  string
		want    string
		wantErr bool
	}{
		{"username", "Username for 'https://github.com/o/r.git':", "x-access-token\n", false},
		{"password no userinfo", "Password for 'https://github.com/o/r.git':", "tok-value", false},
		{"password with userinfo", "Password for 'https://x-access-token@github.com/o/r.git':", "tok-value", false},
		{"port", "Password for 'https://github.com:8443/o/r.git':", "tok-value", false},
		{"other host", "Password for 'https://evil.example/o/r.git':", "", true},
		{"allowed host in path", "Password for 'https://evil.example/x?://github.com/':", "", true},
		{"allowed host in path 2", "Password for 'https://evil.example/@github.com/x':", "", true},
		{"host suffix", "Password for 'https://github.com.evil.example/x':", "", true},
		{"userinfo smuggle", "Password for 'https://x-access-token@github.com@evil.example/x':", "", true},
		{"no url", "Password:", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", script, tc.prompt)
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"ELYORAA_GIT_TOKEN_FILE=" + tokenFile,
				"ELYORAA_GIT_HOST=github.com",
			}
			out, err := cmd.Output()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("prompt %q answered with credentials: %q", tc.prompt, out)
				}
				return
			}
			if err != nil {
				t.Fatalf("prompt %q refused: %v", tc.prompt, err)
			}
			if string(out) != tc.want {
				t.Fatalf("prompt %q: got %q, want %q", tc.prompt, out, tc.want)
			}
		})
	}

	// Without ELYORAA_GIT_HOST the helper must fail closed.
	cmd := exec.Command("sh", script, "Password for 'https://github.com/o/r.git':")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "ELYORAA_GIT_TOKEN_FILE=" + tokenFile}
	if out, err := cmd.Output(); err == nil {
		t.Fatalf("askpass answered without ELYORAA_GIT_HOST: %q", out)
	}
}
