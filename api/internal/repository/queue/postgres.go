package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// notifyChannel wakes blocked Next calls when a job lands.
const notifyChannel = "elyoraa_jobs"

// PostgresBackend is a multi-instance-safe queue on PostgreSQL. Workers
// claim rows with FOR UPDATE SKIP LOCKED and hold a lease while processing;
// a crashed worker's row returns to 'queued' when the lease expires, so a
// replica dying mid-review re-runs the job instead of losing it. LISTEN on
// notifyChannel gives prompt wakeups; a periodic poll is the fallback.
type PostgresBackend struct {
	pool     *pgxpool.Pool
	instance string
	lease    time.Duration
	poll     time.Duration

	wake         chan struct{}
	stopCh       chan struct{}
	stopping     atomic.Bool
	closed       atomic.Bool
	listenCancel context.CancelFunc
	wg           sync.WaitGroup
}

// NewPostgresBackend creates a queue backend. lease should exceed the
// longest possible job runtime (lease expiry re-queues the job while it may
// still be running); instance identifies this replica for claim ownership.
func NewPostgresBackend(pool *pgxpool.Pool, instance string, lease time.Duration) *PostgresBackend {
	if lease <= 0 {
		lease = 20 * time.Minute
	}
	listenCtx, listenCancel := context.WithCancel(context.Background())
	b := &PostgresBackend{
		pool:         pool,
		instance:     instance,
		lease:        lease,
		poll:         2 * time.Second,
		wake:         make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
		listenCancel: listenCancel,
	}
	b.wg.Add(1)
	go b.listen(listenCtx)
	return b
}

// Put inserts or refreshes a queued row. An existing row for the same key —
// stale, running, or finished-but-undeleted — is reset to queued so the job
// runs again with its newest payload.
func (b *PostgresBackend) Put(ctx context.Context, j domain.Review) bool {
	if b.stopping.Load() || b.closed.Load() {
		return false
	}
	payload, err := json.Marshal(j)
	if err != nil {
		slog.Error("queue put: marshal", "job", j.JobKey, "error", err)
		return false
	}
	_, err = b.pool.Exec(ctx, `
		INSERT INTO job_queue (job_key, payload, status, enqueued_at)
		VALUES ($1, $2, 'queued', now())
		ON CONFLICT (job_key) DO UPDATE SET
			payload = EXCLUDED.payload, status = 'queued',
			locked_by = NULL, lease_until = NULL, enqueued_at = now()`,
		j.JobKey, payload)
	if err != nil {
		slog.Error("queue put", "job", j.JobKey, "error", err)
		return false
	}
	// Wake a Next waiter on every replica cheaply; delivery failure is
	// harmless — the poll fallback covers it.
	_, _ = b.pool.Exec(ctx, "SELECT pg_notify($1, '')", notifyChannel)
	return true
}

// Next claims the oldest queued job. It re-queues expired leases first so a
// crashed sibling's work resurfaces promptly.
func (b *PostgresBackend) Next(ctx context.Context) (domain.Review, bool) {
	for {
		if b.stopping.Load() || b.closed.Load() {
			return domain.Review{}, false
		}
		b.requeueExpiredLeases(ctx)
		if j, ok := b.claim(ctx); ok {
			return j, true
		}
		select {
		case <-ctx.Done():
			return domain.Review{}, false
		case <-b.stopCh:
			return domain.Review{}, false
		case <-b.wake:
		case <-time.After(b.poll):
		}
	}
}

func (b *PostgresBackend) claim(ctx context.Context) (domain.Review, bool) {
	var payload []byte
	err := b.pool.QueryRow(ctx, `
		UPDATE job_queue
		SET status = 'running', locked_by = $1,
		    lease_until = now() + $2 * interval '1 second'
		WHERE job_key = (
			SELECT job_key FROM job_queue
			WHERE status = 'queued'
			ORDER BY enqueued_at
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING payload`, b.instance, b.lease.Seconds()).Scan(&payload)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("queue claim", "error", err)
		}
		return domain.Review{}, false
	}
	var j domain.Review
	if err := json.Unmarshal(payload, &j); err != nil {
		slog.Error("queue claim: unmarshal", "error", err)
		return domain.Review{}, false
	}
	return j, true
}

func (b *PostgresBackend) requeueExpiredLeases(ctx context.Context) {
	if _, err := b.pool.Exec(ctx, `
		UPDATE job_queue
		SET status = 'queued', locked_by = NULL, lease_until = NULL
		WHERE status = 'running' AND lease_until < now()`); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("queue lease sweep", "error", err)
	}
}

