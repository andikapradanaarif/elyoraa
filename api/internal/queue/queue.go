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

// ProcessorFunc adapts a function to Processor.
type ProcessorFunc func(ctx context.Context, job job.Review) error

func (f ProcessorFunc) Process(ctx context.Context, job job.Review) error {
	return f(ctx, job)
}

// Queue dispatches review jobs to a pool of workers backed by a Backend.
type Queue struct {
	backend   Backend
	workers   int
	processor Processor
	metrics   *metrics.Registry
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
	}
}

// Submit enqueues a job. Returns false if the queue is full and the job is dropped.
func (q *Queue) Submit(ctx context.Context, j job.Review) bool {
	if !q.backend.Put(ctx, j) {
		if q.metrics != nil {
			q.metrics.IncDroppedJobs()
		}
		return false
	}
	if q.metrics != nil {
		q.metrics.IncQueueDepth()
	}
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

		func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Error("worker panic recovered", "worker", id, "job", j.JobKey, "panic", fmt.Sprintf("%v", r))
					if q.metrics != nil {
						q.metrics.IncReviewErrors()
					}
				}
				if q.metrics != nil {
					q.metrics.DecActiveReviews()
				}
				q.backend.MarkDone(ctx, j)
			}()

			if err := q.processor.Process(ctx, j); err != nil {
				slog.Error("review failed", "worker", id, "job", j.JobKey, "error", err)
				if q.metrics != nil {
					q.metrics.IncReviewErrors()
				}
			}
		}()
	}
}
