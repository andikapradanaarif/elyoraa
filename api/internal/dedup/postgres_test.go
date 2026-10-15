package dedup

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/migration"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Integration tests run only when TEST_DATABASE_URL points at a Postgres.
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
	if _, err := pool.Exec(context.Background(), "TRUNCATE job_queue, job_records, job_claims"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func pgDedupJob(key, delivery, origin string) domain.Review {
	return domain.Review{
		JobKey: key, Owner: "o", Repo: "r", PRNumber: 7, HeadSHA: "abc",
		Type: "review", DeliveryID: delivery, Origin: origin,
	}
}

// The core HA property: a supersede claimed on replica B must cancel the job
// running on replica A — via the claim flag the watcher observes.
func TestPostgresDedup_CrossInstanceSupersede(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	da := newPostgres(ctx, pool, "inst-A", 50*time.Millisecond)
	db := newPostgres(ctx, pool, "inst-B", 50*time.Millisecond)

	old := pgDedupJob("o/r/7/abc", "d-old", domain.OriginMentionKeyword)
	jobCtx, _, ok := da.TryStart(ctx, old)
	if !ok {
		t.Fatal("initial TryStart")
	}

	// B supersedes with an accepted-intent job, same key, new delivery.
	newer := pgDedupJob("o/r/7/abc", "d-new", domain.OriginAccepted)
	if _, _, ok := db.TryStart(ctx, newer); !ok {
		t.Fatal("stronger origin must supersede")
	}

	select {
	case <-jobCtx.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("superseded job's context was not cancelled across instances")
	}
}

// A redelivery carries the same delivery ID — never a supersede.
func TestPostgresDedup_RedeliveryDropped(t *testing.T) {
	pool := testPool(t)
	d := NewPostgres(context.Background(), pool, "inst-A")

	j := pgDedupJob("o/r/7/abc", "d-1", domain.OriginAccepted)
	if _, _, ok := d.TryStart(context.Background(), j); !ok {
		t.Fatal("first claim")
	}
	if _, _, ok := d.TryStart(context.Background(), j); ok {
		t.Fatal("same delivery ID must not supersede")
	}
}

// Done from a superseded instance must not clear the replacement's claim.
func TestPostgresDedup_StaleDoneIsScoped(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	da := NewPostgres(ctx, pool, "inst-A")
	db := NewPostgres(ctx, pool, "inst-B")

	old := pgDedupJob("o/r/7/abc", "d-old", domain.OriginMentionKeyword)
	da.TryStart(ctx, old)
	newer := pgDedupJob("o/r/7/abc", "d-new", domain.OriginAccepted)
	db.TryStart(ctx, newer)

	da.Done(old) // stale — must not delete d-new's claim

	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM job_claims WHERE job_key='o/r/7/abc'`).Scan(&n)
	if n != 1 {
		t.Fatal("stale Done removed the replacement's claim")
	}
	// And a fresh claim for the key must still reject weaker intent.
	if _, _, ok := da.TryStart(ctx, pgDedupJob("o/r/7/abc", "d-third", domain.OriginAuto)); ok {
		t.Fatal("auto must not supersede the accepted claim")
	}
}

// A new review for the same PR supersedes an in-flight different-SHA job.
func TestPostgresDedup_SamePRSupersede(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	da := newPostgres(ctx, pool, "inst-A", 50*time.Millisecond)

	old := pgDedupJob("o/r/7/sha-old", "d-1", domain.OriginMentionKeyword)
	jobCtx, _, ok := da.TryStart(ctx, old)
	if !ok {
		t.Fatal("first claim")
	}
	newer := pgDedupJob("o/r/7/sha-new", "d-2", domain.OriginMentionKeyword)
	if _, _, ok := da.TryStart(ctx, newer); !ok {
		t.Fatal("same-PR new SHA must supersede")
	}
	select {
	case <-jobCtx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("old-SHA job ctx not cancelled")
	}
}

// Respond jobs must never supersede a running review for the same PR.
func TestPostgresDedup_RespondDoesNotSweep(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	d := NewPostgres(ctx, pool, "inst-A")

	rev := pgDedupJob("o/r/7/abc", "d-1", domain.OriginMentionKeyword)
	if _, _, ok := d.TryStart(ctx, rev); !ok {
		t.Fatal("review claim")
	}
	resp := pgDedupJob(RespondKey("o", "r", 7, 42), "d-2", domain.OriginDispatched)
	resp.Type = "respond"
	if _, _, ok := d.TryStart(ctx, resp); !ok {
		t.Fatal("respond claim")
	}
	// The review claim must still be there.
	var superseded bool
	_ = pool.QueryRow(ctx,
		`SELECT superseded FROM job_claims WHERE job_key='o/r/7/abc'`).Scan(&superseded)
	if superseded {
		t.Fatal("respond job superseded a review — must never happen")
	}
}

// BindCancel must reject a worker whose claim was superseded while queued.
func TestPostgresDedup_BindCancelRejectsSuperseded(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	da := NewPostgres(ctx, pool, "inst-A")
	db := NewPostgres(ctx, pool, "inst-B")

	old := pgDedupJob("o/r/7/abc", "d-old", domain.OriginMentionKeyword)
	da.TryStart(ctx, old)
	newer := pgDedupJob("o/r/7/abc", "d-new", domain.OriginAccepted)
	db.TryStart(ctx, newer)

	// The stale instance's worker waking up must not bind.
	if da.BindCancel(old, func() {}) {
		t.Fatal("superseded instance must not bind")
	}
	// The new instance binds fine.
	if !db.BindCancel(newer, func() {}) {
		t.Fatal("current owner should bind")
	}
}
