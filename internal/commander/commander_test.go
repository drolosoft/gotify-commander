package commander

import (
	"context"
	"log"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/ratelimit"
	"github.com/drolosoft/gotify-commander/internal/stream"
)

// The listener hands commands to the Commander directly.
var _ stream.Handler = (*Commander)(nil)

// ─── mocks ───────────────────────────────────────────────────────────────────

type mockExecutor struct{ output string }

func (m *mockExecutor) Run(_ context.Context, _ string, _ ...string) (executor.Result, error) {
	return executor.Result{Output: m.output}, nil
}

type capturedMessage struct {
	title    string
	message  string
	priority int
}

// mockSender records every message. Execute sends from its own goroutine,
// so the slice is guarded and the tests read it through snapshot.
type mockSender struct {
	mu       sync.Mutex
	messages []capturedMessage
}

func (m *mockSender) Send(title, message string, priority int, markdown bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.messages = append(m.messages, capturedMessage{title, message, priority})
}

// snapshot returns a copy of the messages recorded so far.
func (m *mockSender) snapshot() []capturedMessage {
	m.mu.Lock()
	defer m.mu.Unlock()

	return append([]capturedMessage(nil), m.messages...)
}

// waitForMessages polls until at least count messages arrived or a second
// passed, for the messages Execute sends in the background.
func (m *mockSender) waitForMessages(count int) []capturedMessage {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		messages := m.snapshot()
		if len(messages) >= count {
			return messages
		}
		time.Sleep(5 * time.Millisecond)
	}

	return m.snapshot()
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func testConfig() *config.Config {
	return &config.Config{
		Gotify: config.GotifyConfig{
			ServerURL:        "http://localhost:80",
			ClientToken:      "test-token",
			CommandAppID:     1,
			ResponseAppToken: "resp-token",
		},
		Defaults: config.Defaults{
			Timeout:  5 * time.Second,
			LogLines: 30,
		},
		SSHTargets: map[string]config.SSHTarget{},
		Services: map[string]config.Service{
			"nginx": {
				Machine: "vps",
				Port:    80,
				Aliases: []string{"ng"},
				Systemd: "nginx",
			},
		},
	}
}

func newTestCommander(output string) (*Commander, *mockSender) {
	cfg := testConfig()
	exec := &mockExecutor{output: output}
	sender := &mockSender{}
	cmd := New(cfg, exec, sender)
	return cmd, sender
}

// ─── tests ───────────────────────────────────────────────────────────────────

func TestHandleCommandHelp(t *testing.T) {
	cmd, sender := newTestCommander("")
	cmd.HandleCommand("help")

	if len(sender.messages) == 0 {
		t.Fatal("expected at least one message, got none")
	}
	msg := sender.messages[0]
	if !strings.Contains(msg.message, "Commander") && !strings.Contains(msg.title, "Help") {
		t.Errorf("expected help response to mention 'Commander' or title 'Help', got title=%q message=%q", msg.title, msg.message)
	}
}

func TestHandleCommandPing(t *testing.T) {
	cmd, sender := newTestCommander("")
	cmd.HandleCommand("ping")

	if len(sender.messages) == 0 {
		t.Fatal("expected at least one message, got none")
	}
	msg := sender.messages[0]
	if !strings.Contains(strings.ToLower(msg.message), "pong") {
		t.Errorf("expected pong in message, got %q", msg.message)
	}
}

func TestHandleCommandRestart(t *testing.T) {
	cmd, sender := newTestCommander("")
	cmd.HandleCommand("restart nginx")

	if len(sender.messages) == 0 {
		t.Fatal("expected at least one message, got none")
	}
	msg := sender.messages[0]
	combined := msg.title + " " + msg.message
	if !strings.Contains(strings.ToLower(combined), "nginx") {
		t.Errorf("expected 'nginx' in response, got title=%q message=%q", msg.title, msg.message)
	}
}

func TestHandleCommandUnknown(t *testing.T) {
	cmd, sender := newTestCommander("")
	cmd.HandleCommand("foobar")

	if len(sender.messages) == 0 {
		t.Fatal("expected at least one message, got none")
	}
	msg := sender.messages[0]
	if msg.priority < 5 {
		t.Errorf("expected priority >= 5 for unknown command, got %d", msg.priority)
	}
}

func TestHandleCommandSanitization(t *testing.T) {
	cmd, sender := newTestCommander("")
	// "nginx;rm -rf /" — the semicolon makes the target fail ValidateInput
	cmd.HandleCommand("restart nginx;rm -rf /")

	if len(sender.messages) == 0 {
		t.Fatal("expected at least one message, got none")
	}
	msg := sender.messages[0]
	// Must be rejected with elevated priority (parse error or security rejection)
	if msg.priority < 5 {
		t.Errorf("expected rejection with priority >= 5, got priority=%d title=%q", msg.priority, msg.title)
	}
}

