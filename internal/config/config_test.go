package config

import (
	"testing"
	"time"
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
			name:    "missing response_app_token fails",
			mutate:  func(c *Config) { c.Gotify.ResponseAppToken = "" },
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
