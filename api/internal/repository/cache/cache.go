package cache

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrRepoNotCloned is returned when no bare clone exists for a repository
// under the workspace base — i.e. the repo is not registered. Registration
// is manual (`git clone --bare` into WORKSPACE_BASE/{owner}/{repo}.git);
// elyoraa never clones on its own.
var ErrRepoNotCloned = errors.New("repository not cloned on server")

// fetchRefspecs are the refspecs every managed bare clone must fetch: heads
// (branch state) and PR head refs. git clone --bare persists no refspec of
// its own, and a `clone -c remote.origin.fetch=...` value becomes the ONLY
// refspec — without the heads refspec, `git fetch` refreshes PR refs but
// leaves every branch frozen at clone time.
var fetchRefspecs = []string{
	"+refs/heads/*:refs/heads/*",
	"+refs/pull/*/head:refs/remotes/origin/pr/*",
}

// Cache manages bare clones and per-review worktrees.
type Cache struct {
	base        string
	askpassPath string
	tokenDir    string
	gitHost     string // host the askpass credential helper may answer for
	mu          sync.Mutex
}

// GitHost extracts the hostname from a git base URL — the only host the
// GIT_ASKPASS helper is allowed to answer credential prompts for.
// Empty/unparseable/file:// bases yield "" (https prompts never arise).
func GitHost(gitBaseURL string) string {
	if gitBaseURL == "" {
		return "github.com"
	}
	if u, err := url.Parse(gitBaseURL); err == nil {
		return u.Hostname()
	}
	return ""
}

// New creates a cache rooted at base using the given GIT_ASKPASS script.
// gitBaseURL ("https://github.com") determines which host the askpass helper
// may answer credential prompts for; empty defaults to github.com. Repos are
// registered by manually cloning them into base — the cache never clones.
func New(base, askpassPath, tokenDir, gitBaseURL string) *Cache {
	if gitBaseURL == "" {
		gitBaseURL = "https://github.com"
	}
	return &Cache{
		base:        base,
		askpassPath: askpassPath,
		tokenDir:    tokenDir,
		gitHost:     GitHost(gitBaseURL),
	}
}

// IsCloned reports whether a bare clone exists for owner/repo.
func (c *Cache) IsCloned(owner, repo string) bool {
	info, err := os.Stat(c.barePath(owner, repo))
	return err == nil && info.IsDir()
}

// Fetch refreshes refs in the bare clone and ensures PR heads are available.
// The clone must already exist — the workspace base is the repo registry, so
// a missing clone means the repo was never registered (or was unregistered
// after the job enqueued). Any missing heads/PR-head fetch refspec is added
// before the fetch, so a plain manual `git clone --bare` is enough to
// register a repo.
func (c *Cache) Fetch(ctx context.Context, owner, repo, token string) error {
	if !c.IsCloned(owner, repo) {
		return ErrRepoNotCloned
	}
	bare := c.barePath(owner, repo)
	if err := c.ensureFetchRefspecs(ctx, bare); err != nil {
		return fmt.Errorf("configure fetch refspecs: %w", err)
	}

	tokenFile, cleanup, err := c.writeToken(token)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := c.updateLastReviewed(bare); err != nil {
		// non-fatal
	}

	// --update-head-ok: an agent may legitimately `git checkout <branch>` in
	// its worktree; without the flag that pins the shared ref and makes every
	// other job's fetch abort with "refusing to fetch into branch checked
	// out". The worst outcome here is one agent's own checkout going stale.
	cmd := exec.CommandContext(ctx, "git", "-C", bare, "fetch",
		"--prune", "--update-head-ok")
	cmd.Env = c.gitEnv(tokenFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w\n%s", err, out)
	}
	return nil
}

