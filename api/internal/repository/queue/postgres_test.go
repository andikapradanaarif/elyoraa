package queue

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/migration"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests run only when TEST_DATABASE_URL points at a Postgres.
// Example: TEST_DATABASE_URL=postgres://localhost/elyoraa_test go test ./internal/repository/queue/
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set — skipping Postgres integration test")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := migration.New(pool).Up(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, table := range []string{"job_queue", "job_records", "job_claims"} {
		if _, err := pool.Exec(context.Background(), "TRUNCATE "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}
	t.Cleanup(pool.Close)
	return pool
}

func pgJob(key string) domain.Review {
	return domain.Review{
		JobKey: key, Owner: "o", Repo: "r", PRNumber: 1,
		HeadSHA: "abc", Type: "review", DeliveryID: "d-" + key,
	}
}

func TestPostgresBackend_PutNextDone(t *testing.T) {
	pool := testPool(t)
	b := NewPostgresBackend(pool, "inst-1", time.Minute)
	defer b.Close()
	ctx := context.Background()

	if !b.Put(ctx, pgJob("o/r/1/abc")) {
		t.Fatal("Put failed")
	}
	if b.Len() != 1 {
		t.Fatalf("Len = %d, want 1", b.Len())
	}
	j, ok := b.Next(ctx)
	if !ok || j.JobKey != "o/r/1/abc" {
		t.Fatalf("Next = %+v %v", j, ok)
	}
	b.MarkDone(ctx, j)
	if b.Len() != 0 {
		t.Fatal("queue row should be deleted after MarkDone")
	}
}

// Two backend instances must not hand the same job to two workers.
func TestPostgresBackend_SkipLocked(t *testing.T) {
	pool := testPool(t)
	b1 := NewPostgresBackend(pool, "inst-1", time.Minute)
	b2 := NewPostgresBackend(pool, "inst-2", time.Minute)
	defer b1.Close()
	defer b2.Close()
	ctx := context.Background()

	b1.Put(ctx, pgJob("o/r/1/x"))
	b1.Put(ctx, pgJob("o/r/1/y"))

	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for _, b := range []*PostgresBackend{b1, b2} {
		wg.Add(1)
		go func(b *PostgresBackend) {
			defer wg.Done()
			for i := 0; i < 2; i++ {
				cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
				j, ok := b.Next(cctx)
				cancel()
				if !ok {
					return
				}
				mu.Lock()
				claimed[j.JobKey]++
				mu.Unlock()
			}
		}(b)
	}
	wg.Wait()
	for key, n := range claimed {
		if n != 1 {
			t.Fatalf("job %s claimed %d times", key, n)
		}
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d jobs, want 2", len(claimed))
	}
}

// A crashed worker's row must return to queued once its lease expires.
func TestPostgresBackend_LeaseExpiry(t *testing.T) {
	pool := testPool(t)
	dead := NewPostgresBackend(pool, "dead-inst", 100*time.Millisecond)
	live := NewPostgresBackend(pool, "live-inst", time.Minute)
	defer dead.Close()
	defer live.Close()
	ctx := context.Background()

	dead.Put(ctx, pgJob("o/r/1/z"))
	j, ok := dead.Next(ctx) // claimed by "dead" — never MarkDone'd
	if !ok {
		t.Fatal("dead instance should claim the job")
	}
	time.Sleep(300 * time.Millisecond) // lease lapses

	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	j2, ok := live.Next(cctx)
	if !ok || j2.JobKey != j.JobKey {
		t.Fatal("expired lease should re-queue the job for a live replica")
	}
}

