package executor

import "testing"

// TestShellQuote checks that every word comes out as exactly one POSIX shell
// word, and that plain words are left readable.
func TestShellQuote(t *testing.T) {
	cases := []struct {
		word string
		want string
	}{
		{"launchctl", "launchctl"},
		{"com.tubearchivist.web", "com.tubearchivist.web"},
		{"http://127.0.0.1:8080/", "http://127.0.0.1:8080/"},
		{"--max-time", "--max-time"},
		{"", "''"},
		{"%{http_code}", "'%{http_code}'"},
		{"foo; rm -rf /", "'foo; rm -rf /'"},
		{"$(whoami)", "'$(whoami)'"},
		{"`id`", "'`id`'"},
		{"it's", `'it'\''s'`},
		{"a\nb", "'a\nb'"},
	}

	for _, testCase := range cases {
		got := shellQuote(testCase.word)
		if got != testCase.want {
			t.Errorf("shellQuote(%q) = %q, want %q", testCase.word, got, testCase.want)
		}
	}
}

// TestRemoteCommandQuotesEveryArgument checks the command string sent to the
// remote shell for a name and arguments.
func TestRemoteCommandQuotesEveryArgument(t *testing.T) {
	got := remoteCommand("launchctl", "load", "/Users/me/Library/My Agent.plist")
	want := "launchctl load '/Users/me/Library/My Agent.plist'"

	if got != want {
		t.Errorf("remoteCommand = %q, want %q", got, want)
	}
}
