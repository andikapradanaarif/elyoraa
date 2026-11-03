package queue

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	bolt "go.etcd.io/bbolt"
)

// Job statuses recorded in the tracker.
const (
	JobStatusQueued     = "queued"
	JobStatusProcessing = "processing"
	JobStatusCompleted  = "completed"
	JobStatusFailed     = "failed"
	JobStatusCancelled  = "cancelled"
)

const (
	// outcomesBucket stores one JobRecord per job key.
	outcomesBucket = "outcomes"
	// maxJobRecords bounds how many outcome records are retained.
	maxJobRecords = 2000
)

// JobRecord is a persisted record of a review job's lifecycle. The full job
// payload is stored so a failed job can be retried without a new webhook.
type JobRecord struct {
	JobKey     string   `json:"job_key"`
	DeliveryID string   `json:"delivery_id,omitempty"`
	Type       string   `json:"type,omitempty"`
	Owner      string   `json:"owner"`
	Repo       string   `json:"repo"`
	PRNumber   int      `json:"pr_number"`
	Status     string   `json:"status"`
	Error      string   `json:"error,omitempty"`
	Attempts   int      `json:"attempts"`
	Actions    []string `json:"actions,omitempty"` // respond job: action types the agent declared
	// RefinedLines is the generated-excluded line count measured after GetPR.
	// Kept separate from Job.SizeLines (the enqueue-time payload estimate) so
	// a manual retry rescales from the same base the webhook recorded.
	RefinedLines int           `json:"refined_lines,omitempty"`
	Job          domain.Review `json:"-"` // stored for manual retry
	EnqueuedAt   time.Time     `json:"enqueued_at"`
	StartedAt    *time.Time    `json:"started_at,omitempty"`
	FinishedAt   *time.Time    `json:"finished_at,omitempty"`
	// DurationSeconds is execution time (pickup → finish), excluding queue
	// wait. Populated on completion for rate calibration.
	DurationSeconds int64 `json:"duration_seconds,omitempty"`
	// AttemptSeconds is the duration of the final successful Devin attempt —
	// retry backoff excluded. Calibration prefers it over DurationSeconds.
	AttemptSeconds int64     `json:"attempt_seconds,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// jobRecordWire is the on-disk representation including the full job payload.
type jobRecordWire struct {
	JobRecord
	Job domain.Review `json:"job"`
}

// JobTracker records job lifecycle events so outcomes can be inspected and
// failed jobs retried later.
type JobTracker interface {
	// Enqueued records a job that was accepted into the queue.
	Enqueued(ctx context.Context, j domain.Review)
	// Started records that a worker picked the job up.
	Started(ctx context.Context, j domain.Review)
	// Completed records a successful finish with the number of attempts used.
	Completed(ctx context.Context, j domain.Review, attempts int)
	// Cancelled records a job that was superseded or otherwise cancelled.
	Cancelled(ctx context.Context, j domain.Review, attempts int)
	// Failed records a terminal failure with the error and attempt count.
	Failed(ctx context.Context, j domain.Review, attempts int, err error)
	// Annotate writes metadata discovered during processing — declared
	// actions, refined size — onto an existing record. Process can't write
	// these itself: it receives a copy of the job, not the queue's instance.
	Annotate(ctx context.Context, key string, a JobAnnotation)
	// List returns all recorded jobs, most recently updated first.
	List(ctx context.Context) []JobRecord
	// Get returns a single job record by key.
	Get(ctx context.Context, key string) (JobRecord, bool)
	// StuckProcessing returns records of jobs believed dead — safe to mark
	// failed at startup. Single-process backends report every "processing"
	// record; the Postgres backend only reports records whose queue row is
	// gone, since a live row belongs to some replica.
	StuckProcessing(ctx context.Context) []JobRecord
}

// JobAnnotation carries job metadata discovered during processing — declared
// actions, refined size, phase transitions, attempt timing — written to the
// record by the worker.
type JobAnnotation struct {
	Actions      []string
	RefinedLines int
	// Type updates the record's job type — a gate that triaged to "review"
	// must read as a review for audit and rate calibration.
	Type string
	// AttemptSeconds records the final successful Devin attempt duration so
	// calibration measures work time, not retry/backoff wall time.
	AttemptSeconds int64
}

// NewJobTracker returns a persistent tracker when the backend is BoltDB-backed,
// otherwise a bounded in-memory tracker.
func NewJobTracker(b Backend) JobTracker {
	if bb, ok := b.(*BoltDBBackend); ok {
		if t, err := NewBoltJobTracker(bb.db); err == nil && t != nil {
			return t
		}
	}
	if pb, ok := b.(*PostgresBackend); ok {
		return NewPostgresJobTracker(pb.pool, pb.instance)
	}
	return NewMemoryJobTracker(maxJobRecords)
}

// MemoryJobTracker keeps job records in memory with a bounded size.
type MemoryJobTracker struct {
	mu      sync.Mutex
	records map[string]*JobRecord
	order   []string // insertion order for eviction
	max     int
}

// NewMemoryJobTracker creates an in-memory tracker keeping at most max records.
func NewMemoryJobTracker(max int) *MemoryJobTracker {
	if max <= 0 {
		max = 1000
	}
	return &MemoryJobTracker{records: make(map[string]*JobRecord), max: max}
}

func (m *MemoryJobTracker) upsert(j domain.Review, mutate func(*JobRecord)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[j.JobKey]
	if !ok {
		rec = &JobRecord{
			JobKey:     j.JobKey,
			DeliveryID: j.DeliveryID,
			Type:       j.Type,
			Owner:      j.Owner,
			Repo:       j.Repo,
			PRNumber:   j.PRNumber,
			EnqueuedAt: j.EnqueuedAt,
			Job:        j,
		}
		m.records[j.JobKey] = rec
		m.order = append(m.order, j.JobKey)
		for len(m.order) > m.max {
			oldest := m.order[0]
			m.order = m.order[1:]
			delete(m.records, oldest)
		}
	}
	mutate(rec)
	rec.UpdatedAt = time.Now()
}

func (m *MemoryJobTracker) Enqueued(_ context.Context, j domain.Review) {
	m.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusQueued
		r.Error = ""
		r.Job = j
		r.DeliveryID = j.DeliveryID
		r.Type = j.Type
	})
}

func (m *MemoryJobTracker) Started(_ context.Context, j domain.Review) {
	m.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusProcessing
		now := time.Now()
		r.StartedAt = &now
	})
}

func (m *MemoryJobTracker) Completed(_ context.Context, j domain.Review, attempts int) {
	m.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusCompleted
		r.Error = ""
		r.Attempts = attempts
		now := time.Now()
		r.FinishedAt = &now
		if r.StartedAt != nil {
			r.DurationSeconds = int64(now.Sub(*r.StartedAt).Seconds())
		}
	})
}

func (m *MemoryJobTracker) Cancelled(_ context.Context, j domain.Review, attempts int) {
	m.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusCancelled
		r.Attempts = attempts
		now := time.Now()
		r.FinishedAt = &now
	})
}

func (m *MemoryJobTracker) Failed(_ context.Context, j domain.Review, attempts int, err error) {
	m.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusFailed
		r.Attempts = attempts
		if err != nil {
			r.Error = err.Error()
		}
		now := time.Now()
		r.FinishedAt = &now
	})
}

func (m *MemoryJobTracker) Annotate(_ context.Context, key string, a JobAnnotation) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec, ok := m.records[key]
	if !ok {
		return
	}
	if len(a.Actions) > 0 {
		rec.Actions = a.Actions
	}
	if a.RefinedLines > 0 {
		rec.RefinedLines = a.RefinedLines
	}
	if a.Type != "" {
		rec.Type = a.Type
		rec.Job.Type = a.Type
	}
	if a.AttemptSeconds > 0 {
		rec.AttemptSeconds = a.AttemptSeconds
	}
	rec.UpdatedAt = time.Now()
}

func (m *MemoryJobTracker) List(_ context.Context) []JobRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]JobRecord, 0, len(m.records))
	for _, r := range m.records {
		out = append(out, *r)
	}
	sortRecords(out)
	return out
}

func (m *MemoryJobTracker) Get(_ context.Context, key string) (JobRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[key]
	if !ok {
		return JobRecord{}, false
	}
	return *r, true
}

// StuckProcessing reports every "processing" record — a memory tracker only
// exists in one process, so anything processing at boot was killed with it.
func (m *MemoryJobTracker) StuckProcessing(ctx context.Context) []JobRecord {
	var out []JobRecord
	for _, r := range m.List(ctx) {
		if r.Status == JobStatusProcessing {
			out = append(out, r)
		}
	}
	return out
}

// BoltJobTracker persists job records in the same BoltDB file as the queue.
type BoltJobTracker struct {
	db *bolt.DB
}

// NewBoltJobTracker creates a tracker on the given DB, creating the bucket.
func NewBoltJobTracker(db *bolt.DB) (*BoltJobTracker, error) {
	if db == nil {
		return nil, nil
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(outcomesBucket))
		return err
	}); err != nil {
		return nil, err
	}
	return &BoltJobTracker{db: db}, nil
}

func (t *BoltJobTracker) upsert(j domain.Review, mutate func(*JobRecord)) {
	_ = t.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(outcomesBucket))
		rec := JobRecord{
			JobKey:     j.JobKey,
			DeliveryID: j.DeliveryID,
			Type:       j.Type,
			Owner:      j.Owner,
			Repo:       j.Repo,
			PRNumber:   j.PRNumber,
			EnqueuedAt: j.EnqueuedAt,
			Job:        j,
		}
		if v := bkt.Get([]byte(j.JobKey)); v != nil {
			var w jobRecordWire
			if err := json.Unmarshal(v, &w); err == nil {
				rec = w.JobRecord
				rec.Job = w.Job
			}
		}
		mutate(&rec)
		rec.UpdatedAt = time.Now()
		data, err := json.Marshal(jobRecordWire{JobRecord: rec, Job: rec.Job})
		if err != nil {
			return err
		}
		return bkt.Put([]byte(j.JobKey), data)
	})
	t.prune()
}

func (t *BoltJobTracker) Enqueued(_ context.Context, j domain.Review) {
	t.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusQueued
		r.Error = ""
		r.Job = j
		r.DeliveryID = j.DeliveryID
		r.Type = j.Type
	})
}

func (t *BoltJobTracker) Started(_ context.Context, j domain.Review) {
	t.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusProcessing
		now := time.Now()
		r.StartedAt = &now
	})
}

func (t *BoltJobTracker) Completed(_ context.Context, j domain.Review, attempts int) {
	t.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusCompleted
		r.Error = ""
		r.Attempts = attempts
		now := time.Now()
		r.FinishedAt = &now
		if r.StartedAt != nil {
			r.DurationSeconds = int64(now.Sub(*r.StartedAt).Seconds())
		}
	})
}

func (t *BoltJobTracker) Cancelled(_ context.Context, j domain.Review, attempts int) {
	t.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusCancelled
		r.Attempts = attempts
		now := time.Now()
		r.FinishedAt = &now
	})
}

func (t *BoltJobTracker) Failed(_ context.Context, j domain.Review, attempts int, err error) {
	t.upsert(j, func(r *JobRecord) {
		r.Status = JobStatusFailed
		r.Attempts = attempts
		if err != nil {
			r.Error = err.Error()
		}
		now := time.Now()
		r.FinishedAt = &now
	})
}

func (t *BoltJobTracker) Annotate(_ context.Context, key string, a JobAnnotation) {
	_ = t.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(outcomesBucket))
		v := bkt.Get([]byte(key))
		if v == nil {
			return nil
		}
		var w jobRecordWire
		if err := json.Unmarshal(v, &w); err != nil {
			return nil
		}
		if len(a.Actions) > 0 {
			w.Actions = a.Actions
		}
		if a.RefinedLines > 0 {
			w.RefinedLines = a.RefinedLines
		}
		if a.Type != "" {
			w.Type = a.Type
			w.Job.Type = a.Type
		}
		if a.AttemptSeconds > 0 {
			w.AttemptSeconds = a.AttemptSeconds
		}
		w.UpdatedAt = time.Now()
		data, err := json.Marshal(w)
		if err != nil {
			return err
		}
		return bkt.Put([]byte(key), data)
	})
}

func (t *BoltJobTracker) List(_ context.Context) []JobRecord {
	var out []JobRecord
	_ = t.db.View(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(outcomesBucket))
		return bkt.ForEach(func(_, v []byte) error {
			var w jobRecordWire
			if err := json.Unmarshal(v, &w); err == nil {
				rec := w.JobRecord
				rec.Job = w.Job
				out = append(out, rec)
			}
			return nil
		})
	})
	sortRecords(out)
	return out
}

func (t *BoltJobTracker) Get(_ context.Context, key string) (JobRecord, bool) {
	var rec JobRecord
	found := false
	_ = t.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(outcomesBucket)).Get([]byte(key))
		if v == nil {
			return nil
		}
		var w jobRecordWire
		if err := json.Unmarshal(v, &w); err == nil {
			rec = w.JobRecord
			rec.Job = w.Job
			found = true
		}
		return nil
	})
	return rec, found
}

// StuckProcessing reports records whose job can never finish: "processing"
// or "queued" records with no live queue row. A record whose row survived —
// still queued, or reclaimed from "inflight" at open — re-runs on its own
// and must be left alone. Bolt is single-process, so a missing row at boot
// means the job is gone for good (same rule as the Postgres backend).
func (t *BoltJobTracker) StuckProcessing(ctx context.Context) []JobRecord {
	var out []JobRecord
	_ = t.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(outcomesBucket)).ForEach(func(_, v []byte) error {
			var w jobRecordWire
			if err := json.Unmarshal(v, &w); err != nil {
				return nil
			}
			if w.Status != JobStatusProcessing && w.Status != JobStatusQueued {
				return nil
			}
			if queueRowExists(tx, w.JobKey) {
				return nil
			}
			rec := w.JobRecord
			rec.Job = w.Job
			out = append(out, rec)
			return nil
		})
	})
	sortRecords(out)
	return out
}

// queueRowExists reports whether a queue row (queued or in-flight) still
// carries this job key — the job re-runs on its own, so its record is not
// stuck. Rows are few enough that scanning payloads is cheap.
func queueRowExists(tx *bolt.Tx, jobKey string) bool {
	for _, name := range []string{jobsBucket, inflightBucket} {
		found := false
		if err := tx.Bucket([]byte(name)).ForEach(func(_, v []byte) error {
			var stored domain.Review
			if err := json.Unmarshal(v, &stored); err == nil && stored.JobKey == jobKey {
				found = true
			}
			return nil
		}); err == nil && found {
			return true
		}
	}
	return false
}

// prune deletes the oldest records once the bucket exceeds the cap.
func (t *BoltJobTracker) prune() {
	_ = t.db.Update(func(tx *bolt.Tx) error {
		bkt := tx.Bucket([]byte(outcomesBucket))
		if bkt.Stats().KeyN <= maxJobRecords {
			return nil
		}
		type kv struct {
			key     []byte
			updated time.Time
		}
		var all []kv
		_ = bkt.ForEach(func(k, v []byte) error {
			var w jobRecordWire
			if err := json.Unmarshal(v, &w); err == nil {
				kk := make([]byte, len(k))
				copy(kk, k)
				all = append(all, kv{key: kk, updated: w.UpdatedAt})
			}
			return nil
		})
		sort.Slice(all, func(i, j int) bool { return all[i].updated.Before(all[j].updated) })
		excess := len(all) - maxJobRecords
		for i := 0; i < excess; i++ {
			_ = bkt.Delete(all[i].key)
		}
		return nil
	})
}

func sortRecords(rs []JobRecord) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].UpdatedAt.After(rs[j].UpdatedAt) })
}
