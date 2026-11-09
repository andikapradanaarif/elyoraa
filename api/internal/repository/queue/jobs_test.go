package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

func testJob() domain.Review {
	return domain.Review{
		JobKey:     "o/r/1/abc",
		DeliveryID: "d-1",
		Owner:      "o",
		Repo:       "r",
		PRNumber:   1,
		HeadSHA:    "abc",
		Type:       "review",
	}
}

func testTracker(t *testing.T, tr JobTracker) {
	ctx := context.Background()
	j := testJob()

	tr.Enqueued(ctx, j)
	rec, ok := tr.Get(ctx, j.JobKey)
	if !ok {
		t.Fatal("Get after Enqueued returned not found")
	}
	if rec.Status != JobStatusQueued {
		t.Fatalf("status = %q, want queued", rec.Status)
	}
	if rec.Job.JobKey != j.JobKey {
		t.Fatalf("stored job key = %q, want %q", rec.Job.JobKey, j.JobKey)
	}

	tr.Started(ctx, j)
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.Status != JobStatusProcessing {
		t.Fatalf("status = %q, want processing", rec.Status)
	}

	tr.Failed(ctx, j, 4, errors.New("crush exploded"))
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.Status != JobStatusFailed || rec.Error != "crush exploded" || rec.Attempts != 4 {
		t.Fatalf("bad failed record: %+v", rec)
	}
	if rec.FinishedAt == nil {
		t.Fatal("FinishedAt not set on failure")
	}

	// Retry path: re-enqueue the stored job.
	if rec.Job.JobKey == "" {
		t.Fatal("stored job missing, cannot retry")
	}
	tr.Enqueued(ctx, rec.Job)
	tr.Completed(ctx, rec.Job, 2)
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.Status != JobStatusCompleted || rec.Attempts != 2 || rec.Error != "" {
		t.Fatalf("bad completed record: %+v", rec)
	}

	list := tr.List(ctx)
	if len(list) != 1 || list[0].JobKey != j.JobKey {
		t.Fatalf("List returned %+v", list)
	}
}

func TestMemoryJobTracker(t *testing.T) {
	testTracker(t, NewMemoryJobTracker(100))
}

func TestBoltJobTracker(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	defer b.Close()

	tr, err := NewBoltJobTracker(b.db)
	if err != nil {
		t.Fatalf("NewBoltJobTracker: %v", err)
	}
	testTracker(t, tr)

	// Reopen and verify persistence.
	_ = b.Close()
	b, err = OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer b.Close()
	tr, err = NewBoltJobTracker(b.db)
	if err != nil {
		t.Fatalf("NewBoltJobTracker after reopen: %v", err)
	}
	rec, ok := tr.Get(context.Background(), "o/r/1/abc")
	if !ok {
		t.Fatal("record not persisted across reopen")
	}
	if rec.Status != JobStatusCompleted || rec.Job.JobKey != "o/r/1/abc" {
		t.Fatalf("bad persisted record: %+v", rec)
	}
}

func TestMemoryJobTrackerEviction(t *testing.T) {
	tr := NewMemoryJobTracker(3)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c", "d"} {
		tr.Enqueued(ctx, domain.Review{JobKey: k})
	}
	if got := len(tr.List(ctx)); got != 3 {
		t.Fatalf("len = %d, want 3", got)
	}
	if _, ok := tr.Get(ctx, "a"); ok {
		t.Fatal("oldest record should have been evicted")
	}
}

// StuckProcessing must be row-aware: records whose queue row survives are
// not stuck — the job re-runs on its own. Only rows that vanished for good
// (killed mid-flight before inflight tracking existed, or lost between Next
// and Started) are reported dead.
func TestBoltStuckProcessingRowAware(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	defer b.Close()
	tr, err := NewBoltJobTracker(b.db)
	if err != nil {
		t.Fatalf("NewBoltJobTracker: %v", err)
	}
	ctx := context.Background()

	live := domain.Review{JobKey: "o/r/1/live", Owner: "o", Repo: "r", PRNumber: 1}
	inflight := domain.Review{JobKey: "o/r/1/inflight", Owner: "o", Repo: "r", PRNumber: 1}
	deadProc := domain.Review{JobKey: "o/r/1/deadproc", Owner: "o", Repo: "r", PRNumber: 1}
	deadQueued := domain.Review{JobKey: "o/r/1/deadq", Owner: "o", Repo: "r", PRNumber: 1}

	// queued record + live row → not stuck (will be picked up).
	if !b.Put(ctx, live) {
		t.Fatal("Put live failed")
	}
	tr.Enqueued(ctx, live)

	// processing record + in-flight row → not stuck (requeues at open).
	if !b.Put(ctx, inflight) {
		t.Fatal("Put inflight failed")
	}
	tr.Enqueued(ctx, inflight)
	if _, ok := b.Next(ctx); !ok {
		t.Fatal("Next inflight failed")
	}
	tr.Started(ctx, inflight)

	// processing record, no row at all → stuck.
	tr.Enqueued(ctx, deadProc)
	tr.Started(ctx, deadProc)

	// queued record, no row at all → stuck (Next/Started race orphan).
	tr.Enqueued(ctx, deadQueued)

	stuck := tr.StuckProcessing(ctx)
	got := map[string]bool{}
	for _, rec := range stuck {
		got[rec.JobKey] = true
	}
	if len(got) != 2 || !got[deadProc.JobKey] || !got[deadQueued.JobKey] {
		t.Fatalf("StuckProcessing = %v, want {%s %s}", got, deadProc.JobKey, deadQueued.JobKey)
	}
}

func TestNewJobTrackerSelectsBolt(t *testing.T) {
	dir := t.TempDir()
	b, err := OpenBoltDB(filepath.Join(dir, "queue.db"), 0)
	if err != nil {
		t.Fatalf("OpenBoltDB: %v", err)
	}
	defer b.Close()

	tr := NewJobTracker(b)
	if _, ok := tr.(*BoltJobTracker); !ok {
		t.Fatalf("NewJobTracker returned %T, want *BoltJobTracker", tr)
	}
}
