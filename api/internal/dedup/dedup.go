package dedup

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

// Interface is the dedup contract. Dedup is the single-process
// implementation; PostgresDedup the multi-replica one (QUEUE_BACKEND=
// postgres).
type Interface interface {
	// TryStart registers the job and returns its context; false skips it.
	TryStart(parent context.Context, j domain.Review) (context.Context, *domain.Review, bool)
	// BindCancel attaches a cancel func to a claimed job; false means the
	// job lost its slot while queued and must be skipped.
	BindCancel(j domain.Review, cancel context.CancelFunc) bool
	// Done releases the job's claim.
	Done(j domain.Review)
	// Active returns the in-flight count.
	Active() int
	// UpdateJob mutates the stored copy of an in-flight job.
	UpdateJob(key string, mutate func(*domain.Review))
}

// Dedup tracks in-flight review jobs and handles cancel-and-replace.
type Dedup struct {
	mu       sync.Mutex
	running  map[string]*state
	replaced map[string]struct{} // tombstones for superseded keys
}

type state struct {
	job     *domain.Review
	cancels []context.CancelFunc
}

// New creates a new dedup tracker.
func New() *Dedup {
	return &Dedup{running: make(map[string]*state), replaced: make(map[string]struct{})}
}

// TryStart registers a job. It returns the job to run, a context, and true if the job should proceed.
// If a job with the same key is already running, it returns false (skip).
// If a job for the same PR but a different SHA is running, it cancels the old one and copies the
// pending comment ID to the new job.
func (d *Dedup) TryStart(parent context.Context, j domain.Review) (context.Context, *domain.Review, bool) {
	prKey := fmt.Sprintf("%s/%s/%d", j.Owner, j.Repo, j.PRNumber)

	d.mu.Lock()
	defer d.mu.Unlock()

	if st, ok := d.running[j.JobKey]; ok {
		// Supersedes share a key but carry newer intent — see canSupersede.
		// No key-level tombstone is set: the superseded instance is rejected
		// by the delivery-ID check in BindCancel, and the new job must still
		// bind.
		if st.job == nil || !canSupersede(st.job, &j) {
			return nil, nil, false
		}
		if j.PendingCommentID == 0 {
			j.PendingCommentID = st.job.PendingCommentID
		}
		for _, c := range st.cancels {
			c()
		}
		delete(d.running, j.JobKey)
	}
	// Clear any tombstone for this exact key so a manual retry can bind again.
	delete(d.replaced, j.JobKey)

	// Replace any running job for the same PR. Respond jobs skip this loop:
	// they live outside the review keyspace and must never cancel a running
	// review (two consecutive mentions on one PR must both run).
	if !j.IsRespond() {
		for key, st := range d.running {
			if strings.HasPrefix(key, prKey+"/") && key != j.JobKey {
				if j.PendingCommentID == 0 && st.job != nil {
					j.PendingCommentID = st.job.PendingCommentID
				}
				for _, c := range st.cancels {
					c()
				}
				delete(d.running, key)
				d.replaced[key] = struct{}{}
			}
		}
	}

	ctx, cancel := context.WithCancel(parent)
	d.running[j.JobKey] = &state{job: &j, cancels: []context.CancelFunc{cancel}}
	return ctx, &j, true
}

// BindCancel attaches a cancel func to a job so a later replacement can
// terminate the in-flight work. Returns false if the job was superseded while
// queued — the worker should skip it. Jobs unknown to the tracker (e.g.
// recovered from a durable queue after restart) get a fresh state so they can
// still be cancelled by a later replacement.
func (d *Dedup) BindCancel(j domain.Review, cancel context.CancelFunc) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.replaced[j.JobKey]; ok {
		return false
	}
	st, ok := d.running[j.JobKey]
	if ok && st.job != nil && st.job.DeliveryID != "" && j.DeliveryID != "" && st.job.DeliveryID != j.DeliveryID {
		// The slot was taken over by a different job instance (same-key
		// supersede) — this stale instance must not bind or run.
		return false
	}
	if !ok {
		st = &state{}
		d.running[j.JobKey] = st
	}
	st.cancels = append(st.cancels, cancel)
	return true
}

// Done marks a job as complete and cancels its internal contexts if it is still tracked.
// A superseded instance must not clear the superseding job's slot, so the
// running state is only removed when the delivery IDs match (or are unknown).
func (d *Dedup) Done(j domain.Review) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.running[j.JobKey]; ok {
		if st.job != nil && st.job.DeliveryID != "" && j.DeliveryID != "" && st.job.DeliveryID != j.DeliveryID {
			return // slot owned by a superseding job — leave its state untouched
		}
		for _, c := range st.cancels {
			c()
		}
		delete(d.running, j.JobKey)
	}
	delete(d.replaced, j.JobKey)
}

// Active returns the number of in-flight jobs.
func (d *Dedup) Active() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.running)
}

// canSupersede reports whether n may replace the in-flight job cur that shares
// its key. The rule is one origin-strength table: a job may supersede another
// iff its origin is stronger-or-equal and it is not a literal redelivery
// (delivery IDs differ). Automated work (rank 0) never supersedes — so an
// enqueue-time split can never kill a review a gate already committed to,
// which is why gate transitions sync the slot via UpdateJob.
func canSupersede(cur, n *domain.Review) bool {
	// A redelivery carries the same delivery ID — a duplicate, never a
	// supersede. Without this the superseded instance's Done() could cancel
	// the replacement's slot (both must be non-empty: dispatched jobs and
	// manual retries legitimately share ""-adjacent space).
	if cur.DeliveryID != "" && n.DeliveryID != "" && cur.DeliveryID == n.DeliveryID {
		return false
	}
	if cur.IsRespond() || n.IsRespond() {
		// Respond keys are trigger-scoped: a comment edit reuses the trigger
		// ID but gets a fresh delivery ID, so the newer text wins. A
		// redelivery was already filtered above.
		return cur.IsRespond() && n.IsRespond()
	}
	if cur.IsIssue() || n.IsIssue() {
		return false
	}
	nRank, curRank := n.IntentRank(), cur.IntentRank()
	if nRank == 0 {
		return false
	}
	// Stronger intent wins. Equal rank only wins across a type change (the
	// human redirected intent — e.g. a dispatched review after a dispatched
	// split); equal rank and same type is a duplicate of intent, and
	// restarting a job mid-side-effects (a second acceptance killing a
	// running issue-creation) risks duplicate work.
	return nRank > curRank || (nRank == curRank && n.Type != cur.Type)
}

// UpdateJob mutates the stored copy of an in-flight job — used when a job
// transitions phases (e.g. a gate verdict turns it into a review or split)
// so same-key supersede rules see the current type, not the entry-time one.
func (d *Dedup) UpdateJob(key string, mutate func(*domain.Review)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.running[key]; ok && st.job != nil {
		mutate(st.job)
	}
}

// JobKey returns the canonical key for a job.
func JobKey(owner, repo string, prNumber int, headSHA string) string {
	return fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, headSHA)
}

// RespondKey returns the job key for a mention-routing (respond) job, keyed by
// the trigger ID (comment/review/issue ID from the payload — stable across
// redelivery). It lives outside the review keyspace: consecutive mentions on
// one PR each get a slot, and a respond job can never collide with a review.
func RespondKey(owner, repo string, number int, triggerID int64) string {
	return fmt.Sprintf("respond/%s/%s/%d/%d", owner, repo, number, triggerID)
}
