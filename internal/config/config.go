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
	WebURL      string               `yaml:"-"` // full control panel URL, set at runtime
}

// GotifyConfig holds Gotify server connection settings.
type GotifyConfig struct {
	ServerURL    string `yaml:"server_url"`
	BaseURL      string `yaml:"base_url"`
	ClientToken  string `yaml:"client_token"`
	CommandAppID int    `yaml:"command_app_id"`

	// ResponseAppToken is not used: replies go out through the plugin's own
	// message handler. The field stays so configurations that still carry
	// it keep loading.
	//
	// Deprecated: leave it empty or remove it from the configuration.
	ResponseAppToken string `yaml:"response_app_token"`
}

// Defaults holds fallback values for optional per-service settings.
type Defaults struct {
	Timeout  time.Duration `yaml:"timeout"`
	LogLines int           `yaml:"log_lines"`

	// RateLimitPerMinute is how many commands sent as Gotify messages run
	// in any minute. 0 means DefaultRateLimitPerMinute; read it through
	// CommandsPerMinute. The control panel has its own limit.
	RateLimitPerMinute int `yaml:"rate_limit_per_minute"`

	// ReplayWindow is how old a command posted while the plugin was not
	// connected (a Gotify restart, a dropped stream) may be and still run
	// when the plugin connects again. Older ones are answered as expired
	// and not run. 0s turns the replay off: only commands that arrive on the
	// open stream run, as before. Like timeout, it needs a unit in YAML.
	ReplayWindow time.Duration `yaml:"replay_window"`
}

// DefaultReplayWindow is the replay window of a configuration that does not
// set replay_window. Five minutes covers a Gotify restart and the reconnect
// backoff with room to spare; a "reboot" typed an hour ago is no longer what
// the user wants.
const DefaultReplayWindow = 5 * time.Minute

// DefaultRateLimitPerMinute is the message limit when rate_limit_per_minute
// is not set. It matches the panel's 10 per minute: enough for a person
// tapping commands on a phone, not enough for a script with a stolen client
// token to queue a flood of them.
const DefaultRateLimitPerMinute = 10

// CommandsPerMinute returns the message rate limit, with 0 replaced by
// DefaultRateLimitPerMinute.
func (d Defaults) CommandsPerMinute() int {
	if d.RateLimitPerMinute == 0 {
		return DefaultRateLimitPerMinute
	}
	return d.RateLimitPerMinute
}

// SSHTarget defines how to connect to a remote machine via SSH.
type SSHTarget struct {
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	User    string `yaml:"user"`
	KeyFile string `yaml:"key_file"`

	// KnownHostsFile is the known_hosts file the host key is checked
	// against. Empty means ~/.ssh/known_hosts of the user running Gotify,
	// which a service or a container user often does not have.
	KnownHostsFile string `yaml:"known_hosts_file"`

	// OS is the operating system of the machine, "linux" or "darwin". It
	// picks the commands that free, top, ip, ports, connections, updates
	// and logs run there, because macOS has none of free, ss, apt or
	// journalctl. Empty means "linux", the commands every target ran
	// before this field existed.
	OS string `yaml:"os"`
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
			Timeout:      30 * time.Second,
			LogLines:     30,
			ReplayWindow: DefaultReplayWindow,
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
	// Gotify application IDs start at 1. A zero or negative ID matches no
	// message, so the plugin would listen and never run anything.
	if cfg.Gotify.CommandAppID <= 0 {
		return fmt.Errorf("gotify.command_app_id must be an application ID greater than 0, got %d", cfg.Gotify.CommandAppID)
	}

	// A negative limit would refuse every command; 0 is the way to ask for
	// the default, so anything below it is a typo worth reporting.
	if cfg.Defaults.RateLimitPerMinute < 0 {
		return fmt.Errorf("defaults.rate_limit_per_minute must be 0 (the default, %d) or more, got %d", DefaultRateLimitPerMinute, cfg.Defaults.RateLimitPerMinute)
	}

	// 0 is the way to turn the replay off, so a negative window can only be
	// a typo.
	if cfg.Defaults.ReplayWindow < 0 {
		return fmt.Errorf("defaults.replay_window must be 0 (no replay) or a positive duration, got %s", cfg.Defaults.ReplayWindow)
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

	if err := validateTargetOS(cfg); err != nil {
		return err
	}

	return nil
}

// ValidateCategoryCommands checks that every command listed in
// categories[].commands is one of known, the names the command registry
// handles. It is separate from Validate because the registry lives in a
// package that imports this one.
func ValidateCategoryCommands(cfg *Config, known []string) error {
	knownSet := make(map[string]bool, len(known))
	for _, name := range known {
		knownSet[name] = true
	}

	// A misspelt name would put a button on the panel that only ever
	// answers "unknown command", so it is refused here instead.
	for _, category := range cfg.Categories {
		for _, name := range category.Commands {
			if !knownSet[name] {
				return fmt.Errorf("category %q: unknown command %q", category.Label, name)
			}
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
