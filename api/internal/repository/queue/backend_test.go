package queue

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

func testBackend(t *testing.T, b Backend) {
	t.Cleanup(func() { _ = b.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	j1 := domain.Review{JobKey: "1", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "a"}
	j2 := domain.Review{JobKey: "2", Owner: "o", Repo: "r", PRNumber: 2, HeadSHA: "b"}

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
	b.MarkDone(ctx, got1)
	b.MarkDone(ctx, got2)
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
	j3 := domain.Review{JobKey: "3", Owner: "o", Repo: "r", PRNumber: 3, HeadSHA: "c"}
	if !b.Put(ctx, j3) {
		t.Fatal("Put j3 failed")
	}
	got, ok := b.Next(ctx)
	if !ok || got.JobKey != j3.JobKey {
		t.Fatalf("expected j3 after reopen, got %+v %v", got, ok)
	}
}

// A job claimed but never MarkDone'd (the process died mid-review) must be
// requeued when the file reopens — Bolt is single-process, so a leftover
// in-flight row always belongs to a dead run.
func TestBoltDBBackendInflightRequeue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	b, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	ctx := context.Background()

	j1 := domain.Review{JobKey: "o/r/1/a", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "a"}
	if !b.Put(ctx, j1) {
		t.Fatal("Put failed")
	}
	if _, ok := b.Next(ctx); !ok {
		t.Fatal("Next failed")
	}
	// Simulate the crash: close without MarkDone.
	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	b2, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()

	got, ok := b2.Next(ctx)
	if !ok || got.JobKey != j1.JobKey {
		t.Fatalf("in-flight job was not requeued: ok=%v job=%+v", ok, got)
	}

	// MarkDone removes the row for good — a third open must be empty.
	b2.MarkDone(ctx, got)
	if err := b2.Close(); err != nil {
		t.Fatalf("close2: %v", err)
	}
	b3, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("reopen2: %v", err)
	}
	defer b3.Close()
	if b3.Len() != 0 {
		t.Fatalf("Len = %d, want 0 after MarkDone", b3.Len())
	}
	ctx2, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, ok := b3.Next(ctx2); ok {
		t.Fatal("queue should be empty after MarkDone")
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
	if !b.Put(ctx, domain.Review{JobKey: "1"}) {
		t.Fatal("Put 1 failed")
	}
	if b.Put(ctx, domain.Review{JobKey: "2"}) {
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
	q := NewWithBackend(1, ProcessorFunc(func(ctx context.Context, j domain.Review) (int, error) {
		processed = append(processed, j.JobKey)
		return 1, nil
	}), nil, b)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { q.Start(ctx); close(done) }()

	q.Submit(ctx, domain.Review{JobKey: "a"})
	q.Submit(ctx, domain.Review{JobKey: "b"})

	// Give workers time to process, then shut down: cancel + Start's worker
	// drain makes the processed slice safe to read.
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done

	if len(processed) != 2 {
		t.Fatalf("expected 2 processed, got %d: %v", len(processed), processed)
	}
}

func TestMain(m *testing.M) {
	// Clean up any leftover test files in the temp dir.
	os.Exit(m.Run())
}