func TestStats(t *testing.T) {
	cmd, _ := newTestCommander("")
	count, last, lastTime, uptime := cmd.Stats()
	if count != 0 {
		t.Errorf("expected commandCount=0, got %d", count)
	}
	if last != "" {
		t.Errorf("expected empty lastCommand, got %q", last)
	}
	if !lastTime.IsZero() {
		t.Errorf("expected zero lastTime, got %v", lastTime)
	}
	if uptime < 0 {
		t.Errorf("expected non-negative uptime, got %v", uptime)
	}

	cmd.HandleCommand("ping")
	count, last, _, _ = cmd.Stats()
	if count != 1 {
		t.Errorf("expected commandCount=1, got %d", count)
	}
	if last == "" {
		t.Error("expected lastCommand to be set after ping")
	}
}

func TestSnapshotCarriesStatsToANewCommander(t *testing.T) {
	old, _ := newTestCommander("")
	old.HandleCommand("ping")
	snapshot := old.Snapshot()

	if snapshot.CommandCount != 1 || snapshot.LastCommand == "" || snapshot.LastTime.IsZero() {
		t.Fatalf("snapshot after ping = %+v, want one command with its name and time", snapshot)
	}

	// Both commanders start within microseconds of each other; an Enable an
	// hour ago is what tells a carried uptime from a fresh one.
	snapshot.StartTime = snapshot.StartTime.Add(-time.Hour)

	// The plugin builds a new commander on every saved configuration and
	// hands it the old one's stats, so the counters and the uptime go on.
	replacement, _ := newTestCommander("")
	replacement.SetStats(snapshot)

	count, last, lastTime, uptime := replacement.Stats()
	if count != 1 {
		t.Errorf("count after SetStats = %d, want 1", count)
	}
	if last != snapshot.LastCommand || !lastTime.Equal(snapshot.LastTime) {
		t.Errorf("last command after SetStats = %q at %v, want %q at %v", last, lastTime, snapshot.LastCommand, snapshot.LastTime)
	}
	if uptime < time.Hour {
		t.Errorf("uptime after SetStats = %v, want it counted from %v", uptime, snapshot.StartTime)
	}

	replacement.HandleCommand("ping")
	if count, _, _, _ := replacement.Stats(); count != 2 {
		t.Errorf("count after one more ping = %d, want 2", count)
	}
}

// panicOn replaces the handler of action with one that panics.
func panicOn(cmd *Commander, action command.Action) {
	cmd.registry.Register(action, func(command.Command) command.Response {
		panic("builtin exploded")
	})
}

func TestPanicInHandleCommandIsRecovered(t *testing.T) {
	cmd, sender := newTestCommander("")
	panicOn(cmd, command.ActionPing)

	cmd.HandleCommand("ping")

	messages := sender.snapshot()
	if len(messages) != 1 {
		t.Fatalf("expected one internal error message, got %d: %+v", len(messages), messages)
	}
	if !strings.Contains(messages[0].message, "internal error") {
		t.Errorf("expected an internal error reply, got title=%q message=%q", messages[0].title, messages[0].message)
	}
	if messages[0].priority < 7 {
		t.Errorf("expected priority >= 7 for an internal error, got %d", messages[0].priority)
	}

	// The commander keeps working after the panic.
	cmd.HandleCommand("help")
	if len(sender.snapshot()) != 2 {
		t.Error("expected help to answer after the recovered panic")
	}
}

func TestPanicInExecuteIsRecovered(t *testing.T) {
	cmd, _ := newTestCommander("")
	panicOn(cmd, command.ActionPing)

	resp := cmd.Execute("ping")

	if !strings.Contains(resp.Message, "internal error") {
		t.Errorf("expected an internal error response, got title=%q message=%q", resp.Title, resp.Message)
	}
	if resp.Priority < 7 {
		t.Errorf("expected priority >= 7 for an internal error, got %d", resp.Priority)
	}
}

func TestHandleSkipsCancelledContext(t *testing.T) {
	cmd, sender := newTestCommander("")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cmd.Handle(ctx, "ping")

	if messages := sender.snapshot(); len(messages) != 0 {
		t.Errorf("expected no message for a cancelled command, got %+v", messages)
	}
	if count, _, _, _ := cmd.Stats(); count != 0 {
		t.Errorf("expected the cancelled command not to count, got %d", count)
	}
}

