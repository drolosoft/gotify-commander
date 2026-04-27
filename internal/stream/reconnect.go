package stream

import "time"

// Backoff implements exponential back-off with a configurable base and cap.
type Backoff struct {
	base    time.Duration
	max     time.Duration
	current time.Duration
}

// NewBackoff creates a Backoff starting at base and capping at max.
func NewBackoff(base, max time.Duration) *Backoff {
	return &Backoff{base: base, max: max, current: base}
}

// Next returns the current wait duration, then doubles it (capped at max).
func (b *Backoff) Next() time.Duration {
	d := b.current
	next := b.current * 2
	if next > b.max {
		next = b.max
	}
	b.current = next
	return d
}

// Reset returns the backoff to its base duration.
func (b *Backoff) Reset() {
	b.current = b.base
}
