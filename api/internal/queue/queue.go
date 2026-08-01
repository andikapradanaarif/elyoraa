package queue

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/andikapradanaarif/elyoraa/internal/job"
	"github.com/andikapradanaarif/elyoraa/internal/metrics"
)

// Processor handles a single review job.
type Processor interface {
	Process(ctx context.Context, job job.Review) error
}

// Queue is a simple in-memory buffered job queue.
type Queue struct {
	jobs      chan job.Review
	workers   int
	processor Processor
	metrics   *metrics.Registry
}

// New creates a queue with the given worker count and buffer capacity.
func New(workers int, p Processor, m *metrics.Registry, capacity int) *Queue {
	if capacity <= 0 {
		capacity = 64
	}
	return &Queue{
		jobs:      make(chan job.Review, capacity),
		workers:   workers,
		processor: p,
		metrics:   m,
	}
}

// Submit enqueues a job. Returns false if the queue is full and the job is dropped.
func (q *Queue) Submit(ctx context.Context, j job.Review) bool {
	select {
	case q.jobs <- j:
		q.metrics.IncQueueDepth()
		return true
	case <-ctx.Done():
		return false
	default:
		q.metrics.IncDroppedJobs()
		return false
	}
}

// Start launches the worker pool. It blocks until the context is cancelled.
func (q *Queue) Start(ctx context.Context) {
	for i := 0; i < q.workers; i++ {
		go q.worker(ctx, i)
	}
	<-ctx.Done()
	close(q.jobs)
}

func (q *Queue) worker(ctx context.Context, id int) {
	for j := range q.jobs {
		q.metrics.DecQueueDepth()
		q.metrics.IncActiveReviews()

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("worker panic recovered", "worker", id, "job", j.JobKey, "panic", fmt.Sprintf("%v", r))
					q.metrics.IncReviewErrors()
				}
				q.metrics.DecActiveReviews()
			}()

			if err := q.processor.Process(ctx, j); err != nil {
				slog.Error("review failed", "worker", id, "job", j.JobKey, "error", err)
				q.metrics.IncReviewErrors()
			}
		}()
	}
}
