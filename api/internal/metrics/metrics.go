package metrics

import (
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// Registry holds a small set of Prometheus-compatible counters.
type Registry struct {
	mu            sync.RWMutex
	queueDepth    int64
	activeReviews int64
	reviewsTotal  uint64
	reviewErrors  uint64
	droppedJobs   uint64
	durationSum   float64
	durationCount uint64
}

func New() *Registry {
	return &Registry{}
}

func (r *Registry) IncQueueDepth() {
	atomic.AddInt64(&r.queueDepth, 1)
}

func (r *Registry) DecQueueDepth() {
	atomic.AddInt64(&r.queueDepth, -1)
}

func (r *Registry) IncActiveReviews() {
	atomic.AddInt64(&r.activeReviews, 1)
}

func (r *Registry) DecActiveReviews() {
	atomic.AddInt64(&r.activeReviews, -1)
}

func (r *Registry) IncReviewTotal() {
	atomic.AddUint64(&r.reviewsTotal, 1)
}

func (r *Registry) IncReviewErrors() {
	atomic.AddUint64(&r.reviewErrors, 1)
}

func (r *Registry) IncDroppedJobs() {
	atomic.AddUint64(&r.droppedJobs, 1)
}

func (r *Registry) RecordDuration(d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durationSum += d.Seconds()
	r.durationCount++
}

func (r *Registry) Handler(w http.ResponseWriter, _ *http.Request) {
	qd := atomic.LoadInt64(&r.queueDepth)
	ar := atomic.LoadInt64(&r.activeReviews)
	rt := atomic.LoadUint64(&r.reviewsTotal)
	err := atomic.LoadUint64(&r.reviewErrors)
	dj := atomic.LoadUint64(&r.droppedJobs)

	r.mu.RLock()
	sum := r.durationSum
	count := r.durationCount
	r.mu.RUnlock()

	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintln(w, "# HELP elyoraa_queue_depth Current number of jobs in the queue")
	fmt.Fprintln(w, "# TYPE elyoraa_queue_depth gauge")
	fmt.Fprintf(w, "elyoraa_queue_depth %d\n", qd)

	fmt.Fprintln(w, "# HELP elyoraa_active_reviews Number of reviews currently running")
	fmt.Fprintln(w, "# TYPE elyoraa_active_reviews gauge")
	fmt.Fprintf(w, "elyoraa_active_reviews %d\n", ar)

	fmt.Fprintln(w, "# HELP elyoraa_reviews_total Total number of reviews attempted")
	fmt.Fprintln(w, "# TYPE elyoraa_reviews_total counter")
	fmt.Fprintf(w, "elyoraa_reviews_total %d\n", rt)

	fmt.Fprintln(w, "# HELP elyoraa_review_errors_total Total number of review errors")
	fmt.Fprintln(w, "# TYPE elyoraa_review_errors_total counter")
	fmt.Fprintf(w, "elyoraa_review_errors_total %d\n", err)

	fmt.Fprintln(w, "# HELP elyoraa_dropped_jobs_total Total number of jobs dropped due to full queue")
	fmt.Fprintln(w, "# TYPE elyoraa_dropped_jobs_total counter")
	fmt.Fprintf(w, "elyoraa_dropped_jobs_total %d\n", dj)

	fmt.Fprintln(w, "# HELP elyoraa_review_duration_seconds_sum Sum of review durations in seconds")
	fmt.Fprintln(w, "# TYPE elyoraa_review_duration_seconds_sum counter")
	fmt.Fprintf(w, "elyoraa_review_duration_seconds_sum %g\n", sum)

	fmt.Fprintln(w, "# HELP elyoraa_review_duration_seconds_count Count of review durations")
	fmt.Fprintln(w, "# TYPE elyoraa_review_duration_seconds_count counter")
	fmt.Fprintf(w, "elyoraa_review_duration_seconds_count %d\n", count)
}
