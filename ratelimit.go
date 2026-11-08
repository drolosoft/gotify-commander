package main

import (
	"sync"
	"time"
)

// rateLimiter allows at most limit calls in any sliding window. It keeps the
// time of each allowed call, which is cheap at the sizes the panel uses and
// gives an exact "10 per minute" instead of a bucket's average.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	calls  []time.Time

	// now is replaceable so tests can move the clock.
	now func() time.Time
}

// newRateLimiter returns a limiter for limit calls per window.
func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:  limit,
		window: window,
		calls:  make([]time.Time, 0, limit),
		now:    time.Now,
	}
}

// Allow records a call and reports whether it fits in the window. When it
// does not, it also returns how long until the oldest call leaves the window
// and a new one would fit.
func (r *rateLimiter) Allow() (bool, time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()

	// Drop the calls that are already a full window old. They are kept in
	// order, so the first one still inside the window ends the scan.
	expired := 0
	for expired < len(r.calls) && now.Sub(r.calls[expired]) >= r.window {
		expired++
	}
	r.calls = append(r.calls[:0], r.calls[expired:]...)

	if len(r.calls) >= r.limit {
		return false, r.calls[0].Add(r.window).Sub(now)
	}

	r.calls = append(r.calls, now)
	return true, 0
}
