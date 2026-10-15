package usecase

import (
	"testing"
	"time"
)

func TestRateLimiter_SlidingWindow(t *testing.T) {
	l := newRateLimiter(2, time.Hour)
	now := time.Now()

	if !l.allow("alice@o/r", now) || !l.allow("alice@o/r", now) {
		t.Fatal("first two hits should be allowed")
	}
	if l.allow("alice@o/r", now) {
		t.Fatal("third hit inside the window should be denied")
	}
	// A different author has their own budget.
	if !l.allow("bob@o/r", now) {
		t.Fatal("different author should be allowed")
	}
	// A different repo has its own budget for the same author.
	if !l.allow("alice@o/other", now) {
		t.Fatal("different repo should be allowed")
	}
	// Once the oldest hit ages out of the window, a slot frees up.
	if !l.allow("alice@o/r", now.Add(time.Hour+time.Second)) {
		t.Fatal("hit after window expiry should be allowed")
	}
}

func TestRateLimiter_Disabled(t *testing.T) {
	l := newRateLimiter(0, time.Hour)
	for i := 0; i < 100; i++ {
		if !l.allow("a@o/r", time.Now()) {
			t.Fatal("limit 0 should disable limiting")
		}
	}
	var nilLimiter *rateLimiter
	if !nilLimiter.allow("a@o/r", time.Now()) {
		t.Fatal("nil limiter should allow")
	}
}
