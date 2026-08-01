package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/job"
)

func testBackend(t *testing.T, b Backend) {
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	j1 := job.Review{JobKey: "1", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "a"}
	j2 := job.Review{JobKey: "2", Owner: "o", Repo: "r", PRNumber: 2, HeadSHA: "b"}

	if !b.Put(ctx, j1) {
		t.Fatal("Put j1 failed")
	}
	if !b.Put(ctx, j2) {
		t.Fatal("Put j2 failed")
	}

	got1, ok := b.Next(ctx)
	if !ok {
		t.Fatal("Next failed")
	}
	if got1.JobKey != j1.JobKey {
		t.Fatalf("expected job %s, got %s", j1.JobKey, got1.JobKey)
	}

	got2, ok := b.Next(ctx)
	if !ok {
		t.Fatal("Next failed")
	}
	if got2.JobKey != j2.JobKey {
		t.Fatalf("expected job %s, got %s", j2.JobKey, got2.JobKey)
	}
}

func TestMemoryBackend(t *testing.T) {
	b := NewMemoryBackend(2)
	testBackend(t, b)
}

func TestBoltDBBackend(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	testBackend(t, b)

	// Re-open and verify persistence / recovery.
	_ = b.Close()
	b, err = OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Put a new job after reopen.
	j3 := job.Review{JobKey: "3", Owner: "o", Repo: "r", PRNumber: 3, HeadSHA: "c"}
	if !b.Put(ctx, j3) {
		t.Fatal("Put j3 failed")
	}
	got, ok := b.Next(ctx)
	if !ok || got.JobKey != j3.JobKey {
		t.Fatalf("expected j3 after reopen, got %+v %v", got, ok)
	}
}

func TestBoltDBBackendMaxSize(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 1)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	defer b.Close()

	ctx := context.Background()
	if !b.Put(ctx, job.Review{JobKey: "1"}) {
		t.Fatal("Put 1 failed")
	}
	if b.Put(ctx, job.Review{JobKey: "2"}) {
		t.Fatal("Put 2 should have failed (queue full)")
	}
}

func TestBoltDBBackendCancel(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok := b.Next(ctx)
	if ok {
		t.Fatal("Next should return false on cancelled context")
	}
}

func TestQueueWithFileBackend(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}

	var processed []string
	q := NewWithBackend(1, ProcessorFunc(func(ctx context.Context, j job.Review) error {
		processed = append(processed, j.JobKey)
		return nil
	}), nil, b)

	ctx, cancel := context.WithCancel(context.Background())
	go q.Start(ctx)

	q.Submit(ctx, job.Review{JobKey: "a"})
	q.Submit(ctx, job.Review{JobKey: "b"})

	// Give workers time to process.
	time.Sleep(200 * time.Millisecond)
	cancel()

	if len(processed) != 2 {
		t.Fatalf("expected 2 processed, got %d: %v", len(processed), processed)
	}
}

func TestMain(m *testing.M) {
	// Clean up any leftover test files in the temp dir.
	os.Exit(m.Run())
}