func TestHandleRunsLikeHandleCommand(t *testing.T) {
	cmd, sender := newTestCommander("")

	cmd.Handle(context.Background(), "ping")

	messages := sender.snapshot()
	if len(messages) != 1 || !strings.Contains(strings.ToLower(messages[0].message), "pong") {
		t.Errorf("expected one pong, got %+v", messages)
	}
}

func TestBusyTellsTheUserToRetry(t *testing.T) {
	cmd, sender := newTestCommander("")

	cmd.Busy("traffic")

	messages := sender.snapshot()
	if len(messages) != 1 {
		t.Fatalf("expected one busy message, got %d", len(messages))
	}
	if !strings.Contains(strings.ToLower(messages[0].message), "try") || !strings.Contains(messages[0].message, "traffic") {
		t.Errorf("expected a try-later message naming the command, got %q", messages[0].message)
	}
}

func TestExecuteReturnsAndSendsResponse(t *testing.T) {
	cmd, sender := newTestCommander("")

	resp := cmd.Execute("ping")

	if !strings.Contains(strings.ToLower(resp.Message), "pong") {
		t.Errorf("expected pong in the returned response, got %q", resp.Message)
	}
	messages := sender.waitForMessages(1)
	if len(messages) != 1 || messages[0].message != resp.Message {
		t.Errorf("expected the same response sent once, got %+v", messages)
	}
	if count, _, _, _ := cmd.Stats(); count != 1 {
		t.Errorf("expected commandCount=1, got %d", count)
	}
}

func TestExecuteRejectionsAreNotSent(t *testing.T) {
	inputs := []string{"foobar", "restart nginx;rm -rf /", ""}

	for _, input := range inputs {
		cmd, sender := newTestCommander("")

		resp := cmd.Execute(input)

		if resp.Priority < 7 {
			t.Errorf("Execute(%q): expected a rejection with priority >= 7, got %+v", input, resp)
		}
		time.Sleep(20 * time.Millisecond)
		if messages := sender.snapshot(); len(messages) != 0 {
			t.Errorf("Execute(%q): rejections are returned, not sent; got %+v", input, messages)
		}
		if count, _, _, _ := cmd.Stats(); count != 0 {
			t.Errorf("Execute(%q): a rejection must not count, got %d", input, count)
		}
	}
}

func TestHandleCommandForbiddenService(t *testing.T) {
	cmd, sender := newTestCommander("")

	cmd.HandleCommand("restart postgres")

	messages := sender.snapshot()
	if len(messages) != 1 || messages[0].priority < 7 {
		t.Errorf("expected one rejection for a service outside the whitelist, got %+v", messages)
	}
}

func TestCommandNamesMatchRegistry(t *testing.T) {
	names := CommandNames()

	// Every name must reach a handler, and the ones the default panel tabs
	// use must be there.
	cmd, _ := newTestCommander("")
	for _, name := range names {
		if _, ok := cmd.registry.Lookup(command.Action(name)); !ok {
			t.Errorf("CommandNames lists %q but the registry has no handler for it", name)
		}
	}
	for _, want := range []string{"help", "locate", "reboot", "traffic", "analytics"} {
		if !slices.Contains(names, want) {
			t.Errorf("CommandNames() = %v, missing %q", names, want)
		}
	}
	if len(names) != 23 {
		t.Errorf("CommandNames() has %d names, want the 23 builtins", len(names))
	}
}

func TestSSHTargetKnownHostsFileReachesExecutor(t *testing.T) {
	cfg := testConfig()
	cfg.SSHTargets["mac"] = config.SSHTarget{
		Host:           "100.64.0.2",
		User:           "admin",
		KeyFile:        "/home/admin/.ssh/id_ed25519",
		KnownHostsFile: "/home/admin/.ssh/known_hosts",
	}

	cmd := New(cfg, &mockExecutor{}, &mockSender{})

	sshExec, ok := cmd.sshExec.(*executor.SSHExecutor)
	if !ok {
		t.Fatalf("sshExec is %T, want *executor.SSHExecutor", cmd.sshExec)
	}
	if sshExec.KnownHostsFile != "/home/admin/.ssh/known_hosts" {
		t.Errorf("KnownHostsFile = %q, want the configured file", sshExec.KnownHostsFile)
	}
}

