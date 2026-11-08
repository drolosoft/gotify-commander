package stream

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// ─── checkpoint fake ─────────────────────────────────────────────────────────

// memoryCheckpoint is a Checkpoint kept in memory. Every saved ID is also
// sent to saved, so a test can see the order of the saves.
type memoryCheckpoint struct {
	mu      sync.Mutex
	id      int
	loadErr error
	saved   chan int
}

// newMemoryCheckpoint returns a checkpoint that starts at id; 0 is a plugin
// that never saved one.
func newMemoryCheckpoint(id int) *memoryCheckpoint {
	return &memoryCheckpoint{id: id, saved: make(chan int, 64)}
}

// Load implements Checkpoint.
func (checkpoint *memoryCheckpoint) Load() (int, error) {
	checkpoint.mu.Lock()
	defer checkpoint.mu.Unlock()

	return checkpoint.id, checkpoint.loadErr
}

// Save implements Checkpoint.
func (checkpoint *memoryCheckpoint) Save(id int) error {
	checkpoint.mu.Lock()
	checkpoint.id = id
	checkpoint.mu.Unlock()

	checkpoint.saved <- id
	return nil
}

// current returns the last saved ID.
func (checkpoint *memoryCheckpoint) current() int {
	checkpoint.mu.Lock()
	defer checkpoint.mu.Unlock()

	return checkpoint.id
}

