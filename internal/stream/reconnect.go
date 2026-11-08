package stream

import (
	"math/rand/v2"
	"time"
)

// Backoff implements exponential back-off with a configurable base and cap,
// optionally spread by a random jitter so that several clients that lost the
// same server do not all knock on it in the same second.
type Backoff struct {
	base    time.Duration
	max     time.Duration
	jitter  float64
	attempt int

	// roll returns a number in [0, 1). It is a field so tests can make the
	// jitter predictable; in production it is math/rand/v2, which is safe
	// for concurrent use.
	roll func() float64
}

// NewBackoff creates a Backoff starting at base and capping at max, without
// jitter. Use WithJitter to spread the waits.
func NewBackoff(base, max time.Duration) *Backoff {
	return &Backoff{base: base, max: max, roll: rand.Float64}
}

// WithJitter sets the fraction (0.2 means ±20 %) by which every wait is
// randomly spread, and returns the Backoff for chaining.
func (b *Backoff) WithJitter(fraction float64) *Backoff {
	b.jitter = fraction
	return b
}

// Next returns the wait for the current attempt and moves to the next one.
func (b *Backoff) Next() time.Duration {
	wait := BackoffDelay(b.attempt, b.base, b.max, b.jitter, b.roll())
	b.attempt++
	return wait
}

// Reset returns the backoff to its base duration.
func (b *Backoff) Reset() {
	b.attempt = 0
}

// BackoffDelay is the pure computation behind Backoff: base doubled attempt
// times (attempt 0 is the first retry) and capped at max, then spread by
// ±jitter according to roll, a number in [0, 1] where 0.5 means no change.
// The result is never above max, so at the cap the jitter only shortens the
// wait.
func BackoffDelay(attempt int, base, max time.Duration, jitter, roll float64) time.Duration {
	delay := base

	// Doubling stops as soon as the cap is reached, which also keeps a very
	// large attempt number from overflowing the duration.
	for step := 0; step < attempt && delay < max; step++ {
		delay *= 2
	}
	delay = min(delay, max)

	// roll 0 takes the full fraction off, roll 1 adds it.
	spread := float64(delay) * jitter * (2*roll - 1)
	delay += time.Duration(spread)

	return min(delay, max)
}