func TestLogsDoNotCarryCommandArguments(t *testing.T) {
	logs := &strings.Builder{}
	var logsMu sync.Mutex
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}))
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// The longitude is out of range, so Locate answers without calling
	// Nominatim; the latitude is what must not reach the log.
	cmd, _ := newTestCommander("")
	cmd.HandleCommand("locate 40.4168 -300.5")

	// A command dropped because the plugin is stopping is logged too.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cmd.Handle(cancelled, "where 28.4682 -16.2546")

	logsMu.Lock()
	written := logs.String()
	logsMu.Unlock()

	for _, coordinate := range []string{"40.4168", "300.5", "28.4682", "16.2546"} {
		if strings.Contains(written, coordinate) {
			t.Errorf("log carries the coordinate %s:\n%s", coordinate, written)
		}
	}
	if !strings.Contains(written, "action=locate") || !strings.Contains(written, `dropped "where"`) {
		t.Errorf("log does not name the commands:\n%s", written)
	}
}

// writerFunc turns a function into an io.Writer for log.SetOutput.
type writerFunc func(p []byte) (int, error)

// Write calls the function.
func (write writerFunc) Write(p []byte) (int, error) {
	return write(p)
}

// ─── rate limit and dangerous commands ───────────────────────────────────────

// gatedExecutor records every program it is asked to run. A run whose first
// argument is blocked (the "reboot" of "sudo reboot") reports on started and
// waits for release, so a test can keep that command in flight.
type gatedExecutor struct {
	blocked string
	started chan struct{}
	release chan struct{}

	mu   sync.Mutex
	runs []string
}

// newGatedExecutor returns an executor that holds every run of blocked
// until release is closed.
func newGatedExecutor(blocked string) *gatedExecutor {
	return &gatedExecutor{
		blocked: blocked,
		started: make(chan struct{}, 1),
		release: make(chan struct{}),
	}
}

// Run records the program and, for the blocked one, waits for release or
// for the command's timeout, so a regression fails instead of hanging.
func (e *gatedExecutor) Run(ctx context.Context, name string, args ...string) (executor.Result, error) {
	e.mu.Lock()
	e.runs = append(e.runs, strings.Join(append([]string{name}, args...), " "))
	e.mu.Unlock()

	if e.blocked != "" && len(args) > 0 && args[0] == e.blocked {
		// Only the first blocked run needs to be announced; the buffer
		// keeps a later one from waiting for a reader that is gone.
		select {
		case e.started <- struct{}{}:
		default:
		}

		select {
		case <-e.release:
		case <-ctx.Done():
		}
	}

	return executor.Result{Output: "ok"}, nil
}

// count returns how many recorded runs start with program.
func (e *gatedExecutor) count(program string) int {
	e.mu.Lock()
	defer e.mu.Unlock()

	total := 0
	for _, run := range e.runs {
		if strings.HasPrefix(run, program) {
			total++
		}
	}
	return total
}

// TestHandleRateLimitsMessages: with the default of 20 a minute, the 21st
// message inside one minute is answered with the wait and not run.
func TestHandleRateLimitsMessages(t *testing.T) {
	exec := newGatedExecutor("")
	sender := &mockSender{}
	cmdr := New(testConfig(), exec, sender)

	// The clock moves 2 s per command, so all twenty-one fall inside one
	// minute and the twenty-first arrives 40 s after the first.
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	cmdr.messageLimiter = ratelimit.NewWithClock(config.DefaultRateLimitPerMinute, time.Minute, func() time.Time { return now })

	for range 20 {
		cmdr.Handle(context.Background(), "uptime")
		now = now.Add(2 * time.Second)
	}
	cmdr.Handle(context.Background(), "uptime")

	if runs := exec.count("uptime"); runs != 20 {
		t.Errorf("executor ran uptime %d times, want 20", runs)
	}

	messages := sender.snapshot()
	if len(messages) != 21 {
		t.Fatalf("got %d replies, want 21", len(messages))
	}

	refused := messages[len(messages)-1]
	if refused.title != "⏳ Rate limited" {
		t.Errorf("title %q, want %q", refused.title, "⏳ Rate limited")
	}
	if refused.message != "⏳ Too many commands, try again in 20 s" {
		t.Errorf("message %q, want the wait until the first command leaves the minute", refused.message)
	}
	if refused.priority != 5 {
		t.Errorf("priority %d, want 5", refused.priority)
	}
	if count, _, _, _ := cmdr.Stats(); count != 20 {
		t.Errorf("stats count %d, want 20: a refused command does not count", count)
	}

	// A minute after the first command, its slot is free again.
	now = now.Add(20 * time.Second)
	cmdr.Handle(context.Background(), "uptime")
	if runs := exec.count("uptime"); runs != 21 {
		t.Errorf("after the window moved: executor ran uptime %d times, want 21", runs)
	}
}

