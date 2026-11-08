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
	// of Execute, so the two paths have separate budgets. messageLimit is
	// the limit it was built with, for Inherit. Read the limiter through
	// limiter, since Inherit may replace it.
	messageLimiter *ratelimit.Limiter
	messageLimit   int

	// dangerous holds the actions that can take a machine down, and
	// dangerSlot lets only one of them run at a time, from either path. It
	// is a pointer so a Commander that replaces this one can share it; read
	// it through slot.
	dangerous  map[command.Action]bool
	dangerSlot *sync.Mutex

	// enabled is the set of enabled_commands, nil when every command is
	// enabled. confirm is the set of require_confirm, and confirmations
	// holds the one message command waiting for its code.
	enabled       map[command.Action]bool
	confirm       map[command.Action]bool
	confirmations *command.Confirmations

	mu           sync.RWMutex
	commandCount int
	lastCommand  string
	lastTime     time.Time
	startTime    time.Time
}

// New creates a Commander, wires every built-in and operations command, and starts the
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
		messageLimit:   cfg.Defaults.CommandsPerMinute(),
		dangerous:      dangerousActions(cfg),
		dangerSlot:     &sync.Mutex{},

		enabled:       enabledActions(cfg),
		confirm:       actionSet(cfg.ConfirmCommands()),
		confirmations: command.NewConfirmations(time.Now),
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

// confirmationTitle and confirmationPriority mark the reply that asks for a
// code before a dangerous command runs. It is as loud as the refusals: the
// command has not run and the user has to act.
const (
	confirmationTitle    = "⚠️ Confirmation needed"
	confirmationPriority = 7
)

// noPendingTitle, noPendingMessage and noPendingPriority mark the reply to a
// confirm whose code is wrong, already used or expired.
const (
	noPendingTitle    = "❌ Not confirmed"
	noPendingMessage  = "❌ No pending command for that code"
	noPendingPriority = 5
)

// disabledTitle, disabledMessage and disabledPriority mark the refusal of a
// command left out of enabled_commands. The administrator chose it, so it is
// not an alarm.
const (
	disabledTitle    = "🚫 Disabled"
	disabledMessage  = "🚫 Command disabled by configuration"
	disabledPriority = 5
)

// confirmNeedsTarget are the actions whose handler only answers with its
// usage when the target is missing. Without a target there is nothing that
// could go wrong, so no code is asked for.
var confirmNeedsTarget = map[command.Action]bool{
	command.ActionReboot:   true,
	command.ActionShutdown: true,
	command.ActionStop:     true,
}

// HandleCommand is the primary entry point. It parses the raw input, enforces
// security, dispatches to the appropriate handler, and delivers the response.
func (c *Commander) HandleCommand(input string) {
	c.Handle(context.Background(), input)
}

// Handle is HandleCommand with a context, and with Busy it lets the stream
// listener hand commands to the Commander directly. Messages are rate
// limited (defaults.rate_limit_per_minute, 20 by default): one over the
// limit is answered with how long to wait and is not run. A command listed
// in require_confirm is not run either: the reply carries a code, and the
// message "confirm <code>" runs it (see runMessage). A command whose
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
	allowed, retryAfter := c.limiter().Allow()
	if !allowed {
		c.refuseRateLimited(ctx, input, retryAfter)
		return
	}

	resp, executed := c.runMessage(ctx, input)
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

// run is the path of Execute: parse, then runCommand. executed reports
// whether a command handler ran; when it is false, resp is the rejection to
// show the user. The panel asks for confirmation in the browser, so this
// path does not go through require_confirm; messages take runMessage.
func (c *Commander) run(ctx context.Context, input string) (resp command.Response, executed bool) {
	cmd, err := command.Parse(input, c.aliases, c.services)
	if err != nil {
		return parseRejection(err), false
	}

	return c.runCommand(ctx, cmd)
}

// runMessage is run for a command that arrived as a Gotify message. A
// "confirm <code>" runs the command waiting for that code. A command in
// require_confirm that would pass every check is stored with a new code and
// answered with it instead of running; the checks come first so the user is
// not asked to confirm something that would then be refused.
func (c *Commander) runMessage(ctx context.Context, input string) (resp command.Response, executed bool) {
	cmd, err := command.Parse(input, c.aliases, c.services)
	if err != nil {
		return parseRejection(err), false
	}

	if cmd.Action == command.ActionConfirm {
		return c.runConfirmed(ctx, cmd.Target)
	}

	if _, refusal, ok := c.check(cmd); !ok {
		return refusal, false
	}

	if c.needsConfirmation(cmd) {
		return c.requestConfirmation(cmd), false
	}

	return c.runCommand(ctx, cmd)
}

