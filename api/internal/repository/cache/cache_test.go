package cache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitRun runs a git command that must succeed.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// gitRunQuiet runs a git command without failing the test, for use from
// goroutines (t.Fatalf may only be called from the test goroutine).
func gitRunQuiet(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return nil
}

// gitSHA resolves a ref to a commit SHA in dir.
func gitSHA(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}

// seedBareRepo builds a small source repo with one commit (containing
// .elyoraa.yml), then exposes it as a bare clone under
// <seedDir>/<owner>/<repo>.git so the cache can clone it via file://.
// The source repo's origin remote points at the published bare clone so
// tests can push follow-up commits.
func seedBareRepo(t *testing.T, owner, repo string) (seedDir, src, headSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	seedDir = t.TempDir()
	src = filepath.Join(seedDir, "src")

	gitRun(t, seedDir, "init", "-b", "main", "src")
	gitRun(t, src, "config", "user.email", "test@example.com")
	gitRun(t, src, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(src, ".elyoraa.yml"),
		[]byte("enabled: true\nbudget_minutes: 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-m", "init")
	headSHA = gitSHA(t, src, "HEAD")

	// Publish as <seedDir>/<owner>/<repo>.git — the cache joins
	// gitBaseURL/owner/repo.git, so the file:// layout must match.
	if err := os.MkdirAll(filepath.Join(seedDir, owner), 0o755); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(seedDir, owner, repo+".git")
	gitRun(t, seedDir, "clone", "--bare", src, origin)
	gitRun(t, src, "remote", "add", "origin", origin)
	return seedDir, src, headSHA
}

// preClone registers a repo the way an operator does: a manual
// `git clone --bare` into the cache base. A plain clone writes no fetch
// refspec, so tests exercising it also cover Fetch's refspec self-repair.
func preClone(t *testing.T, c *Cache, seedDir, owner, repo string) {
	t.Helper()
	bare := c.barePath(owner, repo)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seedDir, "clone", "--bare",
		"file://"+filepath.Join(seedDir, owner, repo+".git"), bare)
}

// pushSeedChange commits a new file on main in src and pushes it to the
// published bare origin, returning the new head SHA.
func pushSeedChange(t *testing.T, src, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(src, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, src, "add", ".")
	gitRun(t, src, "commit", "-m", "add "+name)
	gitRun(t, src, "push", "origin", "main")
	return gitSHA(t, src, "HEAD")
}

func TestFetch_RegisteredRepo(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _, _ := seedBareRepo(t, owner, repo)
	base := t.TempDir()
	c := New(base, "", t.TempDir(), "file://"+seedDir)

	// Unregistered repo: Fetch must refuse — the cache never clones.
	if err := c.Fetch(context.Background(), owner, repo, "tok"); !errors.Is(err, ErrRepoNotCloned) {
		t.Fatalf("Fetch on unregistered repo = %v, want ErrRepoNotCloned", err)
	}

	// Registration is a plain manual `git clone --bare`, which writes no
	// fetch refspec — Fetch must repair it so branches AND PR heads stay
	// fetchable.
	preClone(t, c, seedDir, owner, repo)
	if err := c.Fetch(context.Background(), owner, repo, "tok"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	out, err := exec.Command("git", "-C", c.barePath(owner, repo), "config", "--get-all", "remote.origin.fetch").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range fetchRefspecs {
		if !strings.Contains(string(out), want) {
			t.Fatalf("clone missing refspec %q, got: %s", want, out)
		}
	}
}

// TestFetch_RefreshesBranches is the staleness regression: a clone made once
// must see branch updates pushed later — `git fetch` previously refreshed
// only refs/pull/*, leaving refs/heads/* frozen at clone time.
func TestFetch_RefreshesBranches(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, src, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)
	ctx := context.Background()

	preClone(t, c, seedDir, owner, repo)
	bare := c.barePath(owner, repo)
	before := gitSHA(t, bare, "refs/heads/main")

	newSHA := pushSeedChange(t, src, "new.txt", "v2\n")
	if newSHA == before {
		t.Fatal("seed push did not move main")
	}
	if err := c.Fetch(ctx, owner, repo, "tok"); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if got := gitSHA(t, bare, "refs/heads/main"); got != newSHA {
		t.Fatalf("refs/heads/main not refreshed: got %s, want %s", got, newSHA)
	}
}