func TestHandleUsesTheConfiguredRateLimit(t *testing.T) {
	cfg := testConfig()
	cfg.Defaults.RateLimitPerMinute = 2
	exec := newGatedExecutor("")
	cmdr := New(cfg, exec, &mockSender{})

	for range 3 {
		cmdr.Handle(context.Background(), "uptime")
	}

	if runs := exec.count("uptime"); runs != 2 {
		t.Errorf("executor ran uptime %d times, want 2 with rate_limit_per_minute: 2", runs)
	}
}

func TestExecuteIsNotCountedByTheMessageLimit(t *testing.T) {
	exec := newGatedExecutor("")
	cmdr := New(testConfig(), exec, &mockSender{})

	// The panel has its own limiter in front of Execute; the messages'
	// budget must not take commands away from it, or the other way round.
	for range config.DefaultRateLimitPerMinute + 1 {
		cmdr.Execute("uptime")
	}
	cmdr.Handle(context.Background(), "uptime")

	if runs := exec.count("uptime"); runs != config.DefaultRateLimitPerMinute+2 {
		t.Errorf("executor ran uptime %d times, want %d", runs, config.DefaultRateLimitPerMinute+2)
	}
}

func TestDangerousCommandsRunOneAtATime(t *testing.T) {
	cfg := testConfig()
	cfg.Categories = []config.Category{
		{Label: "Careful", Type: "service", Commands: []string{"restart"}, Danger: true},
	}
	// The slot is what this test is about, so the messages run at once
	// instead of asking for a code first.
	cfg.RequireConfirm = []string{}
	exec := newGatedExecutor("reboot")
	sender := &mockSender{}
	cmdr := New(cfg, exec, sender)

	// A reboot that is still waiting for the host holds the danger slot.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		cmdr.Handle(context.Background(), "reboot vps")
	}()
	<-exec.started

	cmdr.Handle(context.Background(), "reboot vps")
	cmdr.Handle(context.Background(), "shutdown vps")
	cmdr.Handle(context.Background(), "restart nginx")
	panelReply := cmdr.Execute("reboot vps")
	cmdr.Handle(context.Background(), "ping")

	messages := sender.snapshot()
	if len(messages) != 4 {
		t.Fatalf("got %d replies while the reboot runs, want 4: %+v", len(messages), messages)
	}
	for index, refused := range messages[:3] {
		if refused.message != "⚠️ Another dangerous command is still running" {
			t.Errorf("reply %d: message %q, want the danger refusal", index+1, refused.message)
		}
	}
	if panelReply.Message != "⚠️ Another dangerous command is still running" {
		t.Errorf("panel reply %q, want the danger refusal", panelReply.Message)
	}
	if messages[3].message != "pong" {
		t.Errorf("ping during the reboot answered %q, want pong", messages[3].message)
	}
	if runs := exec.count("sudo"); runs != 1 {
		t.Errorf("executor ran %d privileged commands, want only the first reboot", runs)
	}

	// Once the reboot returns, the next dangerous command may run.
	close(exec.release)
	<-finished

	cmdr.Handle(context.Background(), "shutdown vps")
	if runs := exec.count("sudo shutdown"); runs != 1 {
		t.Errorf("shutdown after the reboot finished ran %d times, want 1", runs)
	}
}

func TestPanicInDangerousCommandFreesTheSlot(t *testing.T) {
	cfg := testConfig()
	cfg.RequireConfirm = []string{}
	sender := &mockSender{}
	cmdr := New(cfg, &mockExecutor{}, sender)
	panicOn(cmdr, command.ActionReboot)

	cmdr.HandleCommand("reboot vps")
	cmdr.HandleCommand("shutdown vps")

	messages := sender.snapshot()
	if len(messages) != 2 {
		t.Fatalf("got %d replies, want 2: %+v", len(messages), messages)
	}
	if strings.Contains(messages[1].message, "Another dangerous command") {
		t.Errorf("shutdown after a panicked reboot was refused: %q", messages[1].message)
	}
}

// ─── confirmation, enabled_commands and handover ─────────────────────────────

// fakeClock is the clock of the message limiter and the confirmations in
// these tests, moved by hand instead of sleeping through a minute.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

// newFakeClock returns a clock stopped at a fixed time.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}
}

// read returns the time of the clock.
func (clock *fakeClock) read() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	return clock.now
}

// advance moves the clock forward by step.
func (clock *fakeClock) advance(step time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()

	clock.now = clock.now.Add(step)
}

// newClockedCommander builds a commander for cfg whose message limiter and
// confirmations read clock. Runs of blocked wait as in newGatedExecutor.
func newClockedCommander(cfg *config.Config, blocked string, clock *fakeClock) (*Commander, *gatedExecutor, *mockSender) {
	exec := newGatedExecutor(blocked)
	sender := &mockSender{}
	cmdr := New(cfg, exec, sender)

	cmdr.messageLimiter = ratelimit.NewWithClock(cfg.Defaults.CommandsPerMinute(), time.Minute, clock.read)
	cmdr.confirmations = command.NewConfirmations(clock.read)

	return cmdr, exec, sender
}

