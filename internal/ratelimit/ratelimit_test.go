package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// call is one step of a limiter scenario: move the clock by advance, then
// call Allow and expect allowed and, when refused, retryAfter.
type call struct {
	advance    time.Duration
	allowed    bool
	retryAfter time.Duration
}

func TestLimiterWindow(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		window time.Duration
		calls  []call
	}{
		{
			name:   "calls under the limit are allowed",
			limit:  3,
			window: time.Minute,
			calls: []call{
				{advance: 0, allowed: true},
				{advance: time.Second, allowed: true},
				{advance: time.Second, allowed: true},
			},
		},
		{
			name:   "the call over the limit waits for the oldest to leave",
			limit:  2,
			window: time.Minute,
			calls: []call{
				{advance: 0, allowed: true},
				{advance: 20 * time.Second, allowed: true},
				{advance: 0, allowed: false, retryAfter: 40 * time.Second},
			},
		},
		{
			name:   "one more fits once the oldest call is a window old",
			limit:  2,
			window: time.Minute,
			calls: []call{
				{advance: 0, allowed: true},
				{advance: 20 * time.Second, allowed: true},
				{advance: 40 * time.Second, allowed: true},
				{advance: 0, allowed: false, retryAfter: 20 * time.Second},
			},
		},
		{
			name:   "a refused call does not spend the budget",
			limit:  1,
			window: time.Minute,
			calls: []call{
				{advance: 0, allowed: true},
				{advance: 30 * time.Second, allowed: false, retryAfter: 30 * time.Second},
				{advance: 29 * time.Second, allowed: false, retryAfter: time.Second},
				{advance: time.Second, allowed: true},
			},
		},
		{
			name:   "a quiet window empties the limiter",
			limit:  2,
			window: time.Minute,
			calls: []call{
				{advance: 0, allowed: true},
				{advance: 0, allowed: true},
				{advance: 2 * time.Minute, allowed: true},
				{advance: 0, allowed: true},
				{advance: 0, allowed: false, retryAfter: time.Minute},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
			limiter := NewWithClock(tt.limit, tt.window, func() time.Time { return now })

			for index, step := range tt.calls {
				now = now.Add(step.advance)

				allowed, retryAfter := limiter.Allow()
				if allowed != step.allowed {
					t.Fatalf("call %d: allowed %v, want %v", index+1, allowed, step.allowed)
				}
				if retryAfter != step.retryAfter {
					t.Errorf("call %d: retry after %s, want %s", index+1, retryAfter, step.retryAfter)
				}
			}
		})
	}
}

func TestNewUsesTheRealClock(t *testing.T) {
	limiter := New(1, time.Hour)

	if allowed, _ := limiter.Allow(); !allowed {
		t.Fatal("first call refused")
	}

	allowed, retryAfter := limiter.Allow()
	if allowed {
		t.Fatal("second call within the hour allowed")
	}
	if retryAfter <= 0 || retryAfter > time.Hour {
		t.Errorf("retry after %s, want a wait within the hour", retryAfter)
	}
}

// TestLimiterIsSafeForConcurrentCallers lets more goroutines than the limit
// call at once; under -race it also checks the lock.
func TestLimiterIsSafeForConcurrentCallers(t *testing.T) {
	const limit = 10
	limiter := New(limit, time.Minute)

	var (
		group   sync.WaitGroup
		mu      sync.Mutex
		allowed int
	)
	for range 4 * limit {
		group.Go(func() {
			ok, _ := limiter.Allow()
			if !ok {
				return
			}

			mu.Lock()
			allowed++
			mu.Unlock()
		})
	}
	group.Wait()

	if allowed != limit {
		t.Errorf("%d calls allowed, want exactly %d", allowed, limit)
	}
}
