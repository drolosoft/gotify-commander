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

func TestListenerWSURL(t *testing.T) {
	cases := []struct {
		serverURL string
		want      string
	}{
		{"http://gotify.example.com", "ws://gotify.example.com/stream?token=tok"},
		{"https://gotify.example.com", "wss://gotify.example.com/stream?token=tok"},
		{"http://gotify.example.com/", "ws://gotify.example.com/stream?token=tok"},
	}

	for _, tc := range cases {
		l := NewListener(tc.serverURL, "tok", 1, nil)
		got := l.wsURL()
		if got != tc.want {
			t.Errorf("wsURL(%q) = %q, want %q", tc.serverURL, got, tc.want)
		}
	}
}
