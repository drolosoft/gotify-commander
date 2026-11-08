package config

import (
	"strings"
	"testing"
)

// TestValidateTargetOS checks the values ssh_targets.<name>.os accepts and
// that a target without one is linux after Validate.
func TestValidateTargetOS(t *testing.T) {
	tests := []struct {
		name    string
		os      string
		wantOS  string
		wantErr string
	}{
		{name: "left out", os: "", wantOS: "linux"},
		{name: "linux", os: "linux", wantOS: "linux"},
		{name: "darwin", os: "darwin", wantOS: "darwin"},
		{name: "macos is not a value", os: "macos", wantErr: `ssh_targets.mac.os must be "linux" or "darwin", got "macos"`},
		{name: "case matters", os: "Darwin", wantErr: `got "Darwin"`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Gotify = GotifyConfig{ServerURL: "http://localhost", ClientToken: "token", CommandAppID: 1}
			cfg.SSHTargets["mac"] = SSHTarget{Host: "mac.example", OS: test.os}

			err := Validate(cfg)

			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("Validate error = %v, want one containing %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if got := cfg.SSHTargets["mac"].OS; got != test.wantOS {
				t.Errorf("os = %q, want %q", got, test.wantOS)
			}
		})
	}
}

// TestParseTargetOS checks that os is read from YAML.
func TestParseTargetOS(t *testing.T) {
	cfg, err := ParseYAML([]byte("ssh_targets:\n  mac:\n    host: mac.example\n    os: darwin\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}

	if got := cfg.SSHTargets["mac"].OS; got != "darwin" {
		t.Errorf("os = %q, want darwin", got)
	}
}
