package commander

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"strings"
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
		sshExec.KnownHostsFile = macTarget.KnownHostsFile
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

// internalErrorTitle and internalErrorPriority mark the reply sent when a
// command panics, so the user knows the command did not just go missing.
const (
	internalErrorTitle    = "⚠️ Internal Error"
	internalErrorPriority = 8
)

// HandleCommand is the primary entry point. It parses the raw input, enforces
// security, dispatches to the appropriate handler, and delivers the response.
func (c *Commander) HandleCommand(input string) {
	c.Handle(context.Background(), input)
}

// Handle is HandleCommand with a context, and with Busy it lets the stream
// listener hand commands to the Commander directly. A command whose context
// is already cancelled (the plugin is being disabled) is dropped without a
// reply. A panic in a command is recovered, logged with its stack and
// answered with an internal error.
func (c *Commander) Handle(ctx context.Context, input string) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		log.Printf("[commander] panic running %q: %v\n%s", commandWord(input), recovered, debug.Stack())
		resp := internalErrorResponse(input)
		c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)
	}()

	resp, executed := c.run(ctx, input)
	if !executed && ctx.Err() != nil {
		log.Printf("[commander] dropped %q: %v", commandWord(input), ctx.Err())
		return
	}

	c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)
}

// Busy tells the user that input was refused because too many commands are
// waiting. The stream listener calls it from its read loop, so it only sends.
func (c *Commander) Busy(input string) {
	message := fmt.Sprintf("Too many commands are waiting; %q was not run. Try again in a moment.", input)
	c.sender.Send("⏳ Busy", message, 5, false)
}

// Execute runs a command and returns the response synchronously.
// Also sends the response via Gotify notification. Rejections (parse,
// whitelist, unknown command) are only returned, as before.
func (c *Commander) Execute(input string) (resp command.Response) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		log.Printf("[commander] panic running %q: %v\n%s", commandWord(input), recovered, debug.Stack())
		resp = internalErrorResponse(input)
	}()

	resp, executed := c.run(context.Background(), input)
	if !executed {
		return resp
	}

	// The caller is an HTTP handler waiting for this response; the Gotify
	// copy goes out in the background so a slow Send does not delay it.
	go c.sendDetached(resp)

	return resp
}

// run is the path HandleCommand and Execute share: parse, security checks,
// handler lookup, execution and stats. executed reports whether a command
// handler ran; when it is false, resp is the rejection to show the user.
// The context is checked before the handler starts; the builtins do not
// take a context yet, so a command already running is not interrupted.
func (c *Commander) run(ctx context.Context, input string) (resp command.Response, executed bool) {
	// 1. Parse.
	cmd, err := command.Parse(input, c.aliases, c.services)
	if err != nil {
		return rejection("❌ Parse Error", fmt.Sprintf("Could not parse command: %s", err.Error()), 7), false
	}

	// 2. Security checks for non-machine targets.
	if cmd.Target != "" && cmd.Target != "vps" && cmd.Target != "mac" {
		if !c.whitelist.IsAllowed(cmd.Target) {
			return rejection("❌ Forbidden", fmt.Sprintf("Service %q is not allowed.", cmd.Target), 8), false
		}
		if err := security.ValidateInput(cmd.Target); err != nil {
			return rejection("❌ Invalid Input", fmt.Sprintf("Rejected: %s", err.Error()), 8), false
		}
	}

	// 3. Look up handler.
	c.mu.RLock()
	handler, ok := c.registry.Lookup(cmd.Action)
	c.mu.RUnlock()

	if !ok {
		return rejection("❌ Unknown Command", fmt.Sprintf("No handler registered for action %q.", cmd.Action), 7), false
	}

	// 4. A command queued before the plugin was disabled must not run after.
	if err := ctx.Err(); err != nil {
		return rejection("⏹ Cancelled", fmt.Sprintf("Command %q was cancelled: %s", input, err.Error()), 5), false
	}

	// 5. Log the action and target only. The raw input can carry private
	// arguments, such as the coordinates of locate.
	log.Printf("[commander] action=%s target=%q", cmd.Action, cmd.Target)

	// 6. Execute.
	resp = handler(cmd)

	// 7. Update stats.
	c.mu.Lock()
	c.commandCount++
	c.lastCommand = string(cmd.Action)
	if cmd.Target != "" {
		c.lastCommand += " " + cmd.Target
	}
	c.lastTime = time.Now()
	c.mu.Unlock()

	return resp, true
}

// sendDetached delivers resp from its own goroutine; a panic in the sender
// is logged instead of ending the Gotify process.
func (c *Commander) sendDetached(resp command.Response) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		log.Printf("[commander] panic sending %q: %v\n%s", resp.Title, recovered, debug.Stack())
	}()

	c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)
}

// Stats returns thread-safe counters and timing info.
func (c *Commander) Stats() (count int, last string, lastTime time.Time, uptime time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.commandCount, c.lastCommand, c.lastTime, time.Since(c.startTime)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// rejection builds the response for an input that was not run.
func rejection(title, message string, priority int) command.Response {
	return command.Response{Title: title, Message: message, Priority: priority}
}

// internalErrorResponse is the reply for a command that panicked. The panic
// value stays in the log: it may name paths or internals the user should
// not see in a notification.
func internalErrorResponse(input string) command.Response {
	message := fmt.Sprintf("⚠️ internal error while running %q. The details are in the Gotify log.", input)
	return rejection(internalErrorTitle, message, internalErrorPriority)
}

// maxLoggedWord bounds the word commandWord returns, so a long line of
// garbage sent to the command app does not end up whole in the log.
const maxLoggedWord = 32

// commandWord returns the first word of input for a log line. The rest is
// left out: arguments can be private (locate carries the user's position),
// and once the input is parsed run logs the action and target on their own.
func commandWord(input string) string {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return ""
	}

	word := []rune(fields[0])
	if len(word) > maxLoggedWord {
		word = word[:maxLoggedWord]
	}
	return string(word)
}

// CommandNames returns the names of the built-in commands, the ones a
// configuration may list in categories[].commands.
func CommandNames() []string {
	registry, _ := buildRegistry(config.DefaultConfig(), nil)
	return registry.Names()
}

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
