package config

import (
	"fmt"
	"strings"
	"time"
	"unicode"

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
	Docker      DockerConfig         `yaml:"docker"`
	Digest      DigestConfig         `yaml:"digest"`
	WebURL      string               `yaml:"-"` // full control panel URL, set at runtime

	// Which commands run, and which ones a message must confirm first.
	//
	// EnabledCommands lists the command names that run, from either the
	// panel or a message; left out or empty, every command runs. One
	// outside the list is answered as disabled and not counted.
	//
	// RequireConfirm turns on the confirmation by message for the commands
	// it lists: a Gotify message with one of them is not run at once, the
	// reply carries a code, and "<keyword> <code>" within the window runs
	// it. Left out, null or empty, no command asks. Confirmation words and
	// timing come from Confirmation. The panel asks in the browser instead.
	EnabledCommands []string           `yaml:"enabled_commands"`
	RequireConfirm  []string           `yaml:"require_confirm"`
	Confirmation    ConfirmationConfig `yaml:"confirmation"`
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
// is not set. Twenty a minute is enough for a person tapping commands on a
// phone, confirmations included, and not enough for a script with a stolen
// client token to queue a flood of them. The panel's limit is separate.
const DefaultRateLimitPerMinute = 20

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

		// Written out so the configuration Gotify shows on a new plugin
		// names the three settings; they only matter once require_confirm
		// lists a command.
		Confirmation: ConfirmationConfig{
			Keyword: DefaultConfirmationKeyword,
			Prompt:  DefaultConfirmationPrompt,
			Window:  DefaultConfirmationWindow,
		},
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

	if err := validateConfirmation(cfg); err != nil {
		return err
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

	// The plugin calls this one function with the registry's names, so the
	// two top-level command lists are checked from here as well.
	return ValidateCommandLists(cfg, known)
}

// ─── enabled_commands and require_confirm ────────────────────────────────────

// ConfirmationConfig is the wording and timing of the confirmation by
// message. Every field may be left empty for its default.
type ConfirmationConfig struct {
	// Keyword is the word the user replies with, before the code. It is
	// read before any command name, so it must not be one.
	Keyword string `yaml:"keyword"`

	// Prompt is the reply that asks for the code. {command}, {keyword},
	// {code} and {seconds} are replaced; {code} is required.
	Prompt string `yaml:"prompt"`

	// Window is how long a code works. Like timeout, it needs a unit in
	// YAML; 0 means DefaultConfirmationWindow.
	Window time.Duration `yaml:"window"`
}

// DefaultConfirmationKeyword is the reply word when confirmation.keyword is
// empty.
const DefaultConfirmationKeyword = "confirm"

// DefaultConfirmationPrompt is the request message when confirmation.prompt
// is empty.
const DefaultConfirmationPrompt = "⚠️ Confirm `{command}`: reply `{keyword} {code}` within {seconds} s"

// DefaultConfirmationWindow is the life of a code when confirmation.window
// is 0. A minute is enough to read the reply on a phone and type four
// digits; a code found later in the message history no longer runs anything.
const DefaultConfirmationWindow = 60 * time.Second

// KeywordOrDefault returns the keyword, or DefaultConfirmationKeyword when
// it is empty.
func (c ConfirmationConfig) KeywordOrDefault() string {
	if c.Keyword == "" {
		return DefaultConfirmationKeyword
	}
	return c.Keyword
}

// PromptOrDefault returns the prompt, or DefaultConfirmationPrompt when it
// is empty.
func (c ConfirmationConfig) PromptOrDefault() string {
	if c.Prompt == "" {
		return DefaultConfirmationPrompt
	}
	return c.Prompt
}

// WindowOrDefault returns the window, or DefaultConfirmationWindow when it
// is 0.
func (c ConfirmationConfig) WindowOrDefault() time.Duration {
	if c.Window == 0 {
		return DefaultConfirmationWindow
	}
	return c.Window
}

// validateConfirmation checks the confirmation block against what this
// package knows: the shape of the keyword, the services it could shadow,
// the prompt and the window. ValidateCommandLists checks the keyword
// against the command names, which live in the registry.
func validateConfirmation(cfg *Config) error {
	confirmation := cfg.Confirmation
	keyword := confirmation.KeywordOrDefault()

	if !isKeyword(keyword) {
		return fmt.Errorf("confirmation.keyword must be one lowercase word starting with a letter, got %q", keyword)
	}

	// A bare service name runs "status <service>", and the keyword is read
	// first, so the two cannot share a word. It only matters while some
	// command asks for confirmation.
	if len(cfg.RequireConfirm) > 0 {
		if _, ok := BuildAliasMap(cfg)[keyword]; ok {
			return fmt.Errorf("confirmation.keyword %q is also a service name or alias", keyword)
		}
	}

	// Without the code in the reply nobody could confirm anything.
	if !strings.Contains(confirmation.PromptOrDefault(), "{code}") {
		return fmt.Errorf("confirmation.prompt must contain {code}, got %q", confirmation.Prompt)
	}

	// 0 asks for the default; the reply counts whole seconds, so anything
	// shorter than one would read "within 0 s".
	if confirmation.Window < 0 || confirmation.Window > 0 && confirmation.Window < time.Second {
		return fmt.Errorf("confirmation.window must be 0 (the default, %s) or at least 1s, got %s", DefaultConfirmationWindow, confirmation.Window)
	}

	return nil
}

// isKeyword reports whether word is one lowercase word: a lowercase letter
// first, then lowercase letters or digits. The parser lowercases the first
// word of a message, so an upper-case keyword could never match.
func isKeyword(word string) bool {
	for index, character := range word {
		if unicode.IsLower(character) {
			continue
		}
		if index > 0 && unicode.IsDigit(character) {
			continue
		}
		return false
	}

	return word != ""
}

// ValidateCommandLists checks that every name in enabled_commands and
// require_confirm is one of known, the names the command registry handles.
// A typo in enabled_commands would disable the command the user meant, and
// one in require_confirm would let it run without asking.
func ValidateCommandLists(cfg *Config, known []string) error {
	knownSet := make(map[string]bool, len(known))
	for _, name := range known {
		knownSet[name] = true
	}

	for _, name := range cfg.EnabledCommands {
		if !knownSet[name] {
			return fmt.Errorf("enabled_commands: unknown command %q", name)
		}
	}

	for _, name := range cfg.RequireConfirm {
		if !knownSet[name] {
			return fmt.Errorf("require_confirm: unknown command %q", name)
		}
	}

	// The keyword is read before the command names, so one named like a
	// command would take that command's place.
	if keyword := cfg.Confirmation.KeywordOrDefault(); len(cfg.RequireConfirm) > 0 && knownSet[keyword] {
		return fmt.Errorf("confirmation.keyword %q is also a command name", keyword)
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

// ─── Operations: docker and digest ──────────────────────────────────────────

// DockerConfig limits the docker command to the containers named here.
type DockerConfig struct {
	// AllowedContainers are the only container names that docker ps shows
	// and docker restart and docker logs accept. Empty means none: a stolen
	// client token must not be able to restart every container on the host.
	AllowedContainers []string `yaml:"allowed_containers"`
}

// DefaultDigestAt is the local time the daily digest goes out when digest.at
// is not set: early enough to read before the working day.
const DefaultDigestAt = "07:30"

// DigestConfig schedules the daily health digest.
type DigestConfig struct {
	// Enabled turns the daily message on. The digest command works either
	// way.
	Enabled bool `yaml:"enabled"`

	// At is the time of day, "15:04" in 24-hour form. Empty means
	// DefaultDigestAt.
	At string `yaml:"at"`

	// Timezone is an IANA name such as "Europe/Madrid". Empty means the
	// timezone of the host that runs Gotify.
	Timezone string `yaml:"timezone"`
}