// runConfirmed runs the pending command whose code is code. It goes through
// runCommand like any other, so the checks, the cancelled context and the
// danger slot still apply when the confirmation arrives.
func (c *Commander) runConfirmed(ctx context.Context, code string) (resp command.Response, executed bool) {
	cmd, ok := c.confirmations.Take(code)
	if !ok {
		log.Printf("[commander] confirm without a pending command for its code")
		return rejection(noPendingTitle, noPendingMessage, noPendingPriority), false
	}

	log.Printf("[commander] confirmed action=%s target=%q", cmd.Action, cmd.Target)
	return c.runCommand(ctx, cmd)
}

// needsConfirmation reports whether cmd, sent as a message, must wait for
// a code first.
func (c *Commander) needsConfirmation(cmd command.Command) bool {
	if !c.confirm[cmd.Action] {
		return false
	}

	return cmd.Target != "" || !confirmNeedsTarget[cmd.Action]
}

// requestConfirmation stores cmd as the pending command and builds the
// reply with its code. When it replaces a pending command, the reply says
// so, because that command's code stops working.
func (c *Commander) requestConfirmation(cmd command.Command) command.Response {
	code, replaced := c.confirmations.Request(cmd)

	log.Printf("[commander] confirmation requested action=%s target=%q", cmd.Action, cmd.Target)

	seconds := int(command.ConfirmationWindow.Seconds())
	message := fmt.Sprintf("⚠️ Confirm `%s`: reply `confirm %s` within %d s", describe(cmd), code, seconds)
	if replaced != nil {
		message += fmt.Sprintf("\n\nThis replaces `%s`, whose code no longer works.", describe(*replaced))
	}

	return command.Response{
		Title:    confirmationTitle,
		Message:  message,
		Priority: confirmationPriority,
		Markdown: true,
	}
}

// check is what every command passes before it runs: enabled_commands, the
// service whitelist, the input filter and a registered handler. ok is false
// when cmd is refused, and refusal is then the reply.
func (c *Commander) check(cmd command.Command) (handler command.Handler, refusal command.Response, ok bool) {
	// confirm only means something to runMessage; one that reaches this
	// point came from the panel, which has nothing pending.
	if cmd.Action == command.ActionConfirm {
		return nil, rejection(noPendingTitle, noPendingMessage, noPendingPriority), false
	}

	// A disabled command is refused before anything else looks at it, so
	// the reply does not depend on its target.
	if c.enabled != nil && !c.enabled[cmd.Action] {
		return nil, rejection(disabledTitle, disabledMessage, disabledPriority), false
	}

	// Security checks for non-machine targets.
	if cmd.Target != "" && cmd.Target != "vps" && cmd.Target != "mac" {
		if !c.whitelist.IsAllowed(cmd.Target) {
			return nil, rejection("❌ Forbidden", fmt.Sprintf("Service %q is not allowed.", cmd.Target), 8), false
		}
		if err := security.ValidateInput(cmd.Target); err != nil {
			return nil, rejection("❌ Invalid Input", fmt.Sprintf("Rejected: %s", err.Error()), 8), false
		}
	}

	c.mu.RLock()
	handler, found := c.registry.Lookup(cmd.Action)
	c.mu.RUnlock()

	if !found {
		return nil, rejection("❌ Unknown Command", fmt.Sprintf("No handler registered for action %q.", cmd.Action), 7), false
	}

	return handler, command.Response{}, true
}