// AddWorktree creates a detached-HEAD worktree at the given SHA.
func (c *Cache) AddWorktree(ctx context.Context, owner, repo string, prNumber int, sha string) (string, func(), error) {
	if len(sha) >= 12 {
		sha = sha[:12]
	}
	nonce, err := randomNonce()
	if err != nil {
		return "", nil, fmt.Errorf("nonce: %w", err)
	}

	worktreeName := fmt.Sprintf("wt-%d-%s-%s", prNumber, sha, nonce)
	worktreePath := filepath.Join(c.worktreeBase(owner, repo), worktreeName)
	if err := os.MkdirAll(c.worktreeBase(owner, repo), 0o755); err != nil {
		return "", nil, fmt.Errorf("mkdir worktree base: %w", err)
	}

	bare := c.barePath(owner, repo)
	cmd := exec.CommandContext(ctx, "git", "-C", bare, "worktree", "add", worktreePath, sha)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", nil, fmt.Errorf("git worktree add: %w\n%s", err, out)
	}

	done := false
	cleanup := func() {
		if done {
			return
		}
		done = true
		c.removeWorktree(worktreePath, bare)
	}

	return worktreePath, cleanup, nil
}

// Sweep removes orphaned worktrees and interrupted clone temp dirs older
// than ttl.
func (c *Cache) Sweep(ctx context.Context, ttl time.Duration) error {
	return filepath.Walk(c.base, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() {
			return nil
		}
		if strings.HasSuffix(info.Name(), ".tmp-clone") {
			// A clone temp dir abandoned by a crash — older versions cloned
			// on demand into .tmp-clone dirs; sweep just bounds the window.
			if time.Since(info.ModTime()) >= ttl {
				_ = os.RemoveAll(path)
			}
			return nil
		}
		if strings.HasPrefix(info.Name(), "wt-") {
			if time.Since(info.ModTime()) < ttl {
				return nil
			}
			bare := filepath.Dir(filepath.Dir(path))
			if c.isActiveWorktree(bare, path) {
				return nil
			}
			c.removeWorktree(path, bare)
		}
		return nil
	})
}

// EvictLRU removes the least-recently-used bare clone if disk budget is exceeded.
// It is a best-effort safety valve; disk usage is estimated from the workspace
// base. Removing a clone unregisters the repo — under the manual registry
// model this stays off unless WORKSPACE_DISK_BUDGET_GB is set.
func (c *Cache) EvictLRU(ctx context.Context, budgetGB int) error {
	// Walk all bare clones and find the one with the oldest last-reviewed file
	// that has no active worktrees.
	var oldest string
	var oldestTime time.Time

	err := filepath.Walk(c.base, func(path string, info os.FileInfo, err error) error {
		if err != nil || !info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".git") {
			return nil
		}
		if c.hasActiveWorktrees(path) {
			return nil
		}
		ts, err := c.readLastReviewed(path)
		if err != nil {
			return nil
		}
		if oldest == "" || ts.Before(oldestTime) {
			oldest = path
			oldestTime = ts
		}
		return nil
	})
	if err != nil {
		return err
	}

	if oldest == "" {
		return nil
	}

	used, err := c.diskUsageGB(c.base)
	if err != nil {
		return err
	}
	if used < int64(budgetGB) {
		return nil
	}

	slog.Warn("disk budget exceeded, evicting least-recently-used clone — repo must be re-registered",
		"path", oldest, "last_reviewed", oldestTime, "used_gb", used, "budget_gb", budgetGB)
	return os.RemoveAll(oldest)
}

// DiskUsageGB returns the total disk usage of the workspace base in GB.
func (c *Cache) DiskUsageGB() (int64, error) {
	return c.diskUsageGB(c.base)
}

// ensureFetchRefspecs adds any missing fetch refspec to the bare clone's
// config. `git config --get-all` exits 1 when the key is absent — that is
// not an error here. Concurrent repairs on the same clone race on git's
// config.lock, so the check-and-add is serialized process-wide.
func (c *Cache) ensureFetchRefspecs(ctx context.Context, bare string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, _ := exec.CommandContext(ctx, "git", "-C", bare, "config",
		"--get-all", "remote.origin.fetch").Output()
	have := strings.Split(string(out), "\n")
	for _, want := range fetchRefspecs {
		found := false
		for _, h := range have {
			if strings.TrimSpace(h) == want {
				found = true
				break
			}
		}
		if found {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "-C", bare, "config",
			"--add", "remote.origin.fetch", want)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("add refspec %q: %w\n%s", want, err, out)
		}
	}
	return nil
}