// expectSaved waits for the next save and fails if it is not want.
func expectSaved(t *testing.T, checkpoint *memoryCheckpoint, want int) {
	t.Helper()

	select {
	case got := <-checkpoint.saved:
		if got != want {
			t.Fatalf("checkpoint saved %d, want %d", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for checkpoint %d", want)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────────────

// stored returns a message of the command app with id, posted age ago.
func stored(id int, text string, age time.Duration) GotifyMessage {
	return GotifyMessage{
		ID:      id,
		AppID:   testAppID,
		Message: text,
		Date:    time.Now().Add(-age),
	}
}

// startReplayListener starts a listener on fake that keeps its checkpoint in
// checkpoint and stops it when the test ends.
func startReplayListener(t *testing.T, fake *fakeGotify, handler Handler, checkpoint Checkpoint) *Listener {
	t.Helper()

	listener := NewListenerWithHandler(fake.server.URL, testToken, testAppID, handler).WithCheckpoint(checkpoint)
	listener.backoff = NewBackoff(10*time.Millisecond, 50*time.Millisecond)
	listener.Start()
	t.Cleanup(listener.Stop)

	return listener
}

// expectNothing fails if channel receives anything within a short while.
func expectNothing(t *testing.T, channel chan string, what string) {
	t.Helper()

	select {
	case got := <-channel:
		t.Fatalf("unexpected %s %q", what, got)
	case <-time.After(150 * time.Millisecond):
	}
}

// ─── tests ───────────────────────────────────────────────────────────────────

// TestPendingMessageRunsOnConnect is the bug of 2026-11-08: a command posted
// while the plugin was disconnected runs once it connects, and only once.
func TestPendingMessageRunsOnConnect(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(5, "ping", 30*time.Second))
	handler := newRecordingHandler(nil)
	checkpoint := newMemoryCheckpoint(4)
	startReplayListener(t, fake, handler, checkpoint)
	conn := fake.nextConn(t)

	expect(t, handler.handled, "ping")
	expectSaved(t, checkpoint, 5)

	sendRaw(t, conn, stored(6, "after", 0))
	expect(t, handler.handled, "after")
}

// TestHistoryRequest checks the request for the pending messages: the
// command app, one page of 100, the client token in the header only.
func TestHistoryRequest(t *testing.T) {
	fake := newFakeGotify(t, false)
	startReplayListener(t, fake, newRecordingHandler(nil), newMemoryCheckpoint(4))

	var request *http.Request
	select {
	case request = <-fake.fetches:
	case <-time.After(3 * time.Second):
		t.Fatal("the listener did not ask for the pending messages")
	}

	if got := request.Header.Get("X-Gotify-Key"); got != testToken {
		t.Errorf("X-Gotify-Key = %q, want %q", got, testToken)
	}
	if got := request.URL.Query().Get("limit"); got != "100" {
		t.Errorf("limit = %q, want 100", got)
	}
	if request.URL.Query().Has("since") {
		t.Errorf("URL %q carries since=, which pages towards older messages", request.URL.String())
	}
	if strings.Contains(request.URL.String(), testToken) {
		t.Errorf("URL %q carries the token", request.URL.String())
	}
}

// TestPendingMessageAlsoPushedLiveRunsOnce: a message posted between the
// dial and the history request arrives both ways and must run once.
func TestPendingMessageAlsoPushedLiveRunsOnce(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(5, "ping", time.Second))
	handler := newRecordingHandler(nil)
	startReplayListener(t, fake, handler, newMemoryCheckpoint(4))
	conn := fake.nextConn(t)

	sendRaw(t, conn, stored(5, "ping", time.Second))
	sendRaw(t, conn, stored(6, "after", 0))

	// The worker keeps the order, so a second ping would come before after.
	expect(t, handler.handled, "ping")
	expect(t, handler.handled, "after")
}

// TestPendingMessagesRunOldestFirst: Gotify lists newest first, the user
// typed them oldest first, and "stop x" before "start x" must stay so.
func TestPendingMessagesRunOldestFirst(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(5, "stop x", 20*time.Second), stored(6, "start x", 10*time.Second))
	handler := newRecordingHandler(nil)
	startReplayListener(t, fake, handler, newMemoryCheckpoint(4))

	expect(t, handler.handled, "stop x")
	expect(t, handler.handled, "start x")
}

// TestPendingMessageOlderThanWindowExpires: a pending command older than
// the replay window is reported as expired and never run.
func TestPendingMessageOlderThanWindowExpires(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(
		stored(3, "reboot", 10*time.Minute),
		GotifyMessage{ID: 4, AppID: testAppID, Message: "restart nginx"},
		stored(5, "ping", time.Minute),
	)
	handler := newRecordingHandler(nil)
	checkpoint := newMemoryCheckpoint(2)
	startReplayListener(t, fake, handler, checkpoint)

	expect(t, handler.expired, "reboot")
	if age := <-handler.ages; age < 10*time.Minute || age > 11*time.Minute {
		t.Errorf("age of the expired command = %v, want about 10m", age)
	}

	// A message without a date has no age to compare, so it is not run.
	expect(t, handler.expired, "restart nginx")
	<-handler.ages

	expect(t, handler.handled, "ping")
	if got := checkpoint.current(); got != 5 {
		t.Errorf("checkpoint = %d, want 5", got)
	}
}

// TestReconnectReplaysNothingTwice: after a dropped connection the history
// is read again, and only what arrived in between runs.
func TestReconnectReplaysNothingTwice(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(5, "ping", time.Second))
	handler := newRecordingHandler(nil)
	startReplayListener(t, fake, handler, newMemoryCheckpoint(4))
	first := fake.nextConn(t)
	expect(t, handler.handled, "ping")

	// Gotify stores every message it pushes, so 6 is in the history too.
	fake.store(stored(6, "uptime", 0))
	sendRaw(t, first, stored(6, "uptime", 0))
	expect(t, handler.handled, "uptime")

	// 7 is posted while the plugin is away: stored, never pushed.
	fake.store(stored(7, "status", 0))
	_ = first.Close()
	second := fake.nextConn(t)
	expect(t, handler.handled, "status")

	sendRaw(t, second, stored(8, "last", 0))
	expect(t, handler.handled, "last")
	expectNothing(t, handler.handled, "command")
}

// TestRestartResumesFromCheckpoint is the Gotify restart: a new listener
// over the same checkpoint runs only what the old one had not taken.
func TestRestartResumesFromCheckpoint(t *testing.T) {
	fake := newFakeGotify(t, false)
	checkpoint := newMemoryCheckpoint(4)

	before := newRecordingHandler(nil)
	old := startReplayListener(t, fake, before, checkpoint)
	conn := fake.nextConn(t)
	fake.store(stored(5, "ping", 0))
	sendRaw(t, conn, stored(5, "ping", 0))
	expect(t, before.handled, "ping")
	old.Stop()

	fake.store(stored(6, "uptime", 0))
	after := newRecordingHandler(nil)
	startReplayListener(t, fake, after, checkpoint)

	expect(t, after.handled, "uptime")
	expectNothing(t, after.handled, "command")
}

