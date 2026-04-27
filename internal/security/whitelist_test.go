package security

import (
	"testing"
)

func TestWhitelist(t *testing.T) {
	wl := NewWhitelist(map[string]bool{
		"nginx":   true,
		"laporra": true,
		"redis":   true,
	})

	if !wl.IsAllowed("nginx") {
		t.Error("expected nginx to be allowed")
	}
	if !wl.IsAllowed("laporra") {
		t.Error("expected laporra to be allowed")
	}
	if wl.IsAllowed("unknown") {
		t.Error("expected unknown to be rejected")
	}
	if wl.IsAllowed("") {
		t.Error("expected empty string to be rejected")
	}
}
