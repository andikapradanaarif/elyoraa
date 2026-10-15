package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
)

// ErrJobCancelled is returned by a Processor when a job was superseded or
// otherwise cancelled mid-flight. It is not a failure.
var ErrJobCancelled = errors.New("job cancelled")

// Processor handles a single review job. It returns the number of attempts
// used so far (for jobs with internal retries) and an error, if any.
type Processor interface {
	Process(ctx context.Context, job domain.Review) (attempts int, err error)
}

// ProcessorFunc adapts a function to Processor.
type ProcessorFunc func(ctx context.Context, job domain.Review) (int, error)

func (f ProcessorFunc) Process(ctx context.Context, job domain.Review) (int, error) {
	return f(ctx, job)
}

// Queue dispatches review jobs to a pool of workers backed by a Backend.
type Queue struct {
	backend   Backend
	workers   int
	processor Processor
	metrics   *metrics.Registry
	tracker   JobTracker
	stopping  atomic.Bool
	wg        sync.WaitGroup
}

// New creates a queue with an in-memory backend.
func New(workers int, p Processor, m *metrics.Registry, capacity int) *Queue {
	return NewWithBackend(workers, p, m, NewMemoryBackend(capacity))
}

// NewWithBackend creates a queue with the supplied backend.
func NewWithBackend(workers int, p Processor, m *metrics.Registry, b Backend) *Queue {
	return &Queue{
		backend:   b,
		workers:   workers,
		processor: p,
		metrics:   m,
		tracker:   NewJobTracker(b),
	}
}

// Tracker returns the job outcome tracker backing this queue.
func (q *Queue) Tracker() JobTracker {
	return q.tracker
}

// Submit enqueues a job. Returns false if the queue is full, draining, and
// the job is dropped.
func (q *Queue) Submit(ctx context.Context, j domain.Review) bool {
	if q.stopping.Load() {
		if q.metrics != nil {
			q.metrics.IncDroppedJobs()
		}
		return false
	}
	if !q.backend.Put(ctx, j) {
		if q.metrics != nil {
			q.metrics.IncDroppedJobs()
		}
		return false
	}
	if q.tracker != nil {
		q.tracker.Enqueued(ctx, j)
	}
	if q.metrics != nil {
		q.metrics.IncQueueDepth()
	}
	slog.Info("job enqueued",
		"job", j.JobKey,
		"delivery", j.DeliveryID,
		"owner", j.Owner,
		"repo", j.Repo,
		"pr", j.PRNumber,
		"type", j.Type,
	)
	return true
}

// Start launches the worker pool. It blocks until the context is cancelled,
// waits for workers to unwind, then releases the backend.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx, i)
	}
	<-ctx.Done()
	// Workers share ctx with us — they unwind on cancellation. Wait so their
	// final tracker writes land before shared resources (Bolt) close.
	q.wg.Wait()
	_ = q.backend.Close()
}

// Stop gracefully drains the pool: new submissions are rejected and the
// backend stops handing out jobs (queued jobs persist on the Bolt backend
// for the next boot), then it waits for in-flight jobs — and so workers —
// to finish until ctx expires, keeping shared handles (e.g. the Bolt tracker)
// alive so outcomes still record. The backend is fully closed last. The
// caller should still cancel the Start context afterwards to kill anything
// that outlived the drain.
func (q *Queue) Stop(ctx context.Context) {
	q.stopping.Store(true)
	if s, ok := q.backend.(StoppableBackend); ok {
		s.SignalStop()
	} else {
		_ = q.backend.Close()
	}
	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		slog.Warn("queue drain timed out; in-flight jobs will be cancelled")
	case <-done:
		slog.Info("queue drained")
	}
	_ = q.backend.Close()
}

func (q *Queue) worker(ctx context.Context, id int) {
	defer q.wg.Done()
	for {
		j, ok := q.backend.Next(ctx)
		if !ok {
			return
		}
		if q.metrics != nil {
			q.metrics.DecQueueDepth()
			q.metrics.IncActiveReviews()
		}
		if q.tracker != nil {
			q.tracker.Started(ctx, j)
		}

		log := slog.With(
			"worker", id,
			"job", j.JobKey,
			"delivery", j.DeliveryID,
			"owner", j.Owner,
			"repo", j.Repo,
			"pr", j.PRNumber,
			"type", j.Type,
		)

		jobType := j.Type
		if jobType == "" {
			jobType = "review"
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					if q.tracker != nil {
						q.tracker.Failed(ctx, j, 0, fmt.Errorf("panic: %v", r))
					}
					log.Error("worker panic recovered", "panic", fmt.Sprintf("%v", r))
					if q.metrics != nil {
						q.metrics.IncReviewErrors()
						q.metrics.IncJobOutcome(jobType, "failed")
					}
				}
				if q.metrics != nil {
					q.metrics.DecActiveReviews()
				}
				q.backend.MarkDone(ctx, j)
			}()

			attempts, err := q.processor.Process(ctx, j)
			switch {
			case err == nil:
				if q.tracker != nil {
					q.tracker.Completed(ctx, j, attempts)
				}
				if q.metrics != nil {
					q.metrics.IncJobOutcome(jobType, "completed")
				}
				log.Info("job completed", "attempts", attempts)
			case errors.Is(err, ErrJobCancelled):
				if q.tracker != nil {
					q.tracker.Cancelled(ctx, j, attempts)
				}
				if q.metrics != nil {
					q.metrics.IncJobOutcome(jobType, "cancelled")
				}
				log.Info("job cancelled", "attempts", attempts)
			default:
				if q.tracker != nil {
					q.tracker.Failed(ctx, j, attempts, err)
				}
				if q.metrics != nil {
					q.metrics.IncJobOutcome(jobType, "failed")
				}
				log.Error("job failed", "attempts", attempts, "error", err)
				if q.metrics != nil {
					q.metrics.IncReviewErrors()
				}
			}
		}()
	}
}
