package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
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
	// labeled maps metric name → "k=v,k=v" label set → count. Used for
	// elyoraa_jobs_total{job_type,outcome}, elyoraa_respond_actions_total
	// {action}, and elyoraa_gate_verdicts_total{verdict}.
	labeled map[string]map[string]uint64
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

// IncJobOutcome records a terminal job outcome by type:
// elyoraa_jobs_total{job_type="review",outcome="completed"}.
func (r *Registry) IncJobOutcome(jobType, outcome string) {
	r.incLabeled("elyoraa_jobs_total", "job_type", jobType, "outcome", outcome)
}

// IncRespondAction records a respond-agent declared action:
// elyoraa_respond_actions_total{action="request_review"}.
func (r *Registry) IncRespondAction(action string) {
	r.incLabeled("elyoraa_respond_actions_total", "action", action)
}

// IncGateVerdict records a gate triage verdict:
// elyoraa_gate_verdicts_total{verdict="review"}.
func (r *Registry) IncGateVerdict(verdict string) {
	r.incLabeled("elyoraa_gate_verdicts_total", "verdict", verdict)
}

// IncReaction records a reaction left on one of the bot's comments:
// elyoraa_reactions_total{reaction="+1",action="created"}.
func (r *Registry) IncReaction(reaction, action string) {
	if reaction == "" {
		reaction = "unknown"
	}
	if action == "" {
		action = "unknown"
	}
	r.incLabeled("elyoraa_reactions_total", "reaction", reaction, "action", action)
}

func (r *Registry) incLabeled(name string, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.labeled == nil {
		r.labeled = make(map[string]map[string]uint64)
	}
	set := r.labeled[name]
	if set == nil {
		set = make(map[string]uint64)
		r.labeled[name] = set
	}
	pairs := make([]string, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		pairs = append(pairs, fmt.Sprintf(`%s=%q`, kv[i], kv[i+1]))
	}
	set[strings.Join(pairs, ",")]++
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

	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.labeled))
	for name := range r.labeled {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		set := r.labeled[name]
		keys := make([]string, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		for _, k := range keys {
			fmt.Fprintf(w, "%s{%s} %d\n", name, k, set[k])
		}
	}
}