// confirmationPattern matches the reply that asks for a code, and captures
// the command and the code.
var confirmationPattern = regexp.MustCompile("^⚠️ Confirm `([^`]+)`: reply `confirm ([0-9]{4})` within 60 s")

// confirmationCode returns the code in reply, which must be a confirmation
// request for want.
func confirmationCode(t *testing.T, reply capturedMessage, want string) string {
	t.Helper()

	if reply.title != "⚠️ Confirmation needed" || reply.priority != 7 {
		t.Fatalf("reply %+v, want a confirmation request with priority 7", reply)
	}

	match := confirmationPattern.FindStringSubmatch(reply.message)
	if match == nil {
		t.Fatalf("reply %q does not ask for a code", reply.message)
	}
	if match[1] != want {
		t.Fatalf("reply asks to confirm %q, want %q", match[1], want)
	}

	return match[2]
}

// lastReply returns the last message sender recorded.
func lastReply(t *testing.T, sender *mockSender) capturedMessage {
	t.Helper()

	messages := sender.snapshot()
	if len(messages) == 0 {
		t.Fatal("no reply was sent")
	}
	return messages[len(messages)-1]
}

func TestDangerousMessagesAskForConfirmation(t *testing.T) {
	for _, input := range []string{"reboot vps", "shutdown vps", "stop nginx", "poweroff mac", "stop ng"} {
		cmdr, exec, sender := newClockedCommander(testConfig(), "", newFakeClock())

		cmdr.Handle(context.Background(), input)

		messages := sender.snapshot()
		if len(messages) != 1 {
			t.Fatalf("%s: got %d replies, want the confirmation request: %+v", input, len(messages), messages)
		}

		// The reply names the parsed command, so an alias shows what it
		// resolved to.
		parsed, err := command.Parse(input, cmdr.aliases, cmdr.services)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		confirmationCode(t, messages[0], string(parsed.Action)+" "+parsed.Target)

		if total := exec.count("sudo") + exec.count("systemctl"); total != 0 {
			t.Errorf("%s: %d commands ran before the confirmation", input, total)
		}
		if count, _, _, _ := cmdr.Stats(); count != 0 {
			t.Errorf("%s: stats count %d, want 0: nothing ran yet", input, count)
		}
	}
}

func TestConfirmRunsThePendingCommandOnce(t *testing.T) {
	clock := newFakeClock()
	cmdr, exec, sender := newClockedCommander(testConfig(), "", clock)

	cmdr.Handle(context.Background(), "reboot vps")
	code := confirmationCode(t, lastReply(t, sender), "reboot vps")

	clock.advance(30 * time.Second)
	cmdr.Handle(context.Background(), "confirm "+code)

	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Fatalf("reboot ran %d times after the confirmation, want 1", runs)
	}
	if reply := lastReply(t, sender); !strings.Contains(reply.title, "Reboot") {
		t.Errorf("reply to the confirmation %+v, want the reboot's own answer", reply)
	}
	if count, last, _, _ := cmdr.Stats(); count != 1 || last != "reboot vps" {
		t.Errorf("stats %d, %q; want one reboot vps", count, last)
	}

	// The code is single use.
	cmdr.Handle(context.Background(), "confirm "+code)

	if reply := lastReply(t, sender); reply.message != "❌ No pending command for that code" {
		t.Errorf("second confirm answered %q, want the no-pending reply", reply.message)
	}
	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Errorf("reboot ran %d times after a reused code, want 1", runs)
	}
}

func TestConfirmWithAWrongCode(t *testing.T) {
	cmdr, exec, sender := newClockedCommander(testConfig(), "", newFakeClock())

	cmdr.Handle(context.Background(), "reboot vps")
	code := confirmationCode(t, lastReply(t, sender), "reboot vps")

	wrong := "1000"
	if code == wrong {
		wrong = "1001"
	}

	for _, input := range []string{"confirm " + wrong, "confirm"} {
		cmdr.Handle(context.Background(), input)

		if reply := lastReply(t, sender); reply.message != "❌ No pending command for that code" {
			t.Errorf("%q answered %q, want the no-pending reply", input, reply.message)
		}
	}
	if runs := exec.count("sudo"); runs != 0 {
		t.Errorf("a wrong code ran %d commands", runs)
	}
}

