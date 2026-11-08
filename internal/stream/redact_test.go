package stream

import "testing"

// TestRedact checks that the configured token and any token= query value
// never survive into a log line.
func TestRedact(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		secret string
		want   string
	}{
		{
			name:   "configured token anywhere in the text",
			text:   "dial failed with key C1ient.T0ken while connecting",
			secret: "C1ient.T0ken",
			want:   "dial failed with key [redacted] while connecting",
		},
		{
			name:   "token query value in a URL",
			text:   `dial ws://gotify.local/stream?token=abc123: refused`,
			secret: "",
			want:   `dial ws://gotify.local/stream?token=[redacted]: refused`,
		},
		{
			name:   "token query value followed by more parameters",
			text:   "GET /stream?token=abc123&since=5 HTTP/1.1",
			secret: "",
			want:   "GET /stream?token=[redacted]&since=5 HTTP/1.1",
		},
		{
			name:   "both the token and a different query token",
			text:   "key=Sekret url=/stream?token=other",
			secret: "Sekret",
			want:   "key=[redacted] url=/stream?token=[redacted]",
		},
		{
			name:   "empty secret leaves ordinary text untouched",
			text:   "connected to ws://gotify.local/stream",
			secret: "",
			want:   "connected to ws://gotify.local/stream",
		},
		{
			name:   "token inside quotes stops at the quote",
			text:   `url "ws://h/stream?token=abc" failed`,
			secret: "",
			want:   `url "ws://h/stream?token=[redacted]" failed`,
		},
	}

	for _, tc := range cases {
		got := redact(tc.text, tc.secret)
		if got != tc.want {
			t.Errorf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// TestCommandWordKeepsOnlyTheVerb checks the busy log never carries arguments.
func TestCommandWordKeepsOnlyTheVerb(t *testing.T) {
	cases := map[string]string{
		"locate 28.5 -17.8": "locate",
		"  ping  ":          "ping",
		"":                  "",
	}

	for input, want := range cases {
		if got := commandWord(input); got != want {
			t.Errorf("commandWord(%q) = %q, want %q", input, got, want)
		}
	}
}
