package commander

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
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