func TestConfirmAfterTheCodeExpired(t *testing.T) {
	clock := newFakeClock()
	cmdr, exec, sender := newClockedCommander(testConfig(), "", clock)

	cmdr.Handle(context.Background(), "reboot vps")
	code := confirmationCode(t, lastReply(t, sender), "reboot vps")

	clock.advance(61 * time.Second)
	cmdr.Handle(context.Background(), "confirm "+code)

	if reply := lastReply(t, sender); reply.message != "❌ No pending command for that code" {
		t.Errorf("expired code answered %q, want the no-pending reply", reply.message)
	}
	if runs := exec.count("sudo"); runs != 0 {
		t.Errorf("an expired code ran %d commands", runs)
	}
}

func TestNewDangerousCommandReplacesThePendingOne(t *testing.T) {
	cmdr, exec, sender := newClockedCommander(testConfig(), "", newFakeClock())

	cmdr.Handle(context.Background(), "reboot vps")
	firstCode := confirmationCode(t, lastReply(t, sender), "reboot vps")

	cmdr.Handle(context.Background(), "shutdown vps")
	replacing := lastReply(t, sender)
	secondCode := confirmationCode(t, replacing, "shutdown vps")
	if !strings.Contains(replacing.message, "replaces `reboot vps`") {
		t.Errorf("reply %q does not say it replaces the reboot", replacing.message)
	}

	cmdr.Handle(context.Background(), "confirm "+firstCode)
	if reply := lastReply(t, sender); reply.message != "❌ No pending command for that code" {
		t.Errorf("replaced code answered %q, want the no-pending reply", reply.message)
	}

	cmdr.Handle(context.Background(), "confirm "+secondCode)
	if exec.count("sudo reboot") != 0 || exec.count("sudo shutdown") != 1 {
		t.Errorf("runs %v, want only the shutdown", exec.runs)
	}
}

func TestRebootWithoutTargetIsNotConfirmed(t *testing.T) {
	cmdr, exec, sender := newClockedCommander(testConfig(), "", newFakeClock())

	// There is nothing to confirm: the reboot answers with its usage.
	cmdr.Handle(context.Background(), "reboot")

	if reply := lastReply(t, sender); !strings.Contains(reply.message, "Target required") {
		t.Errorf("reboot without a target answered %+v, want the usage", reply)
	}
	if runs := exec.count("sudo"); runs != 0 {
		t.Errorf("reboot without a target ran %d commands", runs)
	}
}

func TestEmptyRequireConfirmRunsDirectly(t *testing.T) {
	cfg := testConfig()
	cfg.RequireConfirm = []string{}
	cmdr, exec, sender := newClockedCommander(cfg, "", newFakeClock())

	cmdr.Handle(context.Background(), "reboot vps")

	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Errorf("reboot with require_confirm: [] ran %d times, want 1", runs)
	}
	if reply := lastReply(t, sender); reply.title == "⚠️ Confirmation needed" {
		t.Errorf("require_confirm: [] still asked for a code: %q", reply.message)
	}
}

func TestRequireConfirmReplacesTheDefault(t *testing.T) {
	cfg := testConfig()
	cfg.RequireConfirm = []string{"logs"}
	cmdr, exec, sender := newClockedCommander(cfg, "", newFakeClock())

	cmdr.Handle(context.Background(), "logs ng")
	confirmationCode(t, lastReply(t, sender), "logs nginx")

	cmdr.Handle(context.Background(), "reboot vps")
	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Errorf("reboot outside require_confirm ran %d times, want 1", runs)
	}
}

func TestExecuteDoesNotAskForConfirmation(t *testing.T) {
	cmdr, exec, _ := newClockedCommander(testConfig(), "", newFakeClock())

	// The panel asks in the browser before it posts to /execute.
	resp := cmdr.Execute("reboot vps")

	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Errorf("Execute ran reboot %d times, want 1 (response %+v)", runs, resp)
	}

	// A confirm sent to the panel has no pending command to take.
	resp = cmdr.Execute("confirm 1234")
	if resp.Priority < 5 || resp.Title == "⚠️ Confirmation needed" {
		t.Errorf("Execute(confirm) = %+v, want a rejection", resp)
	}
}

func TestConfirmedCommandStillWaitsForTheDangerSlot(t *testing.T) {
	cmdr, exec, sender := newClockedCommander(testConfig(), "reboot", newFakeClock())

	cmdr.Handle(context.Background(), "shutdown vps")
	code := confirmationCode(t, lastReply(t, sender), "shutdown vps")

	// A reboot from the panel holds the slot while the code is typed.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		cmdr.Execute("reboot vps")
	}()
	<-exec.started

	cmdr.Handle(context.Background(), "confirm "+code)

	if reply := lastReply(t, sender); reply.message != "⚠️ Another dangerous command is still running" {
		t.Errorf("confirm during a reboot answered %q, want the danger refusal", reply.message)
	}
	if runs := exec.count("sudo shutdown"); runs != 0 {
		t.Errorf("shutdown ran %d times while the reboot held the slot", runs)
	}

	close(exec.release)
	<-finished
}

