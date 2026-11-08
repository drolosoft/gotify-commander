package stream

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	b := NewBackoff(time.Second, 60*time.Second)

	// First three calls must double.
	got := b.Next()
	if got != time.Second {
		t.Errorf("first Next(): want 1s, got %v", got)
	}

	got = b.Next()
	if got != 2*time.Second {
		t.Errorf("second Next(): want 2s, got %v", got)
	}

	got = b.Next()
	if got != 4*time.Second {
		t.Errorf("third Next(): want 4s, got %v", got)
	}

	// Call Next() 10 more times; every result must be <= 60s.
	for i := 0; i < 10; i++ {
		got = b.Next()
		if got > 60*time.Second {
			t.Errorf("iteration %d: got %v, want <= 60s", i, got)
		}
	}

	// After many doublings the value must have been capped at max.
	if got != 60*time.Second {
		t.Errorf("after saturation: want 60s, got %v", got)
	}

	// Reset must bring it back to base.
	b.Reset()
	got = b.Next()
	if got != time.Second {
		t.Errorf("after Reset(), first Next(): want 1s, got %v", got)
	}
}

func TestBackoffCustomMaxAndBase(t *testing.T) {
	b := NewBackoff(500*time.Millisecond, 4*time.Second)

	// 500ms → 1s → 2s → 4s → cap
	expected := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		4 * time.Second, // capped
	}

	for i, want := range expected {
		got := b.Next()
		if got != want {
			t.Errorf("call %d: want %v, got %v", i+1, want, got)
		}
	}
}

// TestBackoffDelay checks the pure delay computation: doubling from base,
// the cap, and the jitter spread at both ends of the roll.
func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		name    string
		attempt int
		jitter  float64
		roll    float64
		want    time.Duration
	}{
		{"first attempt without jitter", 0, 0, 0.5, time.Second},
		{"second attempt doubles", 1, 0, 0.5, 2 * time.Second},
		{"sixth attempt is 32s", 5, 0, 0.5, 32 * time.Second},
		{"seventh attempt hits the cap", 6, 0, 0.5, 60 * time.Second},
		{"huge attempt stays at the cap", 10_000, 0, 0.5, 60 * time.Second},
		{"middle roll leaves the delay alone", 2, 0.2, 0.5, 4 * time.Second},
		{"lowest roll takes 20 percent off", 2, 0.2, 0, 3200 * time.Millisecond},
		{"highest roll adds 20 percent", 2, 0.2, 1, 4800 * time.Millisecond},
		{"jitter never goes above the cap", 6, 0.2, 1, 60 * time.Second},
		{"jitter below the cap at the lowest roll", 6, 0.2, 0, 48 * time.Second},
		{"negative attempt counts as the first", -3, 0, 0.5, time.Second},
	}

	for _, tc := range cases {
		got := BackoffDelay(tc.attempt, time.Second, 60*time.Second, tc.jitter, tc.roll)
		if got != tc.want {
			t.Errorf("%s: BackoffDelay(%d, 1s, 60s, %v, %v) = %v, want %v",
				tc.name, tc.attempt, tc.jitter, tc.roll, got, tc.want)
		}
	}
}

// TestBackoffWithJitterStaysInRange runs the jittered Backoff the listener
// uses and checks every wait against the ±20 % band around the plain value.
func TestBackoffWithJitterStaysInRange(t *testing.T) {
	backoff := NewBackoff(time.Second, 60*time.Second).WithJitter(0.2)

	for attempt := 0; attempt < 12; attempt++ {
		plain := BackoffDelay(attempt, time.Second, 60*time.Second, 0, 0.5)
		lowest := time.Duration(float64(plain) * 0.8)
		highest := min(time.Duration(float64(plain)*1.2), 60*time.Second)

		got := backoff.Next()
		if got < lowest || got > highest {
			t.Errorf("attempt %d: got %v, want between %v and %v", attempt, got, lowest, highest)
		}
	}

	// A successful connection starts the sequence again at about one second.
	backoff.Reset()
	got := backoff.Next()
	if got < 800*time.Millisecond || got > 1200*time.Millisecond {
		t.Errorf("after Reset(): got %v, want about 1s", got)
	}
}
