package usecase

import (
	"sync"
	"time"
)

// rateLimiter is a per-key sliding-window counter. It bounds how many
// mention-triggered jobs one author can start per repo per window — every
// mention costs a Devin run, so unbounded mentions are unbounded spend.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
	lastGC time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

// allow reports whether the key may consume another slot and records the hit
// when it may. A limit of 0 disables limiting.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	if l == nil || l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > l.window {
		for k, ts := range l.hits {
			if kept := prune(ts, now, l.window); len(kept) == 0 {
				delete(l.hits, k)
			} else {
				l.hits[k] = kept
			}
		}
		l.lastGC = now
	}

	ts := prune(l.hits[key], now, l.window)
	if len(ts) >= l.limit {
		l.hits[key] = ts
		return false
	}
	l.hits[key] = append(ts, now)
	return true
}

func prune(ts []time.Time, now time.Time, window time.Duration) []time.Time {
	cutoff := now.Add(-window)
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}
