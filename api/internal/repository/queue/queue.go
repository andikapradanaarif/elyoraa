package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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

// Submit enqueues a job. Returns false if the queue is full and the job is dropped.
func (q *Queue) Submit(ctx context.Context, j domain.Review) bool {
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

// Start launches the worker pool. It blocks until the context is cancelled.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		go q.worker(ctx, i)
	}
	<-ctx.Done()
	_ = q.backend.Close()
}

func (q *Queue) worker(ctx context.Context, id int) {
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

		func() {
			defer func() {
				if r := recover(); r != nil {
					if q.tracker != nil {
						q.tracker.Failed(ctx, j, 0, fmt.Errorf("panic: %v", r))
					}
					log.Error("worker panic recovered", "panic", fmt.Sprintf("%v", r))
					if q.metrics != nil {
						q.metrics.IncReviewErrors()
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
				log.Info("job completed", "attempts", attempts)
			case errors.Is(err, ErrJobCancelled):
				if q.tracker != nil {
					q.tracker.Cancelled(ctx, j, attempts)
				}
				log.Info("job cancelled", "attempts", attempts)
			default:
				if q.tracker != nil {
					q.tracker.Failed(ctx, j, attempts, err)
				}
				log.Error("job failed", "attempts", attempts, "error", err)
				if q.metrics != nil {
					q.metrics.IncReviewErrors()
				}
			}
		}()
	}
}
