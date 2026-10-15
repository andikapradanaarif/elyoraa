package dedup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresDedup is the multi-replica dedup: in-flight claims live in
// job_claims so any instance can evaluate supersession. Cancellation can't
// cross processes, so it travels through the row: a superseded claim is
// flagged (or replaced), and the owning replica's watcher — polling the
// claims it holds — cancels the matching local job contexts.
//
// Advisory locks serialize the claim decision per PR across replicas, so
// two webhooks landing on different replicas can't both claim.
type PostgresDedup struct {
	pool     *pgxpool.Pool
	instance string
	poll     time.Duration

	mu      sync.Mutex
	running map[string]*localState
}

type localState struct {
	job     domain.Review
	cancels []context.CancelFunc
}

// NewPostgres creates the Postgres dedup. ctx bounds the watcher goroutine.
func NewPostgres(ctx context.Context, pool *pgxpool.Pool, instance string) *PostgresDedup {
	return newPostgres(ctx, pool, instance, time.Second)
}

func newPostgres(ctx context.Context, pool *pgxpool.Pool, instance string, poll time.Duration) *PostgresDedup {
	d := &PostgresDedup{
		pool:     pool,
		instance: instance,
		poll:     poll,
		running:  make(map[string]*localState),
	}
	go d.watch(ctx)
	return d
}

// TryStart registers a claim, superseding weaker in-flight intent for the
// same key and any in-flight job for the same PR (non-respond). Mirrors the
// in-memory Dedup semantics; the origin table decides same-key supersession.
func (d *PostgresDedup) TryStart(parent context.Context, j domain.Review) (context.Context, *domain.Review, bool) {
	prKey := fmt.Sprintf("%s/%s/%d", j.Owner, j.Repo, j.PRNumber)

	tx, err := d.pool.Begin(parent)
	if err != nil {
		slog.Error("dedup trystart: begin", "error", err)
		return nil, nil, false
	}
	defer tx.Rollback(context.Background())

	// Serialize claim decisions for this PR (respond jobs have their own
	// keyspace — a mention must never cancel a running review).
	lockKey := prKey
	if j.IsRespond() {
		lockKey = j.JobKey
	}
	if _, err := tx.Exec(parent, `SELECT pg_advisory_xact_lock(hashtext($1))`, lockKey); err != nil {
		return nil, nil, false
	}

	var payload []byte
	var cur domain.Review
	var curSuperseded bool
	scanErr := tx.QueryRow(parent,
		`SELECT payload, superseded FROM job_claims WHERE job_key = $1 FOR UPDATE`, j.JobKey).
		Scan(&payload, &curSuperseded)
	if scanErr == nil && !curSuperseded {
		// A superseded row is a corpse awaiting cleanup — always reclaimable.
		// Live claims go through the origin table.
		if err := json.Unmarshal(payload, &cur); err != nil || !canSupersede(&cur, &j) {
			return nil, nil, false
		}
	}
	if scanErr == nil && j.PendingCommentID == 0 {
		if err := json.Unmarshal(payload, &cur); err == nil {
			j.PendingCommentID = cur.PendingCommentID
		}
	}
	if scanErr != nil && !errors.Is(scanErr, pgx.ErrNoRows) {
		slog.Error("dedup trystart: claim read", "error", scanErr)
		return nil, nil, false
	}

	// Same-PR takeover: mark every other in-flight claim superseded. The
	// pending comment hands off from whichever claim has one.
	var supersededKeys []string
	if !j.IsRespond() {
		// Respond claims carry owner/repo/pr too but live outside the review
		// keyspace (the in-memory version's prKey+"/" prefix never matches
		// "respond/...") — the NOT LIKE keeps them out of the sweep.
		if j.PendingCommentID == 0 {
			var pcID *int64
			_ = tx.QueryRow(parent, `
				SELECT pending_comment_id FROM job_claims
				WHERE owner = $1 AND repo = $2 AND pr_number = $3
				  AND job_key <> $4 AND job_key NOT LIKE 'respond/%'
				  AND NOT superseded AND pending_comment_id > 0
				LIMIT 1`, j.Owner, j.Repo, j.PRNumber, j.JobKey).Scan(&pcID)
			if pcID != nil {
				j.PendingCommentID = *pcID
			}
		}
		rows, err := tx.Query(parent, `
			UPDATE job_claims SET superseded = true, superseded_at = now()
			WHERE owner = $1 AND repo = $2 AND pr_number = $3
			  AND job_key <> $4 AND job_key NOT LIKE 'respond/%' AND NOT superseded
			RETURNING job_key`, j.Owner, j.Repo, j.PRNumber, j.JobKey)
		if err != nil {
			slog.Error("dedup trystart: supersede", "error", err)
			return nil, nil, false
		}
		for rows.Next() {
			var k string
			if rows.Scan(&k) == nil {
				supersededKeys = append(supersededKeys, k)
			}
		}
		rows.Close()
	}

	payload, _ = json.Marshal(j)
	if _, err := tx.Exec(parent, `
		INSERT INTO job_claims (job_key, owner, repo, pr_number, job_type, delivery_id,
			origin, payload, pending_comment_id, instance, superseded, claimed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false,now())
		ON CONFLICT (job_key) DO UPDATE SET
			payload = EXCLUDED.payload, job_type = EXCLUDED.job_type,
			delivery_id = EXCLUDED.delivery_id, origin = EXCLUDED.origin,
			pending_comment_id = EXCLUDED.pending_comment_id,
			instance = EXCLUDED.instance, superseded = false,
			superseded_at = NULL, claimed_at = now()`,
		j.JobKey, j.Owner, j.Repo, j.PRNumber, j.Type, j.DeliveryID,
		j.Origin, payload, j.PendingCommentID, d.instance); err != nil {
		slog.Error("dedup trystart: claim write", "error", err)
		return nil, nil, false
	}
	if err := tx.Commit(parent); err != nil {
		slog.Error("dedup trystart: commit", "error", err)
		return nil, nil, false
	}

	// Fast-path local cancels — a same-process supersede shouldn't wait for
	// the watcher. The watcher covers cross-replica claims.
	d.mu.Lock()
	for _, k := range supersededKeys {
		if st, ok := d.running[k]; ok {
			for _, c := range st.cancels {
				c()
			}
			delete(d.running, k)
		}
	}
	if st, ok := d.running[j.JobKey]; ok && st.job.DeliveryID != j.DeliveryID {
		for _, c := range st.cancels {
			c()
		}
		delete(d.running, j.JobKey)
	}

	ctx, cancel := context.WithCancel(parent)
	d.running[j.JobKey] = &localState{job: j, cancels: []context.CancelFunc{cancel}}
	d.mu.Unlock()
	return ctx, &j, true
}

