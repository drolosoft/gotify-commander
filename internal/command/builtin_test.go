package command

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

// mockExecutor is a simple mock implementing executor.Executor.
type mockExecutor struct {
	output   string
	exitCode int
	err      error
}

func (m *mockExecutor) Run(ctx context.Context, name string, args ...string) (executor.Result, error) {
	return executor.Result{Output: m.output, ExitCode: m.exitCode}, m.err
}

// testConfig returns a Config with 3 services for testing.
func testConfig() *config.Config {
	return &config.Config{
		Defaults: config.Defaults{
			Timeout:  30 * time.Second,
			LogLines: 20,
		},
		Services: map[string]config.Service{
			"nginx": {
				Description: "Web server",
				Machine:     "vps",
				Port:        80,
				Aliases:     []string{"ng", "web"},
				Systemd:     "nginx",
			},
			"laporra": {
				Description: "La Porra app",
				Machine:     "vps",
				Port:        21000,
				Aliases:     []string{"porra", "lp"},
				Systemd:     "laporra",
			},
			"tubearchivist": {
				Description: "YouTube archiver",
				Machine:     "mac",
				Port:        8080,
				Aliases:     []string{"tube", "ta"},
				Launchd:     "com.tubearchivist.web",
			},
		},
	}
}

func newTestBuiltins(mockOut string) *Builtins {
	mock := &mockExecutor{output: mockOut}
	return NewBuiltins(testConfig(), mock)
}

// 1. TestHelpCommand — contains "restart", "VPS", "Mac", "nginx", "tube"
func TestHelpCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Help(Command{Action: ActionHelp})

	if resp.Priority != 1 {
		t.Errorf("expected priority 1, got %d", resp.Priority)
	}
	for _, want := range []string{"restart", "VPS", "Mac", "nginx", "tube"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("Help message missing %q", want)
		}
	}
}

// 2. TestPingCommand — contains "pong"
func TestPingCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Ping(Command{Action: ActionPing})

	if resp.Priority != 1 {
		t.Errorf("expected priority 1, got %d", resp.Priority)
	}
	if !strings.Contains(resp.Message, "pong") {
		t.Errorf("Ping message missing 'pong', got: %q", resp.Message)
	}
}

// 3. TestServicesCommand — contains "nginx", "tubearchivist", "VPS"
func TestServicesCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Services(Command{Action: ActionServices})

	if resp.Priority != 1 {
		t.Errorf("expected priority 1, got %d", resp.Priority)
	}
	for _, want := range []string{"nginx", "tubearchivist", "VPS"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("Services message missing %q", want)
		}
	}
}

// 4. TestRestartCommand — mock output "", check "✅" and "nginx"
func TestRestartCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Restart(Command{Action: ActionRestart, Target: "nginx"})

	for _, want := range []string{"✅", "nginx"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("Restart message missing %q, got: %q", want, resp.Message)
		}
	}
}

// 5. TestRestartUnknownService — target "unknown", check error message
func TestRestartUnknownService(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Restart(Command{Action: ActionRestart, Target: "unknown"})

	if resp.Priority < 5 {
		t.Errorf("expected error priority >= 5, got %d", resp.Priority)
	}
	if !strings.Contains(strings.ToLower(resp.Message), "unknown") &&
		!strings.Contains(strings.ToLower(resp.Message), "not found") &&
		!strings.Contains(strings.ToLower(resp.Message), "service") {
		t.Errorf("Restart error message doesn't mention the problem, got: %q", resp.Message)
	}
}

// 6. TestStopCommand — check "🛑"
func TestStopCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Stop(Command{Action: ActionStop, Target: "nginx"})

	if !strings.Contains(resp.Message, "🛑") {
		t.Errorf("Stop message missing '🛑', got: %q", resp.Message)
	}
}

// 7. TestStartCommand — check "▶️"
func TestStartCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Start(Command{Action: ActionStart, Target: "nginx"})

	if !strings.Contains(resp.Message, "▶️") {
		t.Errorf("Start message missing '▶️', got: %q", resp.Message)
	}
}