// MarkDone deletes the claimed row. The locked_by guard prevents a stale
// worker (whose lease expired and was re-claimed by another replica) from
// deleting the new owner's row.
func (b *PostgresBackend) MarkDone(ctx context.Context, j domain.Review) {
	if _, err := b.pool.Exec(ctx,
		`DELETE FROM job_queue WHERE job_key = $1 AND locked_by = $2`,
		j.JobKey, b.instance); err != nil {
		slog.Error("queue mark done", "job", j.JobKey, "error", err)
	}
}

// Len returns the number of queued (unclaimed) jobs.
func (b *PostgresBackend) Len() int {
	var n int
	if err := b.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM job_queue WHERE status = 'queued'`).Scan(&n); err != nil {
		return 0
	}
	return n
}

// SignalStop rejects new work and wakes Next waiters without releasing the
// pool — the shared PostgresJobTracker keeps recording outcomes during the
// drain.
func (b *PostgresBackend) SignalStop() {
	if b.stopping.CompareAndSwap(false, true) {
		close(b.stopCh)
	}
}

// Close stops the LISTEN goroutine. The pool is shared and closed by main.
func (b *PostgresBackend) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		b.SignalStop()
		b.listenCancel()
		b.wg.Wait()
	}
	return nil
}

// listen holds a dedicated connection on the notify channel and fans
// notifications into the wake channel so workers don't poll constantly.
func (b *PostgresBackend) listen(ctx context.Context) {
	defer b.wg.Done()
	for {
		if b.closed.Load() {
			return
		}
		conn, err := b.pool.Acquire(ctx)
		if err != nil {
			if !sleepOrStop(b.stopCh, 5*time.Second) {
				return
			}
			continue
		}
		if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
			conn.Release()
			if !sleepOrStop(b.stopCh, 5*time.Second) {
				return
			}
			continue
		}
		for {
			_, err := conn.Conn().WaitForNotification(ctx)
			if err != nil {
				conn.Release()
				break
			}
			select {
			case b.wake <- struct{}{}:
			default:
			}
		}
		if b.closed.Load() {
			return
		}
		if !sleepOrStop(b.stopCh, time.Second) {
			return
		}
	}
}

func sleepOrStop(stop <-chan struct{}, d time.Duration) bool {
	select {
	case <-stop:
		return false
	case <-time.After(d):
		return true
	}
}

// PostgresJobTracker records job outcomes in job_records — the same table
// every replica writes, so /jobs, calibration, and retries are global.
type PostgresJobTracker struct {
	pool     *pgxpool.Pool
	instance string
}

// NewPostgresJobTracker creates the tracker. Migrations must have run.
func NewPostgresJobTracker(pool *pgxpool.Pool, instance string) *PostgresJobTracker {
	return &PostgresJobTracker{pool: pool, instance: instance}
}

func marshalJob(j domain.Review) []byte {
	b, err := json.Marshal(j)
	if err != nil {
		return nil
	}
	return b
}

// upsert inserts a skeleton record when one doesn't exist (a queue row can
// outlive its record if a replica died between Put and Enqueued), then
// applies the lifecycle mutation.
func (t *PostgresJobTracker) upsert(ctx context.Context, j domain.Review, set string, args ...any) {
	query := `
		INSERT INTO job_records (job_key, delivery_id, job_type, owner, repo, pr_number,
			status, job, enqueued_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'queued', $7, now(), now())
		ON CONFLICT (job_key) DO UPDATE SET ` + set
	_, _ = t.pool.Exec(ctx, query,
		append([]any{j.JobKey, j.DeliveryID, j.Type, j.Owner, j.Repo, j.PRNumber, marshalJob(j)}, args...)...)
}

func (t *PostgresJobTracker) Enqueued(ctx context.Context, j domain.Review) {
	t.upsert(ctx, j, `delivery_id = EXCLUDED.delivery_id, job_type = EXCLUDED.job_type,
		status = 'queued', error = NULL, attempts = 0, job = EXCLUDED.job,
		enqueued_at = now(), started_at = NULL, finished_at = NULL, updated_at = now()`)
}

func (t *PostgresJobTracker) Started(ctx context.Context, j domain.Review) {
	t.upsert(ctx, j, `status = '`+JobStatusProcessing+`', started_at = now(), instance = $8, updated_at = now()`, t.instance)
}

func (t *PostgresJobTracker) Completed(ctx context.Context, j domain.Review, attempts int) {
	t.upsert(ctx, j, `status = '`+JobStatusCompleted+`', error = NULL, attempts = $8,
		finished_at = now(), updated_at = now()`, attempts)
}

func (t *PostgresJobTracker) Cancelled(ctx context.Context, j domain.Review, attempts int) {
	t.upsert(ctx, j, `status = '`+JobStatusCancelled+`', attempts = $8,
		finished_at = now(), updated_at = now()`, attempts)
}

func (t *PostgresJobTracker) Failed(ctx context.Context, j domain.Review, attempts int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	t.upsert(ctx, j, `status = '`+JobStatusFailed+`', error = $8, attempts = $9,
		finished_at = now(), updated_at = now()`, msg, attempts)
}

func (t *PostgresJobTracker) Annotate(ctx context.Context, key string, a JobAnnotation) {
	if len(a.Actions) > 0 {
		b, _ := json.Marshal(a.Actions)
		_, _ = t.pool.Exec(ctx, `UPDATE job_records SET actions = $2, updated_at = now() WHERE job_key = $1`, key, b)
	}
	if a.RefinedLines > 0 {
		_, _ = t.pool.Exec(ctx, `UPDATE job_records SET refined_lines = $2, updated_at = now() WHERE job_key = $1`, key, a.RefinedLines)
	}
	if a.AttemptSeconds > 0 {
		_, _ = t.pool.Exec(ctx, `UPDATE job_records SET attempt_seconds = $2, updated_at = now() WHERE job_key = $1`, key, a.AttemptSeconds)
	}
	if a.Type != "" {
		// Mirror the type change into the stored payload too (retry source).
		_, _ = t.pool.Exec(ctx, `UPDATE job_records SET job_type = $2,
			job = jsonb_set(job, '{Type}', to_jsonb($2::text)), updated_at = now()
			WHERE job_key = $1`, key, a.Type)
	}
}

type scanRow interface{ Scan(dest ...any) error }

func (t *PostgresJobTracker) scanRecord(row scanRow) (JobRecord, bool) {
	var r JobRecord
	var job []byte
	var startedAt, finishedAt, enqueuedAt *time.Time
	var errMsg, deliveryID, jobType, instance *string
	var actions []byte
	var refined *int
	var attemptSec *int64
	err := row.Scan(&r.JobKey, &deliveryID, &jobType, &r.Owner, &r.Repo, &r.PRNumber,
		&r.Status, &errMsg, &r.Attempts, &actions, &refined, &attemptSec,
		&job, &instance, &enqueuedAt, &startedAt, &finishedAt, &r.UpdatedAt)
	if err != nil {
		return JobRecord{}, false
	}
	if deliveryID != nil {
		r.DeliveryID = *deliveryID
	}
	if jobType != nil {
		r.Type = *jobType
	}
	if errMsg != nil {
		r.Error = *errMsg
	}
	if len(actions) > 0 {
		_ = json.Unmarshal(actions, &r.Actions)
	}
	if refined != nil {
		r.RefinedLines = *refined
	}
	if attemptSec != nil {
		r.AttemptSeconds = *attemptSec
	}
	if enqueuedAt != nil {
		r.EnqueuedAt = *enqueuedAt
	}
	r.StartedAt, r.FinishedAt = startedAt, finishedAt
	if startedAt != nil && finishedAt != nil {
		r.DurationSeconds = int64(finishedAt.Sub(*startedAt).Seconds())
	}
	if len(job) > 0 {
		_ = json.Unmarshal(job, &r.Job)
	}
	return r, true
}

const recordCols = `job_key, delivery_id, job_type, owner, repo, pr_number,
	status, error, attempts, actions, refined_lines, attempt_seconds,
	job, instance, enqueued_at, started_at, finished_at, updated_at`

func (t *PostgresJobTracker) List(ctx context.Context) []JobRecord {
	rows, err := t.pool.Query(ctx, `
		SELECT `+recordCols+` FROM job_records ORDER BY updated_at DESC LIMIT 500`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []JobRecord
	for rows.Next() {
		if r, ok := t.scanRecord(rows); ok {
			out = append(out, r)
		}
	}
	return out
}

func (t *PostgresJobTracker) Get(ctx context.Context, key string) (JobRecord, bool) {
	return t.scanRecord(t.pool.QueryRow(ctx, `
		SELECT `+recordCols+` FROM job_records WHERE job_key = $1`, key))
}

// StuckProcessing returns 'processing'/'queued' records with no live queue
// row — the only records safe to fail at startup. A record whose queue row
// still exists is owned by some replica (running now, or re-queued when its
// lease expires) and must be left alone.
func (t *PostgresJobTracker) StuckProcessing(ctx context.Context) []JobRecord {
	rows, err := t.pool.Query(ctx, `
		SELECT `+recordCols+` FROM job_records r
		WHERE r.status IN ($1, $2)
		  AND NOT EXISTS (SELECT 1 FROM job_queue q WHERE q.job_key = r.job_key)`,
		JobStatusProcessing, JobStatusQueued)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []JobRecord
	for rows.Next() {
		if r, ok := t.scanRecord(rows); ok {
			out = append(out, r)
		}
	}
	return out
}
