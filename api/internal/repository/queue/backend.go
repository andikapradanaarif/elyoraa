package queue

import (
	"context"
	"sync"

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

// StoppableBackend can stop work intake without releasing resources — the
// queue uses it to drain in-flight jobs before the final Close, which keeps
// shared handles (e.g. the BoltDB tracker) alive for outcome writes.
type StoppableBackend interface {
	Backend
	// SignalStop wakes blocked Next calls and rejects new Put/Next calls,
	// without releasing underlying resources.
	SignalStop()
}

// MemoryBackend is a simple in-memory channel-backed queue.
type MemoryBackend struct {
	jobs   chan domain.Review
	mu     sync.RWMutex
	closed bool
}

// NewMemoryBackend creates a buffered in-memory backend.
func NewMemoryBackend(capacity int) *MemoryBackend {
	if capacity <= 0 {
		capacity = 64
	}
	return &MemoryBackend{jobs: make(chan domain.Review, capacity)}
}

func (m *MemoryBackend) Put(ctx context.Context, j domain.Review) bool {
	// RLock so Close can't close the channel mid-send.
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return false
	}
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
	case j, ok := <-m.jobs:
		if !ok {
			return domain.Review{}, false
		}
		return j, true
	}
}

func (m *MemoryBackend) MarkDone(ctx context.Context, j domain.Review) {}

func (m *MemoryBackend) Len() int {
	return len(m.jobs)
}

// SignalStop releases Next waiters and rejects further work. For the memory
// backend, closing the jobs channel is the whole teardown.
func (m *MemoryBackend) SignalStop() { _ = m.Close() }

func (m *MemoryBackend) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.jobs)
	}
	return nil
}
