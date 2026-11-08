package stream

import (
	"testing"
)

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
