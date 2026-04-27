package executor

import (
	"context"
	"testing"

	"github.com/drolosoft/gotify-commander/internal/config"
)

func TestNewSSHExecutor(t *testing.T) {
	target := config.SSHTarget{
		Host:    "192.168.1.100",
		Port:    22,
		User:    "admin",
		KeyFile: "/nonexistent/key",
	}
	exec := NewSSHExecutor(target)
	if exec == nil {
		t.Fatal("expected non-nil SSHExecutor")
	}
	if exec.target.Host != "192.168.1.100" {
		t.Errorf("wrong host: got %q, want %q", exec.target.Host, "192.168.1.100")
	}
	if exec.target.User != "admin" {
		t.Errorf("wrong user: got %q, want %q", exec.target.User, "admin")
	}
	if exec.target.KeyFile != "/nonexistent/key" {
		t.Errorf("wrong key_file: got %q, want %q", exec.target.KeyFile, "/nonexistent/key")
	}
}

func TestSSHExecutorDefaultPort(t *testing.T) {
	exec := NewSSHExecutor(config.SSHTarget{Host: "1.2.3.4", Port: 0})
	if exec.target.Port != 22 {
		t.Errorf("expected port 22, got %d", exec.target.Port)
	}
}

func TestSSHExecutorExplicitPort(t *testing.T) {
	exec := NewSSHExecutor(config.SSHTarget{Host: "1.2.3.4", Port: 2222})
	if exec.target.Port != 2222 {
		t.Errorf("expected port 2222, got %d", exec.target.Port)
	}
}

func TestSSHExecutorInitialClientNil(t *testing.T) {
	exec := NewSSHExecutor(config.SSHTarget{Host: "1.2.3.4", Port: 22})
	// Before any connection is made the client should be nil (lazy connect).
	exec.mu.Lock()
	client := exec.client
	exec.mu.Unlock()
	if client != nil {
		t.Error("expected nil client before first connection")
	}
}

func TestSSHExecutorCloseBeforeConnect(t *testing.T) {
	exec := NewSSHExecutor(config.SSHTarget{Host: "1.2.3.4", Port: 22})
	// Close should be safe to call even before any connection was made.
	exec.Close()
}

func TestSSHExecutorImplementsInterface(t *testing.T) {
	// Compile-time guard: SSHExecutor must satisfy the Executor interface.
	var _ Executor = (*SSHExecutor)(nil)
}

// TestSSHExecutorRunNoServer tests that Run returns a meaningful error when
// the target host is unreachable (connection refused / timeout).
// This is a unit test — no live SSH server is needed.
func TestSSHExecutorRunNoServer(t *testing.T) {
	// Use localhost on a port that should have nothing listening.
	exec := NewSSHExecutor(config.SSHTarget{
		Host:    "127.0.0.1",
		Port:    1,
		User:    "nobody",
		KeyFile: "/nonexistent/key",
	})

	// Run will fail at the key-read stage before even trying to connect.
	// We just verify it returns an error and not a panic.
	result, err := exec.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatalf("expected error for unreachable host, got result: %+v", result)
	}
}