func TestEnabledCommandsDisableTheOthers(t *testing.T) {
	cfg := testConfig()
	cfg.EnabledCommands = []string{"ping", "uptime"}
	cmdr, exec, sender := newClockedCommander(cfg, "", newFakeClock())

	cmdr.Handle(context.Background(), "reboot vps")

	// A disabled command is refused before any confirmation is asked.
	reply := lastReply(t, sender)
	if reply.message != "🚫 Command disabled by configuration" {
		t.Errorf("disabled reboot answered %+v, want the disabled reply", reply)
	}

	// The panel is refused the same way.
	if resp := cmdr.Execute("reboot vps"); resp.Message != "🚫 Command disabled by configuration" {
		t.Errorf("Execute(reboot vps) = %+v, want the disabled reply", resp)
	}

	if runs := exec.count("sudo"); runs != 0 {
		t.Errorf("a disabled command ran %d times", runs)
	}
	if count, _, _, _ := cmdr.Stats(); count != 0 {
		t.Errorf("stats count %d, want 0: a disabled command is not counted", count)
	}

	cmdr.Handle(context.Background(), "ping")
	if reply := lastReply(t, sender); reply.message != "pong" {
		t.Errorf("enabled ping answered %q, want pong", reply.message)
	}
}

func TestEmptyEnabledCommandsEnablesEverything(t *testing.T) {
	cfg := testConfig()
	cfg.EnabledCommands = []string{}
	cfg.RequireConfirm = []string{}
	cmdr, exec, _ := newClockedCommander(cfg, "", newFakeClock())

	cmdr.Handle(context.Background(), "reboot vps")

	if runs := exec.count("sudo reboot"); runs != 1 {
		t.Errorf("reboot with enabled_commands: [] ran %d times, want 1", runs)
	}
}

func TestInheritCarriesTheLimiterTheSlotAndTheStats(t *testing.T) {
	clock := newFakeClock()
	cfg := testConfig()
	cfg.RequireConfirm = []string{}
	old, oldExec, _ := newClockedCommander(cfg, "reboot", clock)

	for range 20 {
		old.Handle(context.Background(), "uptime")
	}

	// A reboot still waiting for the host holds the old commander's slot.
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		old.Execute("reboot vps")
	}()
	<-oldExec.started

	replacement, exec, sender := newClockedCommander(cfg, "", clock)
	replacement.Inherit(old)

	// The minute's budget goes on: a saved configuration is not a way
	// around the limit.
	replacement.Handle(context.Background(), "uptime")
	if reply := lastReply(t, sender); reply.title != "⏳ Rate limited" {
		t.Errorf("first message after the handover answered %+v, want rate limited", reply)
	}

	// The reboot started on the old commander still holds the slot.
	if resp := replacement.Execute("shutdown vps"); resp.Message != "⚠️ Another dangerous command is still running" {
		t.Errorf("shutdown during the old reboot answered %q, want the danger refusal", resp.Message)
	}
	if runs := exec.count("sudo"); runs != 0 {
		t.Errorf("replacement ran %d privileged commands during the old reboot", runs)
	}

	if count, _, _, _ := replacement.Stats(); count != 20 {
		t.Errorf("stats count after Inherit %d, want the old 20", count)
	}

	close(oldExec.release)
	<-finished

	// Once the old reboot returns, the shared slot is free for the new one.
	replacement.Execute("shutdown vps")
	if runs := exec.count("sudo shutdown"); runs != 1 {
		t.Errorf("shutdown after the old reboot finished ran %d times, want 1", runs)
	}
}

func TestInheritKeepsAFreshLimiterWhenTheLimitChanged(t *testing.T) {
	clock := newFakeClock()
	old, _, _ := newClockedCommander(testConfig(), "", clock)

	for range 20 {
		old.Handle(context.Background(), "uptime")
	}

	// The administrator raised the limit; the new value must apply now.
	cfg := testConfig()
	cfg.Defaults.RateLimitPerMinute = 30
	replacement, exec, _ := newClockedCommander(cfg, "", clock)
	replacement.Inherit(old)

	replacement.Handle(context.Background(), "uptime")
	if runs := exec.count("uptime"); runs != 1 {
		t.Errorf("uptime after raising the limit ran %d times, want 1", runs)
	}
}