// runCommand checks cmd, applies the one-at-a-time rule for dangerous
// commands, runs it and updates the stats. executed reports whether the
// handler ran.
// The context is checked before the handler starts; the builtins do not
// take a context yet, so a command already running is not interrupted.
func (c *Commander) runCommand(ctx context.Context, cmd command.Command) (resp command.Response, executed bool) {
	handler, refusal, ok := c.check(cmd)
	if !ok {
		return refusal, false
	}

	// A command queued before the plugin was disabled must not run after.
	if err := ctx.Err(); err != nil {
		return rejection("⏹ Cancelled", fmt.Sprintf("Command %q was cancelled: %s", cmd.Raw, err.Error()), 5), false
	}

	// Only one command that can take a machine down runs at a time. A
	// second reboot sent while the first is still waiting for the host is
	// refused instead of queued behind it. The slot is read once, so the
	// unlock goes to the same mutex even if Inherit swaps it meanwhile. The
	// deferred unlock also runs when the handler panics.
	if c.dangerous[cmd.Action] {
		dangerSlot := c.slot()
		if !dangerSlot.TryLock() {
			return rejection(dangerBusyTitle, dangerBusyMessage, dangerBusyPriority), false
		}
		defer dangerSlot.Unlock()
	}

	// Log the action and target only. The raw input can carry private
	// arguments, such as the coordinates of locate.
	log.Printf("[commander] action=%s target=%q", cmd.Action, cmd.Target)

	resp = handler(cmd)

	c.mu.Lock()
	c.commandCount++
	c.lastCommand = describe(cmd)
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

// Inherit makes c carry on from old, the Commander it replaces when the
// configuration is saved: the stats, the danger slot and, while the limit is
// the same, the message limiter. Sharing the slot keeps a reboot still
// running on old from letting a second one start here; keeping the limiter
// means a save is not a way around the minute's budget. When the limit
// changed, c keeps its own limiter so the new value applies at once. The
// pending confirmation is not carried: it was asked under the old
// configuration, so its code just stops working. Call it before c handles
// anything.
func (c *Commander) Inherit(old *Commander) {
	if old == nil || old == c {
		return
	}

	c.SetStats(old.Snapshot())

	old.mu.RLock()
	limiter := old.messageLimiter
	limit := old.messageLimit
	dangerSlot := old.dangerSlot
	old.mu.RUnlock()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.dangerSlot = dangerSlot
	if limit == c.messageLimit {
		c.messageLimiter = limiter
	}
}

// limiter returns the message limiter, which Inherit may have replaced.
func (c *Commander) limiter() *ratelimit.Limiter {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.messageLimiter
}

// slot returns the danger slot, which Inherit may have replaced.
func (c *Commander) slot() *sync.Mutex {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.dangerSlot
}

// Stats returns thread-safe counters and timing info.
func (c *Commander) Stats() (count int, last string, lastTime time.Time, uptime time.Duration) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.commandCount, c.lastCommand, c.lastTime, time.Since(c.startTime)
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// parseRejection is the reply to an input the parser did not understand.
func parseRejection(err error) command.Response {
	return rejection("❌ Parse Error", fmt.Sprintf("Could not parse command: %s", err.Error()), 7)
}

// describe returns the action and target of cmd, the way the stats and the
// confirmation reply name a command: "reboot vps", or "help" alone.
func describe(cmd command.Command) string {
	if cmd.Target == "" {
		return string(cmd.Action)
	}
	return string(cmd.Action) + " " + cmd.Target
}

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

	// The operations commands name their own dangerous actions (restarting
	// a container), so they are not hard-coded here twice.
	for _, action := range command.DangerousActions() {
		dangerous[action] = true
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

// enabledActions returns the set of enabled_commands, or nil when the list
// is empty and every command is enabled.
func enabledActions(cfg *config.Config) map[command.Action]bool {
	if len(cfg.EnabledCommands) == 0 {
		return nil
	}
	return actionSet(cfg.EnabledCommands)
}

// actionSet turns a list of command names from the configuration into a
// set of actions.
func actionSet(names []string) map[command.Action]bool {
	set := make(map[command.Action]bool, len(names))
	for _, name := range names {
		set[command.Action(name)] = true
	}
	return set
}

// CommandNames returns the names of the built-in commands, the ones a
// configuration may list in categories[].commands.
func CommandNames() []string {
	registry, _ := buildRegistry(config.DefaultConfig(), nil)
	return registry.Names()
}

// buildRegistry creates a fresh Builtins+Registry pair with every command
// registered. It returns both so the caller can wire SSH after the fact.
func buildRegistry(cfg *config.Config, exec executor.Executor) (*command.Registry, *command.Builtins) {
	b := command.NewBuiltins(cfg, exec)
	r := command.NewRegistry()
	registerAll(r, b)

	// The operations commands (failed, health, docker, digest) live in
	// their own file and register themselves the same way.
	b.RegisterExtras(r)
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
