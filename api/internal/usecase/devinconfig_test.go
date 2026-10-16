package usecase

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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

func gitRevParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

func TestWriteDevinConfig(t *testing.T) {
	dir, tokenDir := t.TempDir(), t.TempDir()
	if err := WriteDevinConfig(dir, tokenDir); err != nil {
		t.Fatalf("WriteDevinConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "devin-config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
		Sandbox struct {
			Excluded struct {
				Allow []string `json:"allow"`
			} `json:"excluded"`
		} `json:"sandbox"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config JSON: %v", err)
	}

	syncRule := "Exec(" + filepath.Join(dir, syncScriptName) + ")"
	has := func(list []string, want string) bool {
		for _, s := range list {
			if s == want {
				return true
			}
		}
		return false
	}
	if !has(cfg.Permissions.Allow, syncRule) {
		t.Fatal("sync script missing from allow list")
	}
	if !has(cfg.Sandbox.Excluded.Allow, syncRule) {
		t.Fatal("sync script missing from sandbox exclusions — it must run outside the sandbox to reach credentials")
	}
	for _, want := range []string{"Exec(git checkout)", "Exec(git reset)", "Exec(git worktree)"} {
		if !has(cfg.Sandbox.Excluded.Allow, want) {
			t.Fatalf("sandbox exclusions missing %q — worktree git metadata lives outside the writable root", want)
		}
	}
	for _, want := range []string{"Exec(git push)", "Exec(git credential)", "Exec(git config)", "Exec(git checkout -b)"} {
		if !has(cfg.Permissions.Deny, want) {
			t.Fatalf("deny list missing %q", want)
		}
	}
	if !has(cfg.Permissions.Deny, "Read("+tokenDir+"/**)") {
		t.Fatal("token dir read deny missing")
	}

	// The generated helper must exist, be executable, and fetch with a
	// scrubbed environment + fixed argv.
	info, err := os.Stat(filepath.Join(dir, syncScriptName))
	if err != nil {
		t.Fatalf("sync script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatal("sync script is not executable")
	}
	body, _ := os.ReadFile(filepath.Join(dir, syncScriptName))
	for _, want := range []string{"env -i", "fetch --prune origin", "ELYORAA_GIT_TOKEN_FILE", "ELYORAA_GIT_HOST", "ELYORAA_WORKSPACE_BASE"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("sync script missing %q", want)
		}
	}
}

// TestRepoSync_EndToEnd runs the generated helper against a file:// remote:
// no credentials are needed, but the env contract is the same as production.
func TestRepoSync_EndToEnd(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	tmp := t.TempDir()

	// Source repo + published bare origin.
	src := filepath.Join(tmp, "src")
	runGit(t, tmp, "init", "-b", "main", src)
	runGit(t, src, "config", "user.email", "test@example.com")
	runGit(t, src, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "init")
	origin := filepath.Join(tmp, "origin.git")
	runGit(t, tmp, "clone", "--bare", src, origin)
	runGit(t, src, "remote", "add", "origin", origin)

	// Workspace clone in the managed layout + worktree (mirrors the cache).
	base := filepath.Join(tmp, "workspace")
	bare := filepath.Join(base, "o", "r.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, tmp, "clone", "--bare", "file://"+origin, bare)
	runGit(t, bare, "config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/heads/*")
	runGit(t, bare, "config", "--add", "remote.origin.fetch", "+refs/pull/*/head:refs/remotes/origin/pr/*")
	wt := filepath.Join(base, "o", "r", "wt-1-test")
	// Worktrees are added by SHA (detached HEAD), mirroring Cache.AddWorktree.
	runGit(t, bare, "worktree", "add", wt, gitRevParse(t, bare, "refs/heads/main"))

	cfgDir := t.TempDir()
	if err := WriteDevinConfig(cfgDir, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(cfgDir, syncScriptName)

	syncEnv := []string{
		"ELYORAA_WORKSPACE_BASE=" + base,
		"ELYORAA_GIT_HOST=github.com",
		"ELYORAA_GIT_TOKEN_FILE=" + filepath.Join(t.TempDir(), "tok"),
		"GIT_ASKPASS=/nonexistent", // file:// never invokes it
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + tmp,
	}

	// A commit lands on origin after the clone was made.
	if err := os.WriteFile(filepath.Join(src, "b.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "v2")
	runGit(t, src, "push", "origin", "main")
	newSHA := gitRevParse(t, src, "HEAD")
	if gitRevParse(t, bare, "refs/heads/main") == newSHA {
		t.Fatal("bare clone already up to date — test premise broken")
	}

	// Run the helper from inside the worktree (the agent's cwd).
	cmd := exec.Command(script)
	cmd.Dir = wt
	cmd.Env = syncEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repo-sync failed: %v\n%s", err, out)
	}
	if got := gitRevParse(t, bare, "refs/heads/main"); got != newSHA {
		t.Fatalf("repo-sync did not refresh heads: got %s, want %s", got, newSHA)
	}

	// From a directory outside the workspace the helper must refuse.
	cmd = exec.Command(script)
	cmd.Dir = tmp
	cmd.Env = syncEnv
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("repo-sync accepted a directory outside the workspace: %s", out)
	}

	// A nested repo made inside the worktree is not a managed clone.
	nested := filepath.Join(wt, "nested")
	runGit(t, wt, "init", nested)
	cmd = exec.Command(script, nested)
	cmd.Dir = wt
	cmd.Env = syncEnv
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("repo-sync accepted a nested non-managed repo: %s", out)
	}

	// A branch checked out in a linked worktree (as an agent may do) must not
	// abort the fetch — --update-head-ok keeps the sync working.
	wt2 := filepath.Join(base, "o", "r", "wt-2-branch")
	runGit(t, bare, "worktree", "add", wt2, "main")
	if err := os.WriteFile(filepath.Join(src, "c.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, src, "add", ".")
	runGit(t, src, "commit", "-m", "v3")
	runGit(t, src, "push", "origin", "main")
	newSHA = gitRevParse(t, src, "HEAD")

	cmd = exec.Command(script)
	cmd.Dir = wt
	cmd.Env = syncEnv
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repo-sync failed with a branch checked out: %v\n%s", err, out)
	}
	if got := gitRevParse(t, bare, "refs/heads/main"); got != newSHA {
		t.Fatalf("heads not refreshed despite checked-out branch: got %s, want %s", got, newSHA)
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
