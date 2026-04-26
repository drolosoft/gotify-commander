package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Category defines a group of commands displayed in the web UI.
// Type controls the UI selector: "machine" (VPS/Mac buttons), "service" (service chips), "direct" (runs immediately).
type Category struct {
	Label    string   `yaml:"label"`
	Type     string   `yaml:"type"`
	Commands []string `yaml:"commands"`
	Danger   bool     `yaml:"danger"`
}

// Config is the root configuration structure.
type Config struct {
	Gotify      GotifyConfig         `yaml:"gotify"`
	Defaults    Defaults             `yaml:"defaults"`
	SSHTargets  map[string]SSHTarget `yaml:"ssh_targets"`
	Services    map[string]Service   `yaml:"services"`
	Categories  []Category           `yaml:"categories"`
	WebPassword string               `yaml:"web_password"`
	WebURL      string               `yaml:"-"`        // full control panel URL, set at runtime
}

// GotifyConfig holds Gotify server connection settings.
type GotifyConfig struct {
	ServerURL        string `yaml:"server_url"`
	BaseURL          string `yaml:"base_url"`
	ClientToken      string `yaml:"client_token"`
	CommandAppID     int    `yaml:"command_app_id"`
	ResponseAppToken string `yaml:"response_app_token"`
}

// Defaults holds fallback values for optional per-service settings.
type Defaults struct {
	Timeout  time.Duration `yaml:"timeout"`
	LogLines int           `yaml:"log_lines"`
}

// SSHTarget defines how to connect to a remote machine via SSH.
type SSHTarget struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	User    string `yaml:"user"`
	KeyFile string `yaml:"key_file"`
}

// Service represents a single managed service.
type Service struct {
	Description string   `yaml:"description"`
	Machine     string   `yaml:"machine"`
	Port        int      `yaml:"port"`
	Domain      string   `yaml:"domain"`
	Aliases     []string `yaml:"aliases"`
	Systemd     string   `yaml:"systemd"`
	Launchd     string   `yaml:"launchd"`
}

// DefaultConfig returns a Config populated with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		Defaults: Defaults{
			Timeout:  30 * time.Second,
			LogLines: 30,
		},
		SSHTargets: make(map[string]SSHTarget),
		Services:   make(map[string]Service),
	}
}

// ParseYAML unmarshals YAML bytes into a Config, applying defaults first so
// any field not present in the YAML retains its default value.
func ParseYAML(data []byte) (*Config, error) {
	cfg := DefaultConfig()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parsing YAML: %w", err)
	}
	return cfg, nil
}

// Validate checks the Config for required fields and logical consistency.
// It also applies any implicit defaults (e.g. SSH port 22 when unset).
func Validate(cfg *Config) error {
	// Required Gotify fields.
	if cfg.Gotify.ServerURL == "" {
		return fmt.Errorf("gotify.server_url is required")
	}
	if cfg.Gotify.ClientToken == "" {
		return fmt.Errorf("gotify.client_token is required")
	}
	if cfg.Gotify.CommandAppID == 0 {
		return fmt.Errorf("gotify.command_app_id is required")
	}
	if cfg.Gotify.ResponseAppToken == "" {
		return fmt.Errorf("gotify.response_app_token is required")
	}

	// Service validation.
	for name, svc := range cfg.Services {
		switch svc.Machine {
		case "vps":
			if svc.Systemd == "" {
				return fmt.Errorf("service %q: machine 'vps' requires systemd field", name)
			}
		case "mac":
			if svc.Launchd == "" {
				return fmt.Errorf("service %q: machine 'mac' requires launchd field", name)
			}
		default:
			return fmt.Errorf("service %q: machine must be 'vps' or 'mac', got %q", name, svc.Machine)
		}
	}

	// SSH target defaults.
	for name, target := range cfg.SSHTargets {
		if target.Port == 0 {
			target.Port = 22
			cfg.SSHTargets[name] = target
		}
	}

	return nil
}

// BuildAliasMap returns a map from alias (or service name) to the canonical
// service name. Every service name maps to itself; every alias maps to the
// name of the service that declared it.
func BuildAliasMap(cfg *Config) map[string]string {
	m := make(map[string]string, len(cfg.Services))
	for name, svc := range cfg.Services {
		// Canonical name maps to itself.
		m[name] = name
		// Each alias maps to the canonical name.
		for _, alias := range svc.Aliases {
			m[alias] = name
		}
	}
	return m
}
