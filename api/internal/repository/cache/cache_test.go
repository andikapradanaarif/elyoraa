package cache

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// seedBareRepo builds a small source repo with one commit (containing
// .elyoraa.yml), then exposes it as a bare clone under
// <seedDir>/<owner>/<repo>.git so the cache can clone it via file://.
func seedBareRepo(t *testing.T, owner, repo string) (seedDir, headSHA string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	seedDir = t.TempDir()
	src := filepath.Join(seedDir, "src")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run(seedDir, "init", "-b", "main", "src")
	run(src, "config", "user.email", "test@example.com")
	run(src, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(src, ".elyoraa.yml"),
		[]byte("enabled: true\nbudget_minutes: 42\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(src, "add", ".")
	run(src, "commit", "-m", "init")

	out, err := exec.Command("git", "-C", src, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	headSHA = strings.TrimSpace(string(out))

	// Publish as <seedDir>/<owner>/<repo>.git — the cache joins
	// gitBaseURL/owner/repo.git, so the file:// layout must match.
	if err := os.MkdirAll(filepath.Join(seedDir, owner), 0o755); err != nil {
		t.Fatal(err)
	}
	run(seedDir, "clone", "--bare", src, filepath.Join(seedDir, owner, repo+".git"))
	return seedDir, headSHA
}

func TestFetch_CloneOnDemand(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _ := seedBareRepo(t, owner, repo)
	base := t.TempDir()
	c := New(base, "", t.TempDir(), "file://"+seedDir)

	if c.IsCloned(owner, repo) {
		t.Fatal("repo should not be cloned yet")
	}
	if err := c.Fetch(context.Background(), owner, repo, "tok"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !c.IsCloned(owner, repo) {
		t.Fatal("repo should be cloned after Fetch")
	}
	// The temp clone dir must not linger.
	if _, err := os.Stat(c.barePath(owner, repo) + ".tmp-clone"); !os.IsNotExist(err) {
		t.Fatal("tmp-clone dir left behind")
	}
	// PR refs must be fetchable — verify the refspec made it into config.
	out, err := exec.Command("git", "-C", c.barePath(owner, repo), "config", "--get-all", "remote.origin.fetch").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "refs/pull/") {
		t.Fatalf("clone missing PR refspec: %s", out)
	}
}

func TestFetch_CloneFailure_NoLeftover(t *testing.T) {
	base := t.TempDir()
	seed := t.TempDir() // empty — nothing to clone
	c := New(base, "", t.TempDir(), "file://"+seed)

	err := c.Fetch(context.Background(), "o", "missing", "tok")
	if err == nil {
		t.Fatal("expected clone failure")
	}
	if c.IsCloned("o", "missing") {
		t.Fatal("failed clone must not appear cloned")
	}
	if _, serr := os.Stat(c.barePath("o", "missing") + ".tmp-clone"); !os.IsNotExist(serr) {
		t.Fatal("tmp-clone dir left after failed clone")
	}
}

func TestFetch_ConcurrentClones(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)

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
	if !c.IsCloned(owner, repo) {
		t.Fatal("repo should be cloned")
	}
}

func TestReadRepoFile_DefaultBranch(t *testing.T) {
	owner, repo := "o", "r"
	seedDir, _ := seedBareRepo(t, owner, repo)
	c := New(t.TempDir(), "", t.TempDir(), "file://"+seedDir)
	if err := c.Fetch(context.Background(), owner, repo, "tok"); err != nil {
		t.Fatal(err)
	}
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
