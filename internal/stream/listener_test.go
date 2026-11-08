package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ─── fake Gotify server ──────────────────────────────────────────────────────

// testAppID is the command application the listeners under test follow.
const testAppID = 7

// testToken is the client token the listeners under test present.
const testToken = "C1ient-T0ken.secret"

// fakeGotify is a websocket endpoint shaped like Gotify's /stream. It hands
// every accepted connection and its handshake request to the test. It also
// answers GET /application/{id}/message from history, the way Gotify lists
// the stored messages of an application.
type fakeGotify struct {
	server   *httptest.Server
	requests chan *http.Request
	conns    chan *websocket.Conn

	// silent makes the server stop reading after the handshake, so it never
	// answers a ping: from the client's side the connection is dead.
	silent bool

	// release ends the handlers of silent connections when the test is done.
	release chan struct{}

	// fetches receives every request for the message list.
	fetches chan *http.Request

	// historyMu guards history and historyStatus, which the tests change
	// while the listener reads them.
	historyMu sync.Mutex

	// history holds the stored messages of the command app, oldest first.
	history []GotifyMessage

	// historyStatus, when set, is answered instead of the message list.
	historyStatus int
}

// newFakeGotify starts a fake server that is closed when the test ends.
func newFakeGotify(t *testing.T, silent bool) *fakeGotify {
	t.Helper()

	fake := &fakeGotify{
		requests: make(chan *http.Request, 16),
		conns:    make(chan *websocket.Conn, 16),
		silent:   silent,
		release:  make(chan struct{}),
		fetches:  make(chan *http.Request, 16),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /stream", fake.serveStream)
	mux.HandleFunc("GET /application/{id}/message", fake.serveMessages)
	fake.server = httptest.NewServer(mux)

	// Cleanups run last-in first-out: the silent handlers are released
	// before the server waits for them in Close.
	t.Cleanup(fake.server.Close)
	t.Cleanup(func() { close(fake.release) })

	return fake
}

// serveStream upgrades the request and keeps the connection open until the
// client goes away (or, for a silent server, until the test releases it).
func (fake *fakeGotify) serveStream(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	fake.requests <- r
	fake.conns <- conn

	if fake.silent {
		<-fake.release
		return
	}

	// Reading is what makes gorilla answer the client's pings with pongs.
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// serveMessages answers the message list of the command app, newest first
// and wrapped in the paging object, as Gotify does.
func (fake *fakeGotify) serveMessages(w http.ResponseWriter, r *http.Request) {
	select {
	case fake.fetches <- r:
	default:
	}

	if r.PathValue("id") != strconv.Itoa(testAppID) {
		http.NotFound(w, r)
		return
	}

	fake.historyMu.Lock()
	status := fake.historyStatus
	newestFirst := make([]GotifyMessage, 0, len(fake.history))
	for index := len(fake.history) - 1; index >= 0; index-- {
		newestFirst = append(newestFirst, fake.history[index])
	}
	fake.historyMu.Unlock()

	if status != 0 {
		http.Error(w, "unavailable", status)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"messages": newestFirst,
		"paging":   map[string]any{"size": len(newestFirst), "limit": 100},
	})
}

// store adds messages to the history of the command app.
func (fake *fakeGotify) store(messages ...GotifyMessage) {
	fake.historyMu.Lock()
	defer fake.historyMu.Unlock()

	fake.history = append(fake.history, messages...)
}

// failHistory makes the message list answer status from now on.
func (fake *fakeGotify) failHistory(status int) {
	fake.historyMu.Lock()
	defer fake.historyMu.Unlock()

	fake.historyStatus = status
}

// nextConn waits for the listener to connect and returns the server side.
func (fake *fakeGotify) nextConn(t *testing.T) *websocket.Conn {
	t.Helper()

	select {
	case conn := <-fake.conns:
		<-fake.requests
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not connect within 3s")
		return nil
	}
}

// send writes one Gotify message for the command application.
func send(t *testing.T, conn *websocket.Conn, text string) {
	t.Helper()

	sendRaw(t, conn, GotifyMessage{AppID: testAppID, Message: text})
}

// sendRaw writes any message the test builds, as JSON.
func sendRaw(t *testing.T, conn *websocket.Conn, message GotifyMessage) {
	t.Helper()

	data, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// ─── recording handler ───────────────────────────────────────────────────────

// recordingHandler implements Handler for the tests. Handle runs hook (when
// set) and then records the text; Busy and Expired record the text they were
// told about, and Expired the age as well.
type recordingHandler struct {
	handled chan string
	busy    chan string
	expired chan string
	ages    chan time.Duration
	hook    func(ctx context.Context, text string)
}

// newRecordingHandler returns a handler with room for plenty of messages.
func newRecordingHandler(hook func(ctx context.Context, text string)) *recordingHandler {
	return &recordingHandler{
		handled: make(chan string, 64),
		busy:    make(chan string, 64),
		expired: make(chan string, 64),
		ages:    make(chan time.Duration, 64),
		hook:    hook,
	}
}

// Handle implements Handler.
func (handler *recordingHandler) Handle(ctx context.Context, text string) {
	if handler.hook != nil {
		handler.hook(ctx, text)
	}
	handler.handled <- text
}

// Busy implements Handler.
func (handler *recordingHandler) Busy(text string) {
	handler.busy <- text
}

// Expired implements Handler.
func (handler *recordingHandler) Expired(text string, age time.Duration) {
	handler.ages <- age
	handler.expired <- text
}

// expect waits for the next text on channel and fails if it differs.
func expect(t *testing.T, channel chan string, want string) {
	t.Helper()

	select {
	case got := <-channel:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %q", want)
	}
}

// startListener builds a listener against fake with short timings and
// stops it when the test ends.
func startListener(t *testing.T, fake *fakeGotify, handler Handler) *Listener {
	t.Helper()

	listener := NewListenerWithHandler(fake.server.URL, testToken, testAppID, handler)
	listener.backoff = NewBackoff(10*time.Millisecond, 50*time.Millisecond)
	listener.Start()
	t.Cleanup(listener.Stop)

	return listener
}

// ─── tests ───────────────────────────────────────────────────────────────────

func TestListenerWSURL(t *testing.T) {
	cases := []struct {
		serverURL string
		want      string
	}{
		{"http://gotify.example.com", "ws://gotify.example.com/stream"},
		{"https://gotify.example.com", "wss://gotify.example.com/stream"},
		{"http://gotify.example.com/", "ws://gotify.example.com/stream"},
	}

	for _, tc := range cases {
		l := NewListener(tc.serverURL, "tok", 1, nil)
		got := l.wsURL()
		if got != tc.want {
			t.Errorf("wsURL(%q) = %q, want %q", tc.serverURL, got, tc.want)
		}
	}
}

// TestTokenTravelsInHeaderNotURL checks the handshake: the client token is in
// X-Gotify-Key and the URL carries no token= at all.
func TestTokenTravelsInHeaderNotURL(t *testing.T) {
	fake := newFakeGotify(t, false)
	startListener(t, fake, newRecordingHandler(nil))

	var request *http.Request
	select {
	case request = <-fake.requests:
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not connect within 3s")
	}

	if got := request.Header.Get("X-Gotify-Key"); got != testToken {
		t.Errorf("X-Gotify-Key = %q, want %q", got, testToken)
	}
	if strings.Contains(request.URL.String(), "token=") {
		t.Errorf("URL %q carries a token= parameter", request.URL.String())
	}
	if strings.Contains(request.URL.String(), testToken) {
		t.Errorf("URL %q carries the token", request.URL.String())
	}
}

// TestLogsNeverContainToken captures the package's log lines through a
// successful connection and a failed dial and looks for the token.
func TestLogsNeverContainToken(t *testing.T) {
	var output syncBuffer
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(defaultLogOutput) })

	// A delivered message proves the client side finished its handshake and
	// logged it; the server side alone may see the upgrade earlier.
	fake := newFakeGotify(t, false)
	handler := newRecordingHandler(nil)
	connected := startListener(t, fake, handler)
	send(t, fake.nextConn(t), "ping")
	expect(t, handler.handled, "ping")
	connected.Stop()

	// Nothing listens on the discard port, so every dial fails and logs.
	failing := NewListenerWithHandler("http://127.0.0.1:9/?token="+testToken, testToken, testAppID, newRecordingHandler(nil))
	failing.backoff = NewBackoff(10*time.Millisecond, 20*time.Millisecond)
	failing.Start()
	time.Sleep(100 * time.Millisecond)
	failing.Stop()

	lines := output.String()
	if !strings.Contains(lines, "connected to") || !strings.Contains(lines, "connection error") {
		t.Fatalf("expected both a connect and an error line, got:\n%s", lines)
	}
	if strings.Contains(lines, testToken) {
		t.Errorf("a log line contains the token:\n%s", lines)
	}
}

