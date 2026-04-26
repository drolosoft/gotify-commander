package executor

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"golang.org/x/crypto/ssh"
)

// SSHExecutor runs commands on a remote machine via SSH.
type SSHExecutor struct {
	target config.SSHTarget
	client *ssh.Client
	mu     sync.Mutex
}

// NewSSHExecutor creates an SSHExecutor for the given target.
// If target.Port is 0 it defaults to 22.
func NewSSHExecutor(target config.SSHTarget) *SSHExecutor {
	if target.Port == 0 {
		target.Port = 22
	}
	return &SSHExecutor{target: target}
}

// Run executes a command on the remote host and returns the result.
//
// Behavior:
//   - Lazy-connects on first call (and reconnects on stale connection).
//   - Context cancellation/timeout is honoured.
//   - Non-zero exit codes are returned as Result (not error).
func (s *SSHExecutor) Run(ctx context.Context, name string, args ...string) (Result, error) {
	client, err := s.getClient()
	if err != nil {
		return Result{}, fmt.Errorf("ssh connect: %w", err)
	}

	result, err := s.runSession(ctx, client, name, args...)
	if err != nil {
		// Session may have failed because the connection is stale; try once more.
		s.mu.Lock()
		s.client = nil
		s.mu.Unlock()

		client, connErr := s.getClient()
		if connErr != nil {
			return Result{}, fmt.Errorf("ssh reconnect: %w", connErr)
		}
		return s.runSession(ctx, client, name, args...)
	}
	return result, nil
}

// runSession opens a new SSH session, runs the command, and returns the result.
func (s *SSHExecutor) runSession(ctx context.Context, client *ssh.Client, name string, args ...string) (Result, error) {
	session, err := client.NewSession()
	if err != nil {
		return Result{}, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	// Build the full command string. Arguments are expected to already be
	// sanitized by the security layer before reaching the executor.
	parts := make([]string, 0, 1+len(args))
	parts = append(parts, name)
	parts = append(parts, args...)
	cmdStr := strings.Join(parts, " ")

	type outcome struct {
		result Result
		err    error
	}

	ch := make(chan outcome, 1)
	start := time.Now()

	go func() {
		out, runErr := session.CombinedOutput(cmdStr)
		duration := time.Since(start)

		if runErr != nil {
			if exitErr, ok := runErr.(*ssh.ExitError); ok {
				// Command ran but exited non-zero — not a transport error.
				ch <- outcome{
					result: Result{
						Output:   string(out),
						ExitCode: exitErr.ExitStatus(),
						Duration: duration,
					},
				}
				return
			}
			ch <- outcome{err: runErr}
			return
		}

		ch <- outcome{
			result: Result{
				Output:   string(out),
				ExitCode: 0,
				Duration: duration,
			},
		}
	}()

	select {
	case <-ctx.Done():
		// Kill the remote process by closing the session.
		_ = session.Signal(ssh.SIGKILL)
		return Result{}, ctx.Err()
	case o := <-ch:
		return o.result, o.err
	}
}

// getClient returns the cached SSH client, creating one if necessary.
func (s *SSHExecutor) getClient() (*ssh.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		return s.client, nil
	}

	// Read the private key.
	keyData, err := os.ReadFile(s.target.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("read key file %q: %w", s.target.KeyFile, err)
	}

	signer, err := ssh.ParsePrivateKey(keyData)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	sshCfg := &ssh.ClientConfig{
		User: s.target.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		// InsecureIgnoreHostKey is acceptable for a local/trusted Mac Mini.
		// A future improvement could use known_hosts verification.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(s.target.Host, fmt.Sprintf("%d", s.target.Port))
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	s.client = client
	return client, nil
}

// Close closes the underlying SSH connection if open.
func (s *SSHExecutor) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.client != nil {
		_ = s.client.Close()
		s.client = nil
	}
}
