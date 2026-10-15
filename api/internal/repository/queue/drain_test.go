package queue

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

func TestMemoryBackendNextAfterClose(t *testing.T) {
	b := NewMemoryBackend(4)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A closed backend must report exhaustion, not spin zero-value jobs.
	if _, ok := b.Next(context.Background()); ok {
		t.Fatal("Next on closed backend should return false")
	}
	// Double close must not panic.
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Put on a closed backend must be rejected, not panic on the channel.
	if b.Put(context.Background(), domain.Review{JobKey: "x"}) {
		t.Fatal("Put on closed backend should return false")
	}
}

func TestQueueStopDrainsInflight(t *testing.T) {
	var started, finished atomic.Bool
	release := make(chan struct{})
	q := NewWithBackend(1, ProcessorFunc(func(ctx context.Context, j domain.Review) (int, error) {
		started.Store(true)
		select {
		case <-release:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		finished.Store(true)
		return 1, nil
	}), nil, NewMemoryBackend(8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Start(ctx)

	if !q.Submit(ctx, domain.Review{JobKey: "a"}) {
		t.Fatal("submit before Stop should succeed")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !started.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !started.Load() {
		t.Fatal("worker never picked the job")
	}

	// Submissions during drain are rejected.
	stopDone := make(chan struct{})
	go func() { q.Stop(context.Background()); close(stopDone) }()
	for i := 0; i < 20; i++ {
		time.Sleep(5 * time.Millisecond)
		if !q.Submit(ctx, domain.Review{JobKey: "b"}) {
			break
		}
		if i == 19 {
			t.Fatal("Submit during drain should be rejected")
		}
	}

	// Stop must wait for the in-flight job to finish.
	select {
	case <-stopDone:
		t.Fatal("Stop returned while a job was still in flight")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after in-flight job finished")
	}
	if !finished.Load() {
		t.Fatal("in-flight job was not allowed to finish")
	}
}

// The Bolt tracker shares the backend's db handle — Stop must keep it open
// until workers finish, so an in-flight job's outcome still records.
func TestBoltStopPersistsInflightOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	b, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}

	release := make(chan struct{})
	q := NewWithBackend(1, ProcessorFunc(func(ctx context.Context, j domain.Review) (int, error) {
		<-release
		return 1, nil
	}), nil, b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Start(ctx)

	job := domain.Review{JobKey: "o/r/1/abc", Owner: "o", Repo: "r", PRNumber: 1, HeadSHA: "abc"}
	if !q.Submit(ctx, job) {
		t.Fatal("submit failed")
	}

	stopDone := make(chan struct{})
	go func() { q.Stop(context.Background()); close(stopDone) }()
	// Let the worker pick the job, then finish it mid-drain.
	time.Sleep(150 * time.Millisecond)
	close(release)
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not drain")
	}

	// Reopen the file — the in-flight completion must have been persisted.
	b2, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()
	rec, ok := NewJobTracker(b2).Get(ctx, "o/r/1/abc")
	if !ok {
		t.Fatal("job record missing after reopen")
	}
	if rec.Status != JobStatusCompleted {
		t.Fatalf("status = %q, want completed", rec.Status)
	}
}

// Queued-but-unstarted jobs must stay in the Bolt file for the next boot.
func TestBoltStopLeavesQueuedJobs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.db")
	b, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	// No workers running — the job sits in the file.
	q := NewWithBackend(1, nil, nil, b)
	if !q.Submit(context.Background(), domain.Review{JobKey: "o/r/2/def"}) {
		t.Fatal("submit failed")
	}
	q.Stop(context.Background())

	b2, err := OpenBoltDB(path, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b2.Close()
	j, ok := b2.Next(context.Background())
	if !ok || j.JobKey != "o/r/2/def" {
		t.Fatalf("queued job not persisted across stop: ok=%v job=%+v", ok, j)
	}
}
