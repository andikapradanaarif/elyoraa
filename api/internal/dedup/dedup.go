package dedup

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/andikapradanaarif/elyoraa/internal/job"
)

// Dedup tracks in-flight review jobs and handles cancel-and-replace.
type Dedup struct {
	mu      sync.Mutex
	running map[string]*state
}

type state struct {
	job    *job.Review
	cancel context.CancelFunc
}

// New creates a new dedup tracker.
func New() *Dedup {
	return &Dedup{running: make(map[string]*state)}
}

// TryStart registers a job. It returns the job to run, a context, and true if the job should proceed.
// If a job with the same key is already running, it returns false (skip).
// If a job for the same PR but a different SHA is running, it cancels the old one and copies the
// pending comment ID to the new job.
func (d *Dedup) TryStart(parent context.Context, j job.Review) (context.Context, *job.Review, bool) {
	prKey := fmt.Sprintf("%s/%s/%d", j.Owner, j.Repo, j.PRNumber)

	d.mu.Lock()
	defer d.mu.Unlock()

	if existing, ok := d.running[j.JobKey]; ok {
		_ = existing
		return nil, nil, false
	}

	// Replace any running job for the same PR.
	for key, st := range d.running {
		if strings.HasPrefix(key, prKey+"/") && key != j.JobKey {
			if j.PendingCommentID == 0 && st.job != nil {
				j.PendingCommentID = st.job.PendingCommentID
			}
			st.cancel()
			delete(d.running, key)
		}
	}

	ctx, cancel := context.WithCancel(parent)
	d.running[j.JobKey] = &state{job: &j, cancel: cancel}
	return ctx, &j, true
}

// Done marks a job as complete and cancels its internal context if it is still tracked.
func (d *Dedup) Done(jobKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if st, ok := d.running[jobKey]; ok {
		st.cancel()
		delete(d.running, jobKey)
	}
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

// parsePRKey is a helper to determine if two keys are for the same PR.
func parsePRKey(jobKey string) string {
	parts := strings.Split(jobKey, "/")
	if len(parts) < 4 {
		return jobKey
	}
	return strings.Join(parts[:3], "/")
}