// BindCancel attaches a cancel func so a later replacement can terminate the
// in-flight job. Returns false when the claim is gone, flagged superseded,
// or owned by a different delivery — i.e. this instance lost its slot while
// queued.
func (d *PostgresDedup) BindCancel(j domain.Review, cancel context.CancelFunc) bool {
	var deliveryID string
	var superseded bool
	err := d.pool.QueryRow(context.Background(),
		`SELECT delivery_id, superseded FROM job_claims WHERE job_key = $1`,
		j.JobKey).Scan(&deliveryID, &superseded)
	if errors.Is(err, pgx.ErrNoRows) {
		// Job recovered from the queue after a restart — no claim row
		// survives (or the owner crashed). Re-register so a future
		// supersede still finds it.
		payload, _ := json.Marshal(j)
		_, _ = d.pool.Exec(context.Background(), `
			INSERT INTO job_claims (job_key, owner, repo, pr_number, job_type,
				delivery_id, origin, payload, pending_comment_id, instance)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT (job_key) DO NOTHING`,
			j.JobKey, j.Owner, j.Repo, j.PRNumber, j.Type, j.DeliveryID,
			j.Origin, payload, j.PendingCommentID, d.instance)
	} else if err != nil {
		slog.Error("dedup bindcancel: read", "error", err)
		return false
	} else if superseded || (deliveryID != "" && j.DeliveryID != "" && deliveryID != j.DeliveryID) {
		return false
	}

	// The worker binding is the process that runs the job — take claim
	// ownership so THIS replica's watcher sees supersede flags for it.
	// (The TryStart replica only held the claim across the enqueue window.)
	// The delivery guard mirrors Done: never steal a claim that was just
	// replaced by a different delivery between our SELECT and this UPDATE.
	_, _ = d.pool.Exec(context.Background(), `
		UPDATE job_claims SET instance = $2
		WHERE job_key = $1 AND (delivery_id = '' OR $3 = '' OR delivery_id = $3)`,
		j.JobKey, d.instance, j.DeliveryID)

	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.running[j.JobKey]
	if !ok {
		st = &localState{job: j}
		d.running[j.JobKey] = st
	}
	st.cancels = append(st.cancels, cancel)
	return true
}