// TestMessagesAreFiltered sends everything the listener must ignore before
// the two messages it must deliver.
func TestMessagesAreFiltered(t *testing.T) {
	fake := newFakeGotify(t, false)
	handler := newRecordingHandler(nil)
	startListener(t, fake, handler)
	conn := fake.nextConn(t)

	if err := conn.WriteMessage(websocket.TextMessage, []byte("not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sendRaw(t, conn, GotifyMessage{AppID: testAppID + 1, Message: "other app"})
	sendRaw(t, conn, GotifyMessage{AppID: testAppID, Message: "our own reply", Extras: map[string]interface{}{"commander::response": true}})
	sendRaw(t, conn, GotifyMessage{AppID: testAppID})
	sendRaw(t, conn, GotifyMessage{AppID: testAppID, Title: "title only"})
	send(t, conn, "last")

	expect(t, handler.handled, "title only")
	expect(t, handler.handled, "last")
}

// TestPanicInHandlerIsRecovered: a handler that panics must not take the
// process down, and the next message must still be delivered.
func TestPanicInHandlerIsRecovered(t *testing.T) {
	fake := newFakeGotify(t, false)
	handler := newRecordingHandler(func(_ context.Context, text string) {
		if text == "boom" {
			panic("handler exploded")
		}
	})
	startListener(t, fake, handler)
	conn := fake.nextConn(t)

	send(t, conn, "boom")
	send(t, conn, "ping")

	// The panicking call never reaches the record step, so "ping" is next.
	expect(t, handler.handled, "ping")
}

// TestPanicInBusyIsRecovered: the Busy callback runs on the read loop, so a
// panic there must not end the reading either.
func TestPanicInBusyIsRecovered(t *testing.T) {
	fake := newFakeGotify(t, false)
	block := make(chan struct{})
	handler := &panickingBusyHandler{recordingHandler: newRecordingHandler(func(_ context.Context, text string) {
		if text == "slow" {
			<-block
		}
	})}
	startListener(t, fake, handler)
	conn := fake.nextConn(t)

	send(t, conn, "slow")
	for index := 0; index < queueCapacity+2; index++ {
		send(t, conn, "filler")
	}
	time.Sleep(100 * time.Millisecond)
	close(block)

	expect(t, handler.handled, "slow")
	for index := 0; index < queueCapacity; index++ {
		expect(t, handler.handled, "filler")
	}

	send(t, conn, "after")
	expect(t, handler.handled, "after")
}

// panickingBusyHandler is a recordingHandler whose Busy panics.
type panickingBusyHandler struct {
	*recordingHandler
}

// Busy implements Handler by panicking.
func (handler *panickingBusyHandler) Busy(string) {
	panic("busy exploded")
}

// TestSlowHandlerDoesNotBlockReading: while one command runs, the read loop
// keeps reading, fills the queue and reports the overflow as busy.
func TestSlowHandlerDoesNotBlockReading(t *testing.T) {
	fake := newFakeGotify(t, false)
	started := make(chan struct{})
	block := make(chan struct{})
	handler := newRecordingHandler(func(_ context.Context, text string) {
		if text == "traffic" {
			close(started)
			<-block
		}
	})
	startListener(t, fake, handler)
	conn := fake.nextConn(t)

	send(t, conn, "traffic")
	<-started

	for index := 0; index < queueCapacity; index++ {
		send(t, conn, "queued")
	}
	send(t, conn, "overflow")

	expect(t, handler.busy, "overflow")

	close(block)
	expect(t, handler.handled, "traffic")
	for index := 0; index < queueCapacity; index++ {
		expect(t, handler.handled, "queued")
	}
}

// TestDeadConnectionReconnects: a server that never answers pings is
// dropped after the pong wait and the listener dials again.
func TestDeadConnectionReconnects(t *testing.T) {
	fake := newFakeGotify(t, true)
	listener := NewListenerWithHandler(fake.server.URL, testToken, testAppID, newRecordingHandler(nil))
	listener.backoff = NewBackoff(10*time.Millisecond, 50*time.Millisecond)
	listener.pingInterval = 20 * time.Millisecond
	listener.pongWait = 100 * time.Millisecond
	listener.Start()
	t.Cleanup(listener.Stop)

	fake.nextConn(t)
	fake.nextConn(t)
}

// TestLiveConnectionIsKept: a server that answers pings keeps the single
// connection well past several pong waits.
func TestLiveConnectionIsKept(t *testing.T) {
	fake := newFakeGotify(t, false)
	handler := newRecordingHandler(nil)
	listener := NewListenerWithHandler(fake.server.URL, testToken, testAppID, handler)
	listener.backoff = NewBackoff(10*time.Millisecond, 50*time.Millisecond)
	listener.pingInterval = 20 * time.Millisecond
	listener.pongWait = 100 * time.Millisecond
	listener.Start()
	t.Cleanup(listener.Stop)

	conn := fake.nextConn(t)
	time.Sleep(400 * time.Millisecond)

	select {
	case <-fake.conns:
		t.Fatal("listener reconnected although the server answered every ping")
	default:
	}

	send(t, conn, "still here")
	expect(t, handler.handled, "still here")
}

// TestStopWhileConnectedIsRaceFree runs under -race: Stop right after Start,
// Stop while connected, Stop twice and Stop on a listener never started.
func TestStopWhileConnectedIsRaceFree(t *testing.T) {
	fake := newFakeGotify(t, false)

	immediate := NewListenerWithHandler(fake.server.URL, testToken, testAppID, newRecordingHandler(nil))
	immediate.Start()
	immediate.Stop()

	connected := NewListenerWithHandler(fake.server.URL, testToken, testAppID, newRecordingHandler(nil))
	connected.Start()
	fake.nextConn(t)

	began := time.Now()
	connected.Stop()
	connected.Stop()

	// Stop only returns early when every goroutine has finished; a timeout
	// would take the whole stopTimeout.
	if elapsed := time.Since(began); elapsed >= connected.stopTimeout {
		t.Errorf("Stop took %v, the goroutines did not finish", elapsed)
	}

	// Start after Stop is a no-op: the listener is single-use.
	connected.Start()
	select {
	case <-fake.conns:
		t.Error("Start after Stop connected again")
	case <-time.After(100 * time.Millisecond):
	}

	never := NewListener(fake.server.URL, testToken, testAppID, func(string) {})
	never.Stop()
}

// TestStopCancelsHandlerContext: the context handed to Handle is cancelled
// by Stop, so a command that honours it ends and Stop does not time out.
func TestStopCancelsHandlerContext(t *testing.T) {
	fake := newFakeGotify(t, false)
	started := make(chan struct{})
	cancelled := make(chan error, 1)
	handler := newRecordingHandler(func(ctx context.Context, _ string) {
		close(started)
		<-ctx.Done()
		cancelled <- ctx.Err()
	})
	listener := startListener(t, fake, handler)
	conn := fake.nextConn(t)

	send(t, conn, "traffic")
	<-started

	began := time.Now()
	listener.Stop()
	if elapsed := time.Since(began); elapsed >= listener.stopTimeout {
		t.Errorf("Stop took %v, the handler context was not cancelled", elapsed)
	}

	if err := <-cancelled; err != context.Canceled {
		t.Errorf("handler context error = %v, want context.Canceled", err)
	}
}

// TestStopGivesUpOnStuckHandler: a handler that ignores its context makes
// Stop wait only stopTimeout, so a caller holding a lock is never stuck.
func TestStopGivesUpOnStuckHandler(t *testing.T) {
	fake := newFakeGotify(t, false)
	started := make(chan struct{})
	block := make(chan struct{})
	handler := newRecordingHandler(func(context.Context, string) {
		close(started)
		<-block
	})
	listener := startListener(t, fake, handler)
	listener.stopTimeout = 100 * time.Millisecond
	conn := fake.nextConn(t)

	send(t, conn, "stuck")
	<-started

	began := time.Now()
	listener.Stop()
	elapsed := time.Since(began)
	close(block)

	if elapsed > time.Second {
		t.Errorf("Stop took %v with a stuck handler, want about 100ms", elapsed)
	}
}

// TestNewListenerKeepsFuncHandler: the original constructor still delivers
// to a plain function.
func TestNewListenerKeepsFuncHandler(t *testing.T) {
	fake := newFakeGotify(t, false)
	handled := make(chan string, 1)
	listener := NewListener(fake.server.URL, testToken, testAppID, func(text string) {
		handled <- text
	})
	listener.Start()
	t.Cleanup(listener.Stop)
	conn := fake.nextConn(t)

	send(t, conn, "ping")
	expect(t, handled, "ping")
}

// ─── log capture ─────────────────────────────────────────────────────────────

// defaultLogOutput is where the standard logger writes outside this test.
var defaultLogOutput = log.Writer()

// syncBuffer is a bytes.Buffer safe for the logger's concurrent writes.
type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// Write implements io.Writer.
func (buffer *syncBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return buffer.buffer.Write(data)
}

// String returns everything written so far.
func (buffer *syncBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()

	return buffer.buffer.String()
}