// 8. TestStatusSingle — mock "active", check "nginx"
func TestStatusSingle(t *testing.T) {
	b := newTestBuiltins("active")
	resp := b.Status(Command{Action: ActionStatus, Target: "nginx"})

	if !strings.Contains(resp.Message, "nginx") {
		t.Errorf("Status message missing 'nginx', got: %q", resp.Message)
	}
}

// 9. TestStatusNoTarget — check "VPS" section
func TestStatusNoTarget(t *testing.T) {
	b := newTestBuiltins("active")
	resp := b.Status(Command{Action: ActionStatus, Target: ""})

	if !strings.Contains(resp.Message, "VPS") {
		t.Errorf("Status all message missing 'VPS', got: %q", resp.Message)
	}
}

// 10. TestFreeCommand — mock memory output, check "💾" and "Mem:"
func TestFreeCommand(t *testing.T) {
	memOut := "              total        used        free\nMem:           16Gi       8.0Gi       4.0Gi"
	b := newTestBuiltins(memOut)
	resp := b.Free(Command{Action: ActionFree})

	for _, want := range []string{"💾", "Mem:"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("Free message missing %q, got: %q", want, resp.Message)
		}
	}
}

// 11. TestDfCommand — mock disk output, check "💿"
func TestDfCommand(t *testing.T) {
	dfOut := "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1        50G   20G   30G  40% /"
	b := newTestBuiltins(dfOut)
	resp := b.Df(Command{Action: ActionDf})

	if !strings.Contains(resp.Message, "💿") {
		t.Errorf("Df message missing '💿', got: %q", resp.Message)
	}
}

// 12. TestUptimeCommand — mock uptime output, check "⏱️"
func TestUptimeCommand(t *testing.T) {
	uptimeOut := " 10:30:00 up 3 days, 14:22,  1 user,  load average: 0.10, 0.15, 0.20"
	b := newTestBuiltins(uptimeOut)
	resp := b.Uptime(Command{Action: ActionUptime})

	if !strings.Contains(resp.Message, "⏱️") {
		t.Errorf("Uptime message missing '⏱️', got: %q", resp.Message)
	}
}

// 13. TestWhoCommand — mock who output, check "👤"
func TestWhoCommand(t *testing.T) {
	whoOut := "juan     pts/0        2026-04-25 10:00 (192.168.1.1)"
	b := newTestBuiltins(whoOut)
	resp := b.Who(Command{Action: ActionWho})

	if !strings.Contains(resp.Message, "👤") {
		t.Errorf("Who message missing '👤', got: %q", resp.Message)
	}
}

// 14. TestLogsCommand — mock journalctl output, check "📜" and "nginx"
func TestLogsCommand(t *testing.T) {
	logOut := "Apr 25 10:00:01 server nginx[1234]: Starting nginx...\nApr 25 10:00:02 server nginx[1234]: started."
	b := newTestBuiltins(logOut)
	resp := b.Logs(Command{Action: ActionLogs, Target: "nginx"})

	for _, want := range []string{"📜", "nginx"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("Logs message missing %q, got: %q", want, resp.Message)
		}
	}
}

// 15. TestRebootCommand — target "vps", check "⚠️"
func TestRebootCommand(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Reboot(Command{Action: ActionReboot, Target: "vps"})

	if !strings.Contains(resp.Message, "⚠️") {
		t.Errorf("Reboot message missing '⚠️', got: %q", resp.Message)
	}
}

// 16. TestRebootNoTarget — empty target, check error priority >= 5
func TestRebootNoTarget(t *testing.T) {
	b := newTestBuiltins("")
	resp := b.Reboot(Command{Action: ActionReboot, Target: ""})

	if resp.Priority < 5 {
		t.Errorf("expected error priority >= 5, got %d", resp.Priority)
	}
}
