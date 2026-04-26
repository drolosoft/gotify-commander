package commander

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/security"
)

// Sender is any sink that can deliver a notification.
type Sender interface {
	Send(title, message string, priority int, markdown bool)
}

// Commander orchestrates parse → security → execute → respond.
type Commander struct {
	cfg      *config.Config
	exec     executor.Executor
	sshExec  executor.Executor
	sender   Sender
	registry *command.Registry

	aliases   map[string]string
	services  map[string]bool
	whitelist *security.Whitelist

	mu           sync.RWMutex
	commandCount int
	lastCommand  string
	lastTime     time.Time
	startTime    time.Time
}

// New creates a Commander, wires all 23 built-in commands, and starts the
// uptime clock. If cfg.SSHTargets["mac"] is configured, an SSHExecutor is
// created automatically and wired into the builtins for Mac targets.
func New(cfg *config.Config, exec executor.Executor, sender Sender) *Commander {
	aliases := config.BuildAliasMap(cfg)

	services := make(map[string]bool, len(cfg.Services))
	for name := range cfg.Services {
		services[name] = true
	}

	registry, builtins := buildRegistry(cfg, exec)

	c := &Commander{
		cfg:       cfg,
		exec:      exec,
		sender:    sender,
		registry:  registry,
		aliases:   aliases,
		services:  services,
		whitelist: security.NewWhitelist(services),
		startTime: time.Now(),
	}

	// Auto-wire SSH executor for Mac Mini if the target is configured.
	if macTarget, ok := cfg.SSHTargets["mac"]; ok {
		sshExec := executor.NewSSHExecutor(macTarget)
		c.sshExec = sshExec
		builtins.SetSSHExecutor(sshExec)
	}

	return c
}

// SetSSHExecutor stores the SSH executor and forwards it to the builtins if
// they expose SetSSHExecutor.
func (c *Commander) SetSSHExecutor(exec executor.Executor) {
	c.mu.Lock()
	c.sshExec = exec
	c.mu.Unlock()

	// Re-build the registry so builtins receive the SSH executor.
	// buildRegistry already calls registerAll internally, so no extra call needed.
	registry, builtins := buildRegistry(c.cfg, c.exec)
	builtins.SetSSHExecutor(exec)

	c.mu.Lock()
	c.registry = registry
	c.mu.Unlock()
}

// HandleCommand is the primary entry point. It parses the raw input, enforces
// security, dispatches to the appropriate handler, and delivers the response.
func (c *Commander) HandleCommand(input string) {
	// 1. Parse.
	cmd, err := command.Parse(input, c.aliases, c.services)
	if err != nil {
		c.sender.Send("❌ Parse Error", fmt.Sprintf("Could not parse command: %s", err.Error()), 7, false)
		return
	}

	// 2. Security checks for non-machine targets.
	if cmd.Target != "" && cmd.Target != "vps" && cmd.Target != "mac" {
		if !c.whitelist.IsAllowed(cmd.Target) {
			c.sender.Send("❌ Forbidden", fmt.Sprintf("Service %q is not allowed.", cmd.Target), 8, false)
			return
		}
		if err := security.ValidateInput(cmd.Target); err != nil {
			c.sender.Send("❌ Invalid Input", fmt.Sprintf("Rejected: %s", err.Error()), 8, false)
			return
		}
	}

	// 3. Look up handler.
	c.mu.RLock()
	handler, ok := c.registry.Lookup(cmd.Action)
	c.mu.RUnlock()

	if !ok {
		c.sender.Send("❌ Unknown Command", fmt.Sprintf("No handler registered for action %q.", cmd.Action), 7, false)
		return
	}

	// 4. Log.
	log.Printf("[commander] action=%s target=%q raw=%q", cmd.Action, cmd.Target, cmd.Raw)

	// 5. Execute.
	resp := handler(cmd)

	// 6. Update stats.
	c.mu.Lock()
	c.commandCount++
	c.lastCommand = string(cmd.Action)
	if cmd.Target != "" {
		c.lastCommand += " " + cmd.Target
	}
	c.lastTime = time.Now()
	c.mu.Unlock()

	// 7. Send response.
	c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)
}

// Execute runs a command and returns the response synchronously.
// Also sends the response via Gotify notification.
func (c *Commander) Execute(input string) command.Response {
	cmd, err := command.Parse(input, c.aliases, c.services)
	if err != nil {
		return command.Response{Title: "❌ Error", Message: err.Error(), Priority: 7}
	}

	if cmd.Target != "" && cmd.Target != "vps" && cmd.Target != "mac" {
		if !c.whitelist.IsAllowed(cmd.Target) {
			return command.Response{Title: "❌ Forbidden", Message: fmt.Sprintf("Service %q not allowed", cmd.Target), Priority: 8}
		}
		if err := security.ValidateInput(cmd.Target); err != nil {
			return command.Response{Title: "❌ Invalid", Message: err.Error(), Priority: 8}
		}
	}

	c.mu.RLock()
	handler, ok := c.registry.Lookup(cmd.Action)
	c.mu.RUnlock()
	if !ok {
		return command.Response{Title: "❌ Unknown", Message: fmt.Sprintf("Unknown command: %s", cmd.Action), Priority: 7}
	}

	log.Printf("[commander] action=%s target=%q raw=%q", cmd.Action, cmd.Target, cmd.Raw)
	resp := handler(cmd)

	c.mu.Lock()
	c.commandCount++
	c.lastCommand = string(cmd.Action)
	if cmd.Target != "" {
		c.lastCommand += " " + cmd.Target
	}
	c.lastTime = time.Now()
	c.mu.Unlock()

	// Also send via Gotify notification
	go c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)

	return resp
}

// Stats returns thread-safe counters and timing info.
func (c *Commander) Stats() (count int, last string, lastTime time.Time, uptime time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.commandCount, c.lastCommand, c.lastTime, time.Since(c.startTime)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// buildRegistry creates a fresh Builtins+Registry pair with all 23 commands
// registered. It returns both so the caller can wire SSH after the fact.
func buildRegistry(cfg *config.Config, exec executor.Executor) (*command.Registry, *command.Builtins) {
	b := command.NewBuiltins(cfg, exec)
	r := command.NewRegistry()
	registerAll(r, b)
	return r, b
}

// registerAll wires every Action constant to the matching Builtins method.
func registerAll(r *command.Registry, b *command.Builtins) {
	r.Register(command.ActionHelp, b.Help)
	r.Register(command.ActionPing, b.Ping)
	r.Register(command.ActionServices, b.Services)
	r.Register(command.ActionRestart, b.Restart)
	r.Register(command.ActionStop, b.Stop)
	r.Register(command.ActionStart, b.Start)
	r.Register(command.ActionStatus, b.Status)
	r.Register(command.ActionFree, b.Free)
	r.Register(command.ActionDf, b.Df)
	r.Register(command.ActionUptime, b.Uptime)
	r.Register(command.ActionWho, b.Who)
	r.Register(command.ActionLogs, b.Logs)
	r.Register(command.ActionReboot, b.Reboot)
	r.Register(command.ActionShutdown, b.Shutdown)
	r.Register(command.ActionTop, b.Top)
	r.Register(command.ActionPorts, b.Ports)
	r.Register(command.ActionIp, b.Ip)
	r.Register(command.ActionUpdates, b.Updates)
	r.Register(command.ActionCerts, b.Certs)
	r.Register(command.ActionConnections, b.Connections)
	r.Register(command.ActionTraffic, b.Traffic)
	r.Register(command.ActionAnalytics, b.Analytics)
	r.Register(command.ActionLocate, b.Locate)
}
