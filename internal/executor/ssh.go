package executor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHExecutor runs commands on a remote machine via SSH.
type SSHExecutor struct {
	target config.SSHTarget

	// KnownHostsFile is the OpenSSH known_hosts file the remote host key is
	// checked against. Empty means ~/.ssh/known_hosts of the user running the
	// plugin. A host that is missing or whose key differs is refused.
	KnownHostsFile string

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
		// A timeout says nothing about the connection, and running the
		// command again with a finished context would fail anyway.
		if ctx.Err() != nil {
			return Result{}, err
		}

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

	// The SSH protocol carries one command line that the remote user's shell
	// parses, so every word is quoted: an argument cannot add a command.
	cmdStr := remoteCommand(name, args...)

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

	addr := net.JoinHostPort(s.target.Host, strconv.Itoa(s.target.Port))

	hostKeyCallback, hostKeyAlgorithms, err := s.hostKeyCheck(addr)
	if err != nil {
		return nil, err
	}

	sshCfg := &ssh.ClientConfig{
		User: s.target.User,
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback:   hostKeyCallback,
		HostKeyAlgorithms: hostKeyAlgorithms,
		Timeout:           10 * time.Second,
	}

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

// knownHostsPath returns KnownHostsFile, or ~/.ssh/known_hosts of the user
// running the plugin when it is empty.
func (s *SSHExecutor) knownHostsPath() (string, error) {
	if s.KnownHostsFile != "" {
		return s.KnownHostsFile, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find known_hosts: %w", err)
	}

	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// hostKeyCheck loads the known_hosts file and returns the callback that
// verifies addr's host key, plus the key algorithms recorded for addr.
//
// The algorithms matter: without them the client offers RSA and ECDSA before
// Ed25519, the server answers with a key type the file may not hold, and a
// correct entry is reported as a mismatch.
func (s *SSHExecutor) hostKeyCheck(addr string) (ssh.HostKeyCallback, []string, error) {
	path, err := s.knownHostsPath()
	if err != nil {
		return nil, nil, err
	}

	callback, err := knownhosts.New(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load known_hosts %q (add the host with ssh-keyscan): %w", path, err)
	}

	algorithms, err := knownHostAlgorithms(callback, addr)
	if err != nil {
		return nil, nil, fmt.Errorf("host %s is not in known_hosts %q (add it with ssh-keyscan): %w", addr, path, err)
	}

	return callback, algorithms, nil
}

// knownHostAlgorithms asks callback which keys it holds for addr by offering
// a key that cannot match. knownhosts answers with a KeyError listing the
// recorded keys; an empty list means the host is unknown.
func knownHostAlgorithms(callback ssh.HostKeyCallback, addr string) ([]string, error) {
	probeKey, err := ssh.NewPublicKey(make(ed25519.PublicKey, ed25519.PublicKeySize))
	if err != nil {
		return nil, err
	}

	// The remote address is only read when the hostname is empty, but it must
	// still parse as host:port.
	probeRemote := &net.TCPAddr{IP: net.IPv4zero}

	var keyError *knownhosts.KeyError
	err = callback(addr, probeRemote, probeKey)
	if !errors.As(err, &keyError) {
		return nil, fmt.Errorf("unexpected known_hosts answer: %v", err)
	}
	if len(keyError.Want) == 0 {
		return nil, keyError
	}

	var algorithms []string
	for _, known := range keyError.Want {
		// An "ssh-rsa" entry is a key type, not an algorithm: the SHA-2
		// signatures are what the client negotiates for it.
		if known.Key.Type() == ssh.KeyAlgoRSA {
			algorithms = append(algorithms, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
			continue
		}
		algorithms = append(algorithms, known.Key.Type())
	}

	return algorithms, nil
}
