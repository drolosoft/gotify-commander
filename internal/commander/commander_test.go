package commander

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

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

type mockSender struct {
	messages []capturedMessage
}

func (m *mockSender) Send(title, message string, priority int, markdown bool) {
	m.messages = append(m.messages, capturedMessage{title, message, priority})
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
