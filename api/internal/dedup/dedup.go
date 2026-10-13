package dedup

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

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
		// A review request supersedes a queued/running split proposal for the
		// same SHA — an explicit "review anyway" must not be dropped as a
		// duplicate while the gate's proposal is still pending. No key-level
		// tombstone is set: the superseded instance is rejected by the
		// delivery-ID check in BindCancel, and the new job must still bind.
		if st.job == nil || !st.job.IsSplit() || j.IsSplit() || j.IsIssue() {
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

	// Replace any running job for the same PR.
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

// JobKey returns the canonical key for a job.
func JobKey(owner, repo string, prNumber int, headSHA string) string {
	return fmt.Sprintf("%s/%s/%d/%s", owner, repo, prNumber, headSHA)
}