// TestFetch_RepairsLegacyRefspec covers clones created before the heads
// refspec was configured: the clone has only the PR refspec, and Fetch must
// repair it so branch state refreshes again.
func TestFetch_RepairsLegacyRefspec(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, src, _ := seedBareRepo(t, owner, repo)
	base := t.TempDir()
	c := New(base, "", t.TempDir(), "file://"+seedDir)

	// Simulate the old clone layout: a bare clone whose only fetch refspec
	// covers PR heads.
	bare := c.barePath(owner, repo)
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	gitRun(t, base, "clone", "--bare", "file://"+filepath.Join(seedDir, owner, repo+".git"), bare)
	gitRun(t, bare, "config", "--add", "remote.origin.fetch", "+refs/pull/*/head:refs/remotes/origin/pr/*")

	newSHA := pushSeedChange(t, src, "later.txt", "v3\n")
	if err := c.Fetch(context.Background(), owner, repo, "tok"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := gitSHA(t, bare, "refs/heads/main"); got != newSHA {
		t.Fatalf("legacy clone not repaired: refs/heads/main=%s, want %s", got, newSHA)
	}
}

func TestFetch_UnregisteredRepo(t *testing.T) {
	c := New(t.TempDir(), "", t.TempDir(), "file://"+t.TempDir())

	if err := c.Fetch(context.Background(), "o", "missing", "tok"); !errors.Is(err, ErrRepoNotCloned) {
		t.Fatalf("Fetch = %v, want ErrRepoNotCloned", err)
	}
	if c.IsCloned("o", "missing") {
		t.Fatal("unregistered fetch must not create a clone")
	}
}

func TestFetch_ConcurrentFetches(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)
	preClone(t, c, seedDir, owner, repo)

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Fetch(context.Background(), owner, repo, "tok")
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Fetch: %v", err)
		}
	}
}

// TestFetch_ConcurrentFetchesWhileRemoteAdvances is the ref-race regression:
// a fetch that starts before another writer moves a branch aborts with
// "cannot lock ref ... is at X but expected Y", which used to fail the whole
// review job. Every fetch must succeed and the clone must end up current.
func TestFetch_ConcurrentFetchesWhileRemoteAdvances(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, src, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)
	preClone(t, c, seedDir, owner, repo)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 5; i++ {
			if err := os.WriteFile(filepath.Join(src, fmt.Sprintf("f%d.txt", i)), []byte("x\n"), 0o644); err != nil {
				return
			}
			gitRunQuiet(src, "add", ".")
			gitRunQuiet(src, "commit", "-m", "advance")
			gitRunQuiet(src, "push", "origin", "main")
			time.Sleep(10 * time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- c.Fetch(context.Background(), owner, repo, "tok")
		}()
	}
	wg.Wait()
	<-done
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Fetch during advance: %v", err)
		}
	}

	if err := c.Fetch(context.Background(), owner, repo, "tok"); err != nil {
		t.Fatalf("final Fetch: %v", err)
	}
	bare := c.barePath(owner, repo)
	if got, want := gitSHA(t, bare, "refs/heads/main"), gitSHA(t, src, "refs/heads/main"); got != want {
		t.Fatalf("refs/heads/main = %s, want %s", got, want)
	}
}

// TestFetch_SerializesPerClone proves the per-clone fetch lock: two fetches
// for the same repo never overlap, two repos still fetch in parallel.
func TestFetch_SerializesPerClone(t *testing.T) {
	c := New(t.TempDir(), "", t.TempDir(), "")

	var inFlight, maxInFlight int32
	var mu sync.Mutex
	enter := func() func() {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		return func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock := c.lockFetch("same")
			defer enter()()
			time.Sleep(20 * time.Millisecond)
			unlock()
		}()
	}
	wg.Wait()
	if maxInFlight != 1 {
		t.Fatalf("max concurrent holders of one clone's fetch lock = %d, want 1", maxInFlight)
	}

	if a, b := c.lockFetch("a"), c.lockFetch("b"); a != nil && b != nil {
		a()
		b()
	} else {
		t.Fatal("locks for distinct clones must both be acquirable")
	}
}

func TestIsFetchRace(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "ref value moved under us",
			out: "error: cannot lock ref 'refs/heads/feat/x': is at 8b6cf4190000 " +
				"but expected 8abce5ad0000\n",
			want: true,
		},
		{
			name: "packed refs lock held",
			out:  "fatal: unable to create '/ws/o/r.git/packed-refs.lock': File exists.\n",
			want: true,
		},
		{
			name: "authentication failure",
			out:  "fatal: Authentication failed for 'https://github.com/o/r.git/'\n",
			want: false,
		},
		{
			name: "unknown ref",
			out:  "fatal: couldn't find remote ref refs/pull/1/head\n",
			want: false,
		},
	}
	for _, tc := range cases {
		if got := isFetchRace([]byte(tc.out)); got != tc.want {
			t.Fatalf("%s: isFetchRace = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadRepoFile_DefaultBranch(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)
	preClone(t, c, seedDir, owner, repo)
	data, err := c.ReadRepoFile(context.Background(), owner, repo, ".elyoraa.yml")
	if err != nil {
		t.Fatalf("ReadRepoFile: %v", err)
	}
	if !strings.Contains(string(data), "budget_minutes: 42") {
		t.Fatalf("unexpected file content: %s", data)
	}
	if _, err := c.ReadRepoFile(context.Background(), owner, repo, "nonexistent.txt"); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestSweep_RemovesStaleTmpClone(t *testing.T) {
	c := New(t.TempDir(), "", t.TempDir(), "")
	stale := filepath.Join(c.repoBase("o"), "r.git.tmp-clone")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make it look old.
	old := time.Now().Add(-48 * time.Hour)
	_ = os.Chtimes(stale, old, old)

	if err := c.Sweep(context.Background(), time.Hour); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale tmp-clone dir not removed")
	}
}
