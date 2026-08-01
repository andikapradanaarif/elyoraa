package queue

import (
	"context"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
)

// Backend is the durable or in-memory storage for jobs.
type Backend interface {
	// Put adds a job. Returns false if the queue is full or context done.
	Put(ctx context.Context, j domain.Review) bool
	// Next waits for and returns the next job. Returns false if context done.
	Next(ctx context.Context) (domain.Review, bool)
	// MarkDone is called after a job is processed; some backends no-op it.
	MarkDone(ctx context.Context, j domain.Review)
	// Len returns the current number of pending jobs.
	Len() int
	// Close releases backend resources.
	Close() error
}

// MemoryBackend is a simple in-memory channel-backed queue.
type MemoryBackend struct {
	jobs chan domain.Review
}

// NewMemoryBackend creates a buffered in-memory backend.
func NewMemoryBackend(capacity int) *MemoryBackend {
	if capacity <= 0 {
		capacity = 64
	}
	return &MemoryBackend{jobs: make(chan domain.Review, capacity)}
}

func (m *MemoryBackend) Put(ctx context.Context, j domain.Review) bool {
	select {
	case m.jobs <- j:
		return true
	case <-ctx.Done():
		return false
	default:
		return false
	}
}

func (m *MemoryBackend) Next(ctx context.Context) (domain.Review, bool) {
	select {
	case <-ctx.Done():
		return domain.Review{}, false
	case j := <-m.jobs:
		return j, true
	}
}

func (m *MemoryBackend) MarkDone(ctx context.Context, j domain.Review) {}

func (m *MemoryBackend) Len() int {
	return len(m.jobs)
}

func (m *MemoryBackend) Close() error {
	close(m.jobs)
	return nil
}
