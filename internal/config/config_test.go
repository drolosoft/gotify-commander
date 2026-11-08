package config

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Defaults.Timeout != 30*time.Second {
		t.Errorf("expected default timeout 30s, got %v", cfg.Defaults.Timeout)
	}
	if cfg.Defaults.LogLines != 30 {
		t.Errorf("expected default log lines 30, got %d", cfg.Defaults.LogLines)
	}
	if cfg.SSHTargets == nil {
		t.Error("SSHTargets map should be initialized, got nil")
	}
	if cfg.Services == nil {
		t.Error("Services map should be initialized, got nil")
	}
}

func TestParseYAML(t *testing.T) {
	yaml := []byte(`
gotify:
  server_url: "https://gotify.example.com"
  client_token: "mytoken"
  command_app_id: 42
  response_app_token: "apptoken"

defaults:
  timeout: 15s
  log_lines: 50

ssh_targets:
  vps:
    host: "192.168.1.10"
    port: 2222
    user: "admin"
    key_file: "~/.ssh/id_rsa"

services:
  nginx:
    description: "Nginx web server"
    machine: "vps"
    port: 80
    aliases:
      - "web"
      - "www"
    systemd: "nginx.service"
  myapp:
    description: "My macOS app"
    machine: "mac"
    port: 8080
    aliases:
      - "app"
    launchd: "com.example.myapp"
`)

	cfg, err := ParseYAML(yaml)
	if err != nil {
		t.Fatalf("ParseYAML returned error: %v", err)
	}

	// Gotify config
	if cfg.Gotify.ServerURL != "https://gotify.example.com" {
		t.Errorf("expected server_url 'https://gotify.example.com', got %q", cfg.Gotify.ServerURL)
	}
	if cfg.Gotify.ClientToken != "mytoken" {
		t.Errorf("expected client_token 'mytoken', got %q", cfg.Gotify.ClientToken)
	}
	if cfg.Gotify.CommandAppID != 42 {
		t.Errorf("expected command_app_id 42, got %d", cfg.Gotify.CommandAppID)
	}
	if cfg.Gotify.ResponseAppToken != "apptoken" {
		t.Errorf("expected response_app_token 'apptoken', got %q", cfg.Gotify.ResponseAppToken)
	}

	// Defaults
	if cfg.Defaults.Timeout != 15*time.Second {
		t.Errorf("expected timeout 15s, got %v", cfg.Defaults.Timeout)
	}
	if cfg.Defaults.LogLines != 50 {
		t.Errorf("expected log_lines 50, got %d", cfg.Defaults.LogLines)
	}

	// SSH target
	vps, ok := cfg.SSHTargets["vps"]
	if !ok {
		t.Fatal("expected SSH target 'vps' to exist")
	}
	if vps.Host != "192.168.1.10" {
		t.Errorf("expected host '192.168.1.10', got %q", vps.Host)
	}
	if vps.Port != 2222 {
		t.Errorf("expected port 2222, got %d", vps.Port)
	}
	if vps.User != "admin" {
		t.Errorf("expected user 'admin', got %q", vps.User)
	}
	if vps.KeyFile != "~/.ssh/id_rsa" {
		t.Errorf("expected key_file '~/.ssh/id_rsa', got %q", vps.KeyFile)
	}

	// Services
	nginx, ok := cfg.Services["nginx"]
	if !ok {
		t.Fatal("expected service 'nginx' to exist")
	}
	if nginx.Description != "Nginx web server" {
		t.Errorf("expected nginx description 'Nginx web server', got %q", nginx.Description)
	}
	if nginx.Machine != "vps" {
		t.Errorf("expected nginx machine 'vps', got %q", nginx.Machine)
	}
	if nginx.Port != 80 {
		t.Errorf("expected nginx port 80, got %d", nginx.Port)
	}
	if len(nginx.Aliases) != 2 || nginx.Aliases[0] != "web" || nginx.Aliases[1] != "www" {
		t.Errorf("expected nginx aliases [web www], got %v", nginx.Aliases)
	}
	if nginx.Systemd != "nginx.service" {
		t.Errorf("expected nginx systemd 'nginx.service', got %q", nginx.Systemd)
	}

	myapp, ok := cfg.Services["myapp"]
	if !ok {
		t.Fatal("expected service 'myapp' to exist")
	}
	if myapp.Machine != "mac" {
		t.Errorf("expected myapp machine 'mac', got %q", myapp.Machine)
	}
	if myapp.Launchd != "com.example.myapp" {
		t.Errorf("expected myapp launchd 'com.example.myapp', got %q", myapp.Launchd)
	}
}

