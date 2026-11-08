package commander

import (
	"context"
	"fmt"
	"log"
	"math"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/ratelimit"
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

	// messageLimiter counts the commands that arrive as Gotify messages,
	// through Handle. The panel's POST /execute has its own limiter in front
	// of Execute, so the two paths have separate budgets.
	messageLimiter *ratelimit.Limiter

	// dangerous holds the actions that can take a machine down, and
	// dangerSlot lets only one of them run at a time, from either path.
	dangerous  map[command.Action]bool
	dangerSlot sync.Mutex

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

		messageLimiter: ratelimit.New(cfg.Defaults.CommandsPerMinute(), time.Minute),
		dangerous:      dangerousActions(cfg),
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

// rateLimitedTitle and rateLimitedPriority mark the reply to a message that
// came over the limit. The priority matches Busy: the command was not run,
// nothing is wrong with the server.
const (
	rateLimitedTitle    = "⏳ Rate limited"
	rateLimitedPriority = 5
)

// dangerBusyTitle, dangerBusyMessage and dangerBusyPriority mark the refusal
// of a dangerous command while another one is still running.
const (
	dangerBusyTitle    = "⚠️ Busy"
	dangerBusyMessage  = "⚠️ Another dangerous command is still running"
	dangerBusyPriority = 7
)

// HandleCommand is the primary entry point. It parses the raw input, enforces
// security, dispatches to the appropriate handler, and delivers the response.
func (c *Commander) HandleCommand(input string) {
	c.Handle(context.Background(), input)
}

// Handle is HandleCommand with a context, and with Busy it lets the stream
// listener hand commands to the Commander directly. Messages are rate
// limited (defaults.rate_limit_per_minute, 20 by default): one over the
// limit is answered with how long to wait and is not run. A command whose
// context is already cancelled (the plugin is being disabled) is dropped
// without a reply. A panic in a command is recovered, logged with its stack
// and answered with an internal error.
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

	// Any client token of the user can post to the command app and fill
	// the listener's queue, so messages are counted before anything is
	// parsed or run.
	allowed, retryAfter := c.messageLimiter.Allow()
	if !allowed {
		c.refuseRateLimited(ctx, input, retryAfter)
		return
	}

	resp, executed := c.run(ctx, input)
	if !executed && ctx.Err() != nil {
		log.Printf("[commander] dropped %q: %v", commandWord(input), ctx.Err())
		return
	}

	c.sender.Send(resp.Title, resp.Message, resp.Priority, resp.Markdown)
}

// refuseRateLimited answers a message that came over the limit with the
// whole seconds until a new one fits. Like any other refusal, it is dropped
// without a reply when the plugin is being disabled.
func (c *Commander) refuseRateLimited(ctx context.Context, input string, retryAfter time.Duration) {
	if err := ctx.Err(); err != nil {
		log.Printf("[commander] dropped %q: %v", commandWord(input), err)
		return
	}

	log.Printf("[commander] rate limited %q", commandWord(input))

	seconds := int(math.Ceil(retryAfter.Seconds()))
	message := fmt.Sprintf("⏳ Too many commands, try again in %d s", seconds)
	c.sender.Send(rateLimitedTitle, message, rateLimitedPriority, false)
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
// handler lookup, the one-at-a-time rule for dangerous commands, execution
// and stats. executed reports whether a command handler ran; when it is
// false, resp is the rejection to show the user.
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

	// 5. Only one command that can take a machine down runs at a time. A
	// second reboot sent while the first is still waiting for the host is
	// refused instead of queued behind it. The deferred unlock also runs
	// when the handler panics.
	if c.dangerous[cmd.Action] {
		if !c.dangerSlot.TryLock() {
			return rejection(dangerBusyTitle, dangerBusyMessage, dangerBusyPriority), false
		}
		defer c.dangerSlot.Unlock()
	}

	// 6. Log the action and target only. The raw input can carry private
	// arguments, such as the coordinates of locate.
	log.Printf("[commander] action=%s target=%q", cmd.Action, cmd.Target)

	// 7. Execute.
	resp = handler(cmd)

	// 8. Update stats.
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

// Stats is a copy of the counters of a Commander. The plugin builds a new
// Commander whenever the configuration is saved and hands it the old one's
// Stats, so the plugin page and /health do not go back to zero.
type Stats struct {
	CommandCount int
	LastCommand  string
	LastTime     time.Time

	// StartTime is when the plugin was enabled, not when this Commander was
	// built; the uptime counts from it.
	StartTime time.Time
}

// Snapshot returns a copy of the counters, for SetStats on a new Commander.
func (c *Commander) Snapshot() Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return Stats{
		CommandCount: c.commandCount,
		LastCommand:  c.lastCommand,
		LastTime:     c.lastTime,
		StartTime:    c.startTime,
	}
}

// SetStats replaces the counters with stats, which usually come from the
// Snapshot of the Commander this one replaces.
func (c *Commander) SetStats(stats Stats) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.commandCount = stats.CommandCount
	c.lastCommand = stats.LastCommand
	c.lastTime = stats.LastTime
	c.startTime = stats.StartTime
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

// dangerousActions returns reboot and shutdown, which are always dangerous,
// plus every command listed in a category with danger: true. The panel asks
// for confirmation before those; here they share the danger slot.
func dangerousActions(cfg *config.Config) map[command.Action]bool {
	dangerous := map[command.Action]bool{
		command.ActionReboot:   true,
		command.ActionShutdown: true,
	}

	for _, category := range cfg.Categories {
		if !category.Danger {
			continue
		}

		for _, name := range category.Commands {
			dangerous[command.Action(name)] = true
		}
	}

	return dangerous
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