// TestFirstRunDoesNotReplayHistory: without a checkpoint the history is
// old business (a reboot from last month), so none of it runs and the newest
// ID becomes the checkpoint.
func TestFirstRunDoesNotReplayHistory(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(1, "reboot", time.Minute), stored(2, "ping", time.Second))
	handler := newRecordingHandler(nil)
	checkpoint := newMemoryCheckpoint(0)
	startReplayListener(t, fake, handler, checkpoint)
	conn := fake.nextConn(t)

	expectSaved(t, checkpoint, 2)

	sendRaw(t, conn, stored(3, "uptime", 0))
	expect(t, handler.handled, "uptime")
	expectNothing(t, handler.expired, "expired command")
}

// TestUnreadableCheckpointDoesNotReplayHistory: a checkpoint that cannot be
// read is treated as a first run, the side that never runs an old command.
func TestUnreadableCheckpointDoesNotReplayHistory(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(1, "reboot", time.Second))
	handler := newRecordingHandler(nil)
	checkpoint := newMemoryCheckpoint(0)
	checkpoint.loadErr = errors.New("database is locked")
	startReplayListener(t, fake, handler, checkpoint)
	conn := fake.nextConn(t)

	sendRaw(t, conn, stored(2, "ping", 0))
	expect(t, handler.handled, "ping")
}

// TestCheckpointSavedForEachHandledMessage: every command is saved before it
// runs, so a "reboot" that takes the machine down with it is not run again
// by the replay after the boot.
func TestCheckpointSavedForEachHandledMessage(t *testing.T) {
	fake := newFakeGotify(t, false)
	checkpoint := newMemoryCheckpoint(9)
	saved := make(chan int, 4)
	handler := newRecordingHandler(func(context.Context, string) {
		saved <- checkpoint.current()
	})
	startReplayListener(t, fake, handler, checkpoint)
	conn := fake.nextConn(t)

	sendRaw(t, conn, stored(10, "ping", 0))
	sendRaw(t, conn, stored(11, "uptime", 0))

	expect(t, handler.handled, "ping")
	expect(t, handler.handled, "uptime")
	expectSaved(t, checkpoint, 10)
	expectSaved(t, checkpoint, 11)

	if got := <-saved; got != 10 {
		t.Errorf("checkpoint while ping ran = %d, want 10", got)
	}
	if got := <-saved; got != 11 {
		t.Errorf("checkpoint while uptime ran = %d, want 11", got)
	}
}

// TestReplayWindowZeroTurnsReplayOff: with a window of 0 the history is not
// even read and only live messages run.
func TestReplayWindowZeroTurnsReplayOff(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.store(stored(5, "ping", time.Second))
	handler := newRecordingHandler(nil)
	listener := NewListenerWithHandler(fake.server.URL, testToken, testAppID, handler).
		WithCheckpoint(newMemoryCheckpoint(4)).
		WithReplayWindow(0)
	listener.Start()
	t.Cleanup(listener.Stop)
	conn := fake.nextConn(t)

	sendRaw(t, conn, stored(6, "uptime", 0))
	expect(t, handler.handled, "uptime")

	select {
	case <-fake.fetches:
		t.Error("the history was read with the replay turned off")
	default:
	}
}

// TestHistoryFailureKeepsTheStream: a history request that fails is logged
// and the live stream goes on; the next connection can still replay.
func TestHistoryFailureKeepsTheStream(t *testing.T) {
	fake := newFakeGotify(t, false)
	fake.failHistory(http.StatusInternalServerError)
	handler := newRecordingHandler(nil)
	startReplayListener(t, fake, handler, newMemoryCheckpoint(4))
	conn := fake.nextConn(t)

	sendRaw(t, conn, stored(5, "ping", 0))
	expect(t, handler.handled, "ping")
}