// A stale worker's MarkDone must not delete a row re-claimed by a replica.
func TestPostgresBackend_StaleMarkDone(t *testing.T) {
	pool := testPool(t)
	dead := NewPostgresBackend(pool, "dead-inst", 100*time.Millisecond)
	live := NewPostgresBackend(pool, "live-inst", time.Minute)
	defer dead.Close()
	defer live.Close()
	ctx := context.Background()

	dead.Put(ctx, pgJob("o/r/1/w"))
	j1, _ := dead.Next(ctx)
	time.Sleep(300 * time.Millisecond)
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	j2, ok := live.Next(cctx)
	if !ok {
		t.Fatal("re-claim after lease expiry")
	}

	dead.MarkDone(ctx, j1) // stale owner — must NOT delete live's row
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM job_queue WHERE job_key='o/r/1/w'`).Scan(&n)
	if n != 1 {
		t.Fatal("stale MarkDone deleted the live claim's row")
	}
	live.MarkDone(ctx, j2)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM job_queue WHERE job_key='o/r/1/w'`).Scan(&n)
	if n != 0 {
		t.Fatal("live MarkDone should delete the row")
	}
}

func TestPostgresBackend_SignalStop(t *testing.T) {
	pool := testPool(t)
	b := NewPostgresBackend(pool, "inst-1", time.Minute)
	defer b.Close()

	b.SignalStop()
	if b.Put(context.Background(), pgJob("o/r/1/s")) {
		t.Fatal("Put after SignalStop should be rejected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := b.Next(ctx); ok {
		t.Fatal("Next after SignalStop should return false")
	}
}

func TestPostgresJobTracker_Lifecycle(t *testing.T) {
	pool := testPool(t)
	tr := NewPostgresJobTracker(pool, "inst-1")
	ctx := context.Background()
	j := pgJob("o/r/1/t")

	tr.Enqueued(ctx, j)
	rec, ok := tr.Get(ctx, j.JobKey)
	if !ok || rec.Status != JobStatusQueued {
		t.Fatalf("after Enqueued: %+v", rec)
	}
	tr.Started(ctx, j)
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.Status != JobStatusProcessing || rec.StartedAt == nil {
		t.Fatalf("after Started: %+v", rec)
	}
	tr.Annotate(ctx, j.JobKey, JobAnnotation{RefinedLines: 42, Type: "split", AttemptSeconds: 17, Actions: []string{"request_review"}})
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.RefinedLines != 42 || rec.Type != "split" || rec.AttemptSeconds != 17 || len(rec.Actions) != 1 {
		t.Fatalf("after Annotate: %+v", rec)
	}
	if rec.Job.Type != "split" {
		t.Fatalf("Annotate Type should update stored payload, got %q", rec.Job.Type)
	}
	tr.Completed(ctx, j, 2)
	rec, _ = tr.Get(ctx, j.JobKey)
	if rec.Status != JobStatusCompleted || rec.Attempts != 2 || rec.FinishedAt == nil {
		t.Fatalf("after Completed: %+v", rec)
	}
	if rec.Job.JobKey != j.JobKey {
		t.Fatal("stored payload must round-trip for manual retry")
	}
	if list := tr.List(ctx); len(list) != 1 {
		t.Fatalf("List = %d records", len(list))
	}
}

// Multi-replica safety: a processing record whose queue row survives is a
// live job on another replica — the reconciler must not touch it.
func TestPostgresJobTracker_StuckProcessing(t *testing.T) {
	pool := testPool(t)
	tr := NewPostgresJobTracker(pool, "inst-1")
	b := NewPostgresBackend(pool, "inst-1", time.Minute)
	defer b.Close()
	ctx := context.Background()

	// Job A: processing + queue row still claimed → owned by a replica.
	a := pgJob("o/r/1/live")
	tr.Enqueued(ctx, a)
	b.Put(ctx, a)
	if _, ok := b.Next(ctx); !ok {
		t.Fatal("claim A")
	}
	tr.Started(ctx, a)

	// Job B: processing record with no queue row (worker died between the
	// delete and the record write) → orphaned.
	orphan := pgJob("o/r/1/orphan")
	tr.Enqueued(ctx, orphan)
	tr.Started(ctx, orphan)

	stuck := tr.StuckProcessing(ctx)
	if len(stuck) != 1 || stuck[0].JobKey != "o/r/1/orphan" {
		t.Fatalf("StuckProcessing = %+v", stuck)
	}
}
