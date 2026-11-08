package executor

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/drolosoft/gotify-commander/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
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

// testSSHServer is an in-process SSH server that accepts any client key,
// records the command of every exec request and answers it with "ok" and
// exit status 0.
type testSSHServer struct {
	address string

	// hostKey is the server's Ed25519 key; the RSA key it also offers is
	// never written to known_hosts.
	hostKey ssh.PublicKey

	mu       sync.Mutex
	commands []string
}

// recorded returns a copy of the commands the server received so far.
func (s *testSSHServer) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]string(nil), s.commands...)
}

// startTestSSHServer listens on a free local port until the test ends.
func startTestSSHServer(t *testing.T) *testSSHServer {
	t.Helper()

	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}

	serverConfig := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	serverConfig.AddHostKey(hostSigner)

	// A real sshd also offers an RSA key, which the Go client prefers over
	// Ed25519. Trusting only the Ed25519 key must still connect.
	rsaPrivate, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	rsaSigner, err := ssh.NewSignerFromKey(rsaPrivate)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig.AddHostKey(rsaSigner)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })

	server := &testSSHServer{address: listener.Addr().String(), hostKey: hostSigner.PublicKey()}

	// The accept loop ends when the cleanup above closes the listener, and
	// each connection's goroutine ends when the client hangs up.
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn, serverConfig)
		}
	}()

	return server
}

// serve runs the SSH handshake on one connection and answers its sessions.
func (s *testSSHServer) serve(conn net.Conn, serverConfig *ssh.ServerConfig) {
	defer conn.Close()

	_, channels, requests, err := ssh.NewServerConn(conn, serverConfig)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)

	for newChannel := range channels {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}

		channel, channelRequests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.answerSession(channel, channelRequests)
	}
}

// answerSession records the exec request of one session and closes it with
// exit status 0.
func (s *testSSHServer) answerSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()

	for request := range requests {
		if request.Type != "exec" {
			request.Reply(false, nil)
			continue
		}

		var payload struct{ Command string }
		ssh.Unmarshal(request.Payload, &payload)

		s.mu.Lock()
		s.commands = append(s.commands, payload.Command)
		s.mu.Unlock()

		request.Reply(true, nil)
		channel.Write([]byte("ok\n"))
		channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
		return
	}
}

// writeClientKey writes a fresh OpenSSH private key and returns its path.
func writeClientKey(t *testing.T) string {
	t.Helper()

	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "test")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeKnownHosts writes a known_hosts file trusting key for address and
// returns its path.
func writeKnownHosts(t *testing.T, address string, key ssh.PublicKey) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "known_hosts")
	line := knownhosts.Line([]string{address}, key) + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newTestExecutor returns an SSHExecutor aimed at server with a fresh client key.
func newTestExecutor(t *testing.T, server *testSSHServer, knownHostsFile string) *SSHExecutor {
	t.Helper()

	host, portText, err := net.SplitHostPort(server.address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}

	executor := NewSSHExecutor(config.SSHTarget{Host: host, Port: port, User: "tester", KeyFile: writeClientKey(t)})
	executor.KnownHostsFile = knownHostsFile
	t.Cleanup(executor.Close)
	return executor
}

// TestSSHCommandIsQuoted checks that an argument carrying shell syntax reaches
// the remote shell as one single-quoted word.
func TestSSHCommandIsQuoted(t *testing.T) {
	server := startTestSSHServer(t)
	executor := newTestExecutor(t, server, writeKnownHosts(t, server.address, server.hostKey))

	result, err := executor.Run(context.Background(), "echo", "foo; rm -rf /", "$(id)")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(result.Output) != "ok" {
		t.Errorf("unexpected output %q", result.Output)
	}

	commands := server.recorded()
	want := "echo 'foo; rm -rf /' '$(id)'"
	if len(commands) != 1 || commands[0] != want {
		t.Fatalf("server received %q, want [%q]", commands, want)
	}
}

// TestSSHMissingKnownHostsFails checks that a missing known_hosts file is a
// clear error and that nothing runs: there is no fallback to trusting any key.
func TestSSHMissingKnownHostsFails(t *testing.T) {
	server := startTestSSHServer(t)
	missing := filepath.Join(t.TempDir(), "no_such_known_hosts")
	executor := newTestExecutor(t, server, missing)

	_, err := executor.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatal("expected an error without known_hosts, got none")
	}
	if !strings.Contains(err.Error(), "known_hosts") || !strings.Contains(err.Error(), missing) {
		t.Errorf("error does not name the known_hosts file: %v", err)
	}

	if commands := server.recorded(); len(commands) != 0 {
		t.Errorf("a command reached the server: %q", commands)
	}
}

// TestSSHUnknownHostFails checks that a known_hosts file without an entry
// for the target refuses the connection.
func TestSSHUnknownHostFails(t *testing.T) {
	server := startTestSSHServer(t)
	knownHosts := writeKnownHosts(t, "192.0.2.1:22", server.hostKey)
	executor := newTestExecutor(t, server, knownHosts)

	_, err := executor.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatal("expected an error for a host missing from known_hosts")
	}
	if !strings.Contains(err.Error(), "not in") {
		t.Errorf("error does not explain the host is unknown: %v", err)
	}

	if commands := server.recorded(); len(commands) != 0 {
		t.Errorf("a command reached the server: %q", commands)
	}
}

// TestSSHHostKeyMismatchFails checks that a different key on record for the
// target refuses the connection, as a man in the middle would look.
func TestSSHHostKeyMismatchFails(t *testing.T) {
	server := startTestSSHServer(t)

	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := ssh.NewPublicKey(otherPublic)
	if err != nil {
		t.Fatal(err)
	}
	executor := newTestExecutor(t, server, writeKnownHosts(t, server.address, otherKey))

	_, err = executor.Run(context.Background(), "echo", "hello")
	if err == nil {
		t.Fatal("expected an error for a mismatching host key")
	}

	var keyError *knownhosts.KeyError
	if !errors.As(err, &keyError) {
		t.Errorf("expected a knownhosts.KeyError, got %v", err)
	}
	if commands := server.recorded(); len(commands) != 0 {
		t.Errorf("a command reached the server: %q", commands)
	}
}

// TestSSHDefaultKnownHostsPath checks that without KnownHostsFile the running
// user's ~/.ssh/known_hosts is used.
func TestSSHDefaultKnownHostsPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	executor := NewSSHExecutor(config.SSHTarget{Host: "1.2.3.4"})
	path, err := executor.knownHostsPath()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := filepath.Join(home, ".ssh", "known_hosts")
	if path != want {
		t.Errorf("knownHostsPath = %q, want %q", path, want)
	}
}