// Done releases the claim. A superseded instance must not clear the
// replacement's claim, so the delete is scoped to the stored delivery (or
// empty-delivery claims, matching the in-memory rules).
func (d *PostgresDedup) Done(j domain.Review) {
	_, _ = d.pool.Exec(context.Background(), `
		DELETE FROM job_claims
		WHERE job_key = $1 AND (delivery_id = '' OR $2 = '' OR delivery_id = $2)`,
		j.JobKey, j.DeliveryID)

	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.running[j.JobKey]; ok {
		if st.job.DeliveryID == "" || j.DeliveryID == "" || st.job.DeliveryID == j.DeliveryID {
			for _, c := range st.cancels {
				c()
			}
			delete(d.running, j.JobKey)
		}
	}
}

// Active returns this instance's in-flight job count.
func (d *PostgresDedup) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

// UpdateJob mutates the stored payload of an in-flight claim so same-key
// supersede rules see the current type (e.g. a gate verdict turned the job
// into a review).
func (d *PostgresDedup) UpdateJob(key string, mutate func(*domain.Review)) {
	tx, err := d.pool.Begin(context.Background())
	if err != nil {
		return
	}
	defer tx.Rollback(context.Background())

	var payload []byte
	if err := tx.QueryRow(context.Background(),
		`SELECT payload FROM job_claims WHERE job_key = $1 FOR UPDATE`, key).Scan(&payload); err != nil {
		return
	}
	var j domain.Review
	if err := json.Unmarshal(payload, &j); err != nil {
		return
	}
	mutate(&j)
	payload, _ = json.Marshal(j)
	_, _ = tx.Exec(context.Background(), `
		UPDATE job_claims SET payload = $2, job_type = $3, delivery_id = $4,
			origin = $5, pending_comment_id = $6
		WHERE job_key = $1`,
		key, payload, j.Type, j.DeliveryID, j.Origin, j.PendingCommentID)
	_ = tx.Commit(context.Background())

	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.running[key]; ok {
		st.job = j
	}
}

// watch polls this instance's claims and cancels local jobs whose claim was
// superseded, replaced (same key, different delivery), or deleted. Also
// sweeps superseded rows their (possibly dead) owners never cleaned up.
func (d *PostgresDedup) watch(ctx context.Context) {
	ticker := time.NewTicker(d.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.reconcile(ctx)
		}
	}
}

func (d *PostgresDedup) reconcile(ctx context.Context) {
	type claimRow struct {
		key        string
		deliveryID string
		superseded bool
	}
	rows, err := d.pool.Query(ctx,
		`SELECT job_key, delivery_id, superseded FROM job_claims WHERE instance = $1`,
		d.instance)
	if err != nil {
		return
	}
	claims := make(map[string]claimRow)
	for rows.Next() {
		var c claimRow
		if rows.Scan(&c.key, &c.deliveryID, &c.superseded) == nil {
			claims[c.key] = c
		}
	}
	rows.Close()

	d.mu.Lock()
	defer d.mu.Unlock()
	for key, st := range d.running {
		c, ok := claims[key]
		lost := !ok || c.superseded ||
			(c.deliveryID != "" && st.job.DeliveryID != "" && c.deliveryID != st.job.DeliveryID)
		if !lost {
			continue
		}
		for _, cancel := range st.cancels {
			cancel()
		}
		delete(d.running, key)
		if ok && c.superseded {
			// We observed the flag — clean up our own claim row.
			_, _ = d.pool.Exec(context.Background(),
				`DELETE FROM job_claims WHERE job_key = $1 AND superseded`, key)
		}
	}

	// Owners that crashed leave superseded rows behind; bound them.
	_, _ = d.pool.Exec(ctx, `
		DELETE FROM job_claims
		WHERE superseded AND superseded_at < now() - interval '10 minutes'`)
}