func TestValidateConfig(t *testing.T) {
	validBase := func() *Config {
		return &Config{
			Gotify: GotifyConfig{
				ServerURL:        "https://gotify.example.com",
				ClientToken:      "token123",
				CommandAppID:     1,
				ResponseAppToken: "apptoken",
			},
			Defaults: Defaults{
				Timeout:  30 * time.Second,
				LogLines: 30,
			},
			SSHTargets: map[string]SSHTarget{},
			Services: map[string]Service{
				"nginx": {
					Description: "Nginx",
					Machine:     "vps",
					Systemd:     "nginx.service",
				},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{
			name:    "valid config passes",
			mutate:  func(c *Config) {},
			wantErr: false,
		},
		{
			name:    "missing server_url fails",
			mutate:  func(c *Config) { c.Gotify.ServerURL = "" },
			wantErr: true,
		},
		{
			name:    "missing client_token fails",
			mutate:  func(c *Config) { c.Gotify.ClientToken = "" },
			wantErr: true,
		},
		{
			name:    "missing command_app_id fails",
			mutate:  func(c *Config) { c.Gotify.CommandAppID = 0 },
			wantErr: true,
		},
		{
			name:    "negative command_app_id fails",
			mutate:  func(c *Config) { c.Gotify.CommandAppID = -3 },
			wantErr: true,
		},
		{
			name:    "missing response_app_token passes, it is deprecated",
			mutate:  func(c *Config) { c.Gotify.ResponseAppToken = "" },
			wantErr: false,
		},
		{
			name:    "rate_limit_per_minute 0 passes, it means the default",
			mutate:  func(c *Config) { c.Defaults.RateLimitPerMinute = 0 },
			wantErr: false,
		},
		{
			name:    "positive rate_limit_per_minute passes",
			mutate:  func(c *Config) { c.Defaults.RateLimitPerMinute = 3 },
			wantErr: false,
		},
		{
			name:    "negative rate_limit_per_minute fails",
			mutate:  func(c *Config) { c.Defaults.RateLimitPerMinute = -1 },
			wantErr: true,
		},
		{
			name: "invalid machine fails",
			mutate: func(c *Config) {
				c.Services["nginx"] = Service{Machine: "windows", Systemd: "nginx.service"}
			},
			wantErr: true,
		},
		{
			name: "mac service missing launchd fails",
			mutate: func(c *Config) {
				c.Services["nginx"] = Service{Machine: "mac", Launchd: ""}
			},
			wantErr: true,
		},
		{
			name: "vps service missing systemd fails",
			mutate: func(c *Config) {
				c.Services["nginx"] = Service{Machine: "vps", Systemd: ""}
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBase()
			tt.mutate(cfg)
			err := Validate(cfg)
			if tt.wantErr && err == nil {
				t.Error("expected error but got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("expected no error but got: %v", err)
			}
		})
	}
}

func TestCommandsPerMinute(t *testing.T) {
	tests := []struct {
		name  string
		yaml  string
		limit int
	}{
		{name: "missing means the default", yaml: "defaults:\n  timeout: 10s\n", limit: DefaultRateLimitPerMinute},
		{name: "zero means the default", yaml: "defaults:\n  rate_limit_per_minute: 0\n", limit: DefaultRateLimitPerMinute},
		{name: "a value is used as is", yaml: "defaults:\n  rate_limit_per_minute: 4\n", limit: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseYAML([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseYAML: %v", err)
			}

			if got := cfg.Defaults.CommandsPerMinute(); got != tt.limit {
				t.Errorf("CommandsPerMinute() = %d, want %d", got, tt.limit)
			}
		})
	}

	// The release spec asks for 20 commands a minute from messages.
	if DefaultRateLimitPerMinute != 20 {
		t.Errorf("DefaultRateLimitPerMinute = %d, want 20", DefaultRateLimitPerMinute)
	}
}

// TestReplayWindow checks the default and that an explicit 0s is kept: it is
// how an administrator turns the replay of pending messages off, so it must
// not be replaced by the default the way rate_limit_per_minute's 0 is. YAML
// durations need a unit, as timeout does: a bare 0 does not parse.
func TestReplayWindow(t *testing.T) {
	tests := []struct {
		name   string
		yaml   string
		window time.Duration
	}{
		{name: "missing means the default", yaml: "defaults:\n  timeout: 10s\n", window: DefaultReplayWindow},
		{name: "zero turns the replay off", yaml: "defaults:\n  replay_window: 0s\n", window: 0},
		{name: "a duration is used as is", yaml: "defaults:\n  replay_window: 90s\n", window: 90 * time.Second},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseYAML([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseYAML: %v", err)
			}

			if got := cfg.Defaults.ReplayWindow; got != tt.window {
				t.Errorf("ReplayWindow = %v, want %v", got, tt.window)
			}
		})
	}

	if DefaultReplayWindow != 5*time.Minute {
		t.Errorf("DefaultReplayWindow = %v, want 5m", DefaultReplayWindow)
	}
}

func TestValidateCategoryCommands(t *testing.T) {
	known := []string{"free", "df", "help", "locate"}

	cfg := DefaultConfig()
	cfg.Categories = []Category{
		{Label: "Machine", Type: "machine", Commands: []string{"free", "df"}},
		{Label: "Info", Type: "direct", Commands: []string{"help", "locate"}},
	}
	if err := ValidateCategoryCommands(cfg, known); err != nil {
		t.Errorf("known commands: unexpected error %v", err)
	}

	// No categories is valid: the panel falls back to its own tabs.
	if err := ValidateCategoryCommands(DefaultConfig(), known); err != nil {
		t.Errorf("no categories: unexpected error %v", err)
	}

	// A typo would give a button that only ever answers "unknown command",
	// so the configuration is refused and the error names the culprit.
	cfg.Categories = append(cfg.Categories, Category{Label: "Danger", Commands: []string{"free", "reboott"}})
	err := ValidateCategoryCommands(cfg, known)
	if err == nil {
		t.Fatal("unknown command reboott: expected an error")
	}
	if !strings.Contains(err.Error(), `"reboott"`) || !strings.Contains(err.Error(), `"Danger"`) {
		t.Errorf("error %q does not name the command and its category", err)
	}
}

func TestParseKnownHostsFile(t *testing.T) {
	cfg, err := ParseYAML([]byte(`
ssh_targets:
  mac:
    host: "100.64.0.2"
    user: "admin"
    key_file: "/home/admin/.ssh/id_ed25519"
    known_hosts_file: "/home/admin/.ssh/known_hosts"
`))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}

	if got := cfg.SSHTargets["mac"].KnownHostsFile; got != "/home/admin/.ssh/known_hosts" {
		t.Errorf("known_hosts_file = %q, want /home/admin/.ssh/known_hosts", got)
	}
}

func TestBuildAliasMap(t *testing.T) {
	cfg := &Config{
		Services: map[string]Service{
			"nginx": {
				Aliases: []string{"web", "www"},
			},
			"postgres": {
				Aliases: []string{"db", "pg"},
			},
			"redis": {
				Aliases: []string{"cache"},
			},
		},
	}

	aliasMap := BuildAliasMap(cfg)

	// Service names map to themselves
	for _, name := range []string{"nginx", "postgres", "redis"} {
		if got := aliasMap[name]; got != name {
			t.Errorf("service name %q should map to itself, got %q", name, got)
		}
	}

	// Aliases map to canonical names
	expected := map[string]string{
		"web":   "nginx",
		"www":   "nginx",
		"db":    "postgres",
		"pg":    "postgres",
		"cache": "redis",
	}
	for alias, canonical := range expected {
		if got := aliasMap[alias]; got != canonical {
			t.Errorf("alias %q should map to %q, got %q", alias, canonical, got)
		}
	}

	// Total entries: 3 service names + 5 aliases = 8
	if len(aliasMap) != 8 {
		t.Errorf("expected 8 entries in alias map, got %d", len(aliasMap))
	}
}

// TestRequireConfirm: confirmation is opt-in. Left out, null or empty, no
// command asks for a code; a list turns it on for those commands.
func TestRequireConfirm(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want []string
	}{
		{name: "missing means off", yaml: "defaults:\n  timeout: 10s\n", want: nil},
		{name: "null means off", yaml: "require_confirm:\n", want: nil},
		{name: "an empty list means off", yaml: "require_confirm: []\n", want: nil},
		{name: "a list turns it on", yaml: "require_confirm: [reboot, logs]\n", want: []string{"reboot", "logs"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseYAML([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseYAML: %v", err)
			}

			if !sameNames(cfg.RequireConfirm, tt.want) {
				t.Errorf("RequireConfirm = %q, want %q", cfg.RequireConfirm, tt.want)
			}
		})
	}
}

// TestRequireConfirmSurvivesARoundTrip: Gotify stores the configuration by
// marshalling what it parsed, so "off" must stay off and a list must stay
// the same list after a save.
func TestRequireConfirmSurvivesARoundTrip(t *testing.T) {
	documents := []string{
		"defaults:\n  timeout: 10s\n",
		"require_confirm: []\n",
		"require_confirm: [reboot, shutdown, stop]\nconfirmation:\n  keyword: ok\n  window: 10s\n",
	}

	for _, document := range documents {
		first, err := ParseYAML([]byte(document))
		if err != nil {
			t.Fatalf("ParseYAML(%q): %v", document, err)
		}

		saved, err := yaml.Marshal(first)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		second, err := ParseYAML(saved)
		if err != nil {
			t.Fatalf("ParseYAML(saved): %v", err)
		}

		if !sameNames(first.RequireConfirm, second.RequireConfirm) {
			t.Errorf("%q: require_confirm %q became %q after a save", document, first.RequireConfirm, second.RequireConfirm)
		}
		if first.Confirmation != second.Confirmation {
			t.Errorf("%q: confirmation %+v became %+v after a save", document, first.Confirmation, second.Confirmation)
		}
	}
}

// sameNames reports whether two lists of command names are equal, with nil
// and empty taken as the same: both mean "none" in the configuration.
func sameNames(left, right []string) bool {
	if len(left) == 0 && len(right) == 0 {
		return true
	}
	return slices.Equal(left, right)
}

func TestConfirmationDefaults(t *testing.T) {
	cfg, err := ParseYAML([]byte("defaults:\n  timeout: 10s\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}

	confirmation := cfg.Confirmation
	if confirmation.KeywordOrDefault() != "confirm" {
		t.Errorf("keyword = %q, want confirm", confirmation.KeywordOrDefault())
	}
	if confirmation.PromptOrDefault() != "⚠️ Confirm `{command}`: reply `{keyword} {code}` within {seconds} s" {
		t.Errorf("prompt = %q, want the default", confirmation.PromptOrDefault())
	}
	if confirmation.WindowOrDefault() != 60*time.Second {
		t.Errorf("window = %s, want 60s", confirmation.WindowOrDefault())
	}

	// A block written with empty values, or a Config built in code, falls
	// back to the same defaults.
	var empty ConfirmationConfig
	if empty.KeywordOrDefault() != "confirm" || empty.PromptOrDefault() != DefaultConfirmationPrompt || empty.WindowOrDefault() != DefaultConfirmationWindow {
		t.Errorf("zero ConfirmationConfig = %q, %q, %s; want the defaults", empty.KeywordOrDefault(), empty.PromptOrDefault(), empty.WindowOrDefault())
	}

	custom, err := ParseYAML([]byte("confirmation:\n  keyword: ok\n  prompt: \"Send {keyword} {code}\"\n  window: 10s\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}
	if custom.Confirmation.KeywordOrDefault() != "ok" || custom.Confirmation.PromptOrDefault() != "Send {keyword} {code}" || custom.Confirmation.WindowOrDefault() != 10*time.Second {
		t.Errorf("custom confirmation = %+v", custom.Confirmation)
	}
}

func TestValidateConfirmation(t *testing.T) {
	validBase := func() *Config {
		cfg := DefaultConfig()
		cfg.Gotify = GotifyConfig{ServerURL: "https://gotify.example.com", ClientToken: "token123", CommandAppID: 1}
		cfg.Services = map[string]Service{
			"nginx": {Machine: "vps", Systemd: "nginx", Aliases: []string{"web"}},
		}
		cfg.RequireConfirm = []string{"reboot"}
		return cfg
	}

	if err := Validate(validBase()); err != nil {
		t.Fatalf("valid base: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "upper case keyword", mutate: func(c *Config) { c.Confirmation.Keyword = "Confirm" }, wantErr: "confirmation.keyword"},
		{name: "two words", mutate: func(c *Config) { c.Confirmation.Keyword = "do it" }, wantErr: "confirmation.keyword"},
		{name: "punctuation", mutate: func(c *Config) { c.Confirmation.Keyword = "ok!" }, wantErr: "confirmation.keyword"},
		{name: "starts with a digit", mutate: func(c *Config) { c.Confirmation.Keyword = "1ok" }, wantErr: "confirmation.keyword"},
		{name: "a service name", mutate: func(c *Config) { c.Confirmation.Keyword = "nginx" }, wantErr: "confirmation.keyword"},
		{name: "a service alias", mutate: func(c *Config) { c.Confirmation.Keyword = "web" }, wantErr: "confirmation.keyword"},
		{name: "prompt without the code", mutate: func(c *Config) { c.Confirmation.Prompt = "Reply {keyword}" }, wantErr: "{code}"},
		{name: "negative window", mutate: func(c *Config) { c.Confirmation.Window = -time.Second }, wantErr: "confirmation.window"},
		{name: "window under a second", mutate: func(c *Config) { c.Confirmation.Window = 500 * time.Millisecond }, wantErr: "confirmation.window"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validBase()
			tt.mutate(cfg)

			err := Validate(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Validate = %v, want an error naming %s", err, tt.wantErr)
			}
		})
	}

	// A keyword that is fine on its own, accents included.
	for _, keyword := range []string{"ok", "sí", "yes2"} {
		cfg := validBase()
		cfg.Confirmation.Keyword = keyword
		if err := Validate(cfg); err != nil {
			t.Errorf("keyword %q: unexpected error %v", keyword, err)
		}
	}

	// With confirmation off, a service may be called like the keyword.
	cfg := validBase()
	cfg.RequireConfirm = nil
	cfg.Services["confirm"] = Service{Machine: "vps", Systemd: "confirm"}
	if err := Validate(cfg); err != nil {
		t.Errorf("service named confirm with confirmation off: unexpected error %v", err)
	}
}

func TestEnabledCommandsParse(t *testing.T) {
	cfg, err := ParseYAML([]byte("enabled_commands: [ping, uptime]\n"))
	if err != nil {
		t.Fatalf("ParseYAML: %v", err)
	}

	if !slices.Equal(cfg.EnabledCommands, []string{"ping", "uptime"}) {
		t.Errorf("EnabledCommands = %q, want ping and uptime", cfg.EnabledCommands)
	}

	// Absent means every command is enabled, so there is no default list.
	if DefaultConfig().EnabledCommands != nil {
		t.Errorf("default EnabledCommands = %q, want nil", DefaultConfig().EnabledCommands)
	}
}

func TestValidateCommandLists(t *testing.T) {
	known := []string{"ping", "uptime", "reboot", "shutdown", "stop", "logs"}

	cfg := DefaultConfig()
	cfg.EnabledCommands = []string{"ping", "reboot"}
	cfg.RequireConfirm = []string{"reboot", "logs"}
	if err := ValidateCommandLists(cfg, known); err != nil {
		t.Errorf("known commands: unexpected error %v", err)
	}

	// Both lists are checked, and the error names the key and the culprit.
	cases := []struct {
		key     string
		culprit string
		mutate  func(*Config)
	}{
		{key: "enabled_commands", culprit: `"rebot"`, mutate: func(c *Config) { c.EnabledCommands = []string{"ping", "rebot"} }},
		{key: "require_confirm", culprit: `"rebot"`, mutate: func(c *Config) { c.RequireConfirm = []string{"rebot"} }},

		// The keyword is read before the commands, so it must not be one.
		{key: "confirmation.keyword", culprit: `"ping"`, mutate: func(c *Config) {
			c.RequireConfirm = []string{"reboot"}
			c.Confirmation.Keyword = "ping"
		}},
	}

	for _, tc := range cases {
		cfg := DefaultConfig()
		tc.mutate(cfg)

		err := ValidateCommandLists(cfg, known)
		if err == nil {
			t.Errorf("%s with %s: expected an error", tc.key, tc.culprit)
			continue
		}
		if !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.culprit) {
			t.Errorf("error %q does not name %s and the command", err, tc.key)
		}

		// The existing call site in the plugin checks the lists too.
		if ValidateCategoryCommands(cfg, known) == nil {
			t.Errorf("ValidateCategoryCommands with a bad %s: expected an error", tc.key)
		}
	}
}