// ReadRepoFile returns a file's contents from the bare clone's default branch
// (HEAD). Used for repo-level config like .elyoraa.yml — always the default
// branch, never a PR head, so untrusted pushes can't change their own
// treatment. Returns an error when the repo isn't cloned or the file is
// absent.
func (c *Cache) ReadRepoFile(ctx context.Context, owner, repo, name string) ([]byte, error) {
	if !c.IsCloned(owner, repo) {
		return nil, ErrRepoNotCloned
	}
	cmd := exec.CommandContext(ctx, "git", "-C", c.barePath(owner, repo),
		"show", "HEAD:"+name)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	return out, nil
}

func (c *Cache) removeWorktree(worktreePath, bare string) {
	_ = c.killProcessGroup(worktreePath)

	cmd := exec.Command("git", "-C", bare, "worktree", "remove", "--force", worktreePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		// If the git metadata is missing, just delete the directory.
		_ = os.RemoveAll(worktreePath)
		// Also remove worktree record from bare repo if it exists.
		_ = exec.Command("git", "-C", bare, "worktree", "prune").Run()
		_ = fmt.Errorf("worktree remove %s: %w\n%s", worktreePath, err, out)
	}
}

func (c *Cache) killProcessGroup(worktreePath string) error {
	// Find processes with this as working directory and kill the process group.
	// This is a best-effort Linux implementation.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", pid))
		if err != nil {
			continue
		}
		if cwd == worktreePath {
			pgid, err := syscall.Getpgid(pid)
			if err == nil {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
			}
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return nil
}

func (c *Cache) isActiveWorktree(bare, path string) bool {
	cmd := exec.Command("git", "-C", bare, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), path)
}

func (c *Cache) hasActiveWorktrees(bare string) bool {
	cmd := exec.Command("git", "-C", bare, "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	// Each worktree block starts with "worktree <path>". Count beyond the bare repo.
	count := strings.Count(string(out), "\nworktree ")
	return count > 1
}

// WriteTokenFile persists a GitHub installation token in the token dir for
// use as a git credential. Callers must invoke the returned cleanup when the
// token is no longer needed. The dir is deny-listed for agent reads, so the
// file stays hidden inside the Devin sandbox while the excluded askpass
// helper can still consume it.
func (c *Cache) WriteTokenFile(token string) (string, func(), error) {
	return c.writeToken(token)
}

func (c *Cache) writeToken(token string) (string, func(), error) {
	if err := os.MkdirAll(c.tokenDir, 0o700); err != nil {
		return "", nil, err
	}
	f, err := os.CreateTemp(c.tokenDir, "github-token-*")
	if err != nil {
		return "", nil, err
	}
	if _, err := io.WriteString(f, token); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	cleanup := func() { _ = os.Remove(f.Name()) }
	return f.Name(), cleanup, nil
}

func (c *Cache) gitEnv(tokenFile string) []string {
	env := os.Environ()
	env = append(env, "GIT_ASKPASS="+c.askpassPath)
	env = append(env, "GIT_TERMINAL_PROMPT=0")
	env = append(env, "ELYORAA_GIT_TOKEN_FILE="+tokenFile)
	env = append(env, "ELYORAA_GIT_HOST="+c.gitHost)
	return env
}

func (c *Cache) barePath(owner, repo string) string {
	return filepath.Join(c.base, owner, repo+".git")
}

func (c *Cache) repoBase(owner string) string {
	return filepath.Join(c.base, owner)
}

func (c *Cache) worktreeBase(owner, repo string) string {
	return filepath.Join(c.base, owner, repo)
}

func (c *Cache) updateLastReviewed(bare string) error {
	f := filepath.Join(bare, "elyoraa-last-reviewed")
	return os.WriteFile(f, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o644)
}

func (c *Cache) readLastReviewed(bare string) (time.Time, error) {
	f := filepath.Join(bare, "elyoraa-last-reviewed")
	b, err := os.ReadFile(f)
	if err != nil {
		// If missing, use directory mod time.
		info, err := os.Stat(bare)
		if err != nil {
			return time.Time{}, err
		}
		return info.ModTime(), nil
	}
	n, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(n, 0), nil
}

func (c *Cache) diskUsageGB(path string) (int64, error) {
	var total int64
	err := filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total / (1024 * 1024 * 1024), nil
}

func randomNonce() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}
