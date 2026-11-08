package stream

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultReplayWindow is how old a pending command may be and still run when
// the listener connects, unless WithReplayWindow says otherwise. It matches
// the default of defaults.replay_window in the configuration.
const DefaultReplayWindow = 5 * time.Minute

// historyPageSize is how many stored messages are read on connect. Gotify
// does not push what was posted while a client was away, and anything past
// the newest hundred is far older than any sensible replay window.
const historyPageSize = 100

// maxHistoryBytes bounds the answer of the history request. A hundred
// command messages are a few kilobytes; the limit only stops a broken or
// hostile server from filling the memory of the Gotify process.
const maxHistoryBytes = 4 << 20

// historyTimeout bounds the history request. The socket is already open and
// its messages wait in the buffer meanwhile, so a slow answer only delays
// them; it must not hold them for longer than the server's own ping time.
const historyTimeout = 10 * time.Second

// Checkpoint keeps the ID of the last command the listener took from the
// stream (run, refused or expired), so a listener made after a Gotify
// restart knows which stored messages are new.
//
// Load returns 0 when nothing was saved yet: Gotify message IDs start at 1.
// Save is called from the read loop and from the worker, never at the same
// time, with IDs that only grow.
type Checkpoint interface {
	Load() (int, error)
	Save(id int) error
}

// replay reads the stored messages of the command app and queues, oldest
// first, the commands posted since the last one taken. The read loop calls
// it on every connection before reading the socket.
func (l *Listener) replay(ctx context.Context) {
	if l.replayWindow <= 0 {
		return
	}

	messages, err := l.fetchHistory(ctx)
	if err != nil {
		// A stopping listener cancels the request; that is not worth a line.
		if ctx.Err() == nil {
			l.logf("could not read the pending messages, only new ones will run: %v", err)
		}
		return
	}

	// The checkpoint is read once per listener, on the first history that
	// arrives; from then on lastSeenID is ahead of it.
	if !l.baselined {
		l.baselined = true
		l.lastSeenID = max(l.lastSeenID, l.loadCheckpoint())

		if l.lastSeenID == 0 {
			l.startAfter(messages)
			return
		}
	}

	// Gotify lists newest first; the user typed them oldest first, and
	// "stop x" before "start x" must keep that order.
	slices.SortFunc(messages, func(first, second GotifyMessage) int {
		return cmp.Compare(first.ID, second.ID)
	})

	replayed := 0
	for _, message := range messages {
		text, ok := l.commandOf(message)
		if !ok {
			continue
		}

		if !l.claim(message.ID) {
			continue
		}

		// A message without a date has no age to compare, and running a
		// command that may be weeks old is the worse mistake.
		age := time.Since(message.Date)
		if message.Date.IsZero() {
			age = 0
		}
		if message.Date.IsZero() || age > l.replayWindow {
			l.expire(message.ID, text, age)
			continue
		}

		l.enqueue(pendingCommand{id: message.ID, text: text})
		replayed++
	}

	if replayed > 0 {
		l.logf("replayed %d command(s) posted while disconnected", replayed)
	}
}

// startAfter is the first connection of a plugin that never saved a
// checkpoint. Nothing in the history runs: it may hold a "reboot" from last
// month. The newest ID becomes the checkpoint, so the next restart replays
// only what comes after it.
func (l *Listener) startAfter(messages []GotifyMessage) {
	newest := 0
	for _, message := range messages {
		newest = max(newest, message.ID)
	}

	l.lastSeenID = newest
	l.commit(newest)

	if len(messages) > 0 {
		l.logf("first start: %d stored message(s) are not replayed", len(messages))
	}
}

// loadCheckpoint returns the saved ID, or 0 without a checkpoint. A
// checkpoint that cannot be read also gives 0, which means no replay: the
// safe side for a plugin that runs commands on the host.
func (l *Listener) loadCheckpoint() int {
	if l.checkpoint == nil {
		return 0
	}

	id, err := l.checkpoint.Load()
	if err != nil {
		l.logf("could not read the stream checkpoint, pending commands are not replayed: %v", err)
		return 0
	}

	return max(id, 0)
}

// commit saves id as the last command taken, unless a higher one was saved
// already. It is called before a command runs, and for the ones refused or
// expired, so none of them comes back after a restart.
func (l *Listener) commit(id int) {
	if id <= 0 || l.checkpoint == nil {
		return
	}

	l.checkpointMu.Lock()
	defer l.checkpointMu.Unlock()
	defer l.recoverPanic("checkpoint")

	if id <= l.savedID {
		return
	}
	l.savedID = id

	// The ID stays taken in memory, so this listener does not replay it;
	// only a restart before the next save could.
	if err := l.checkpoint.Save(id); err != nil {
		l.logf("could not save the stream checkpoint: %v", err)
	}
}

// expire reports a pending command that is older than the replay window and
// keeps it from coming back.
func (l *Listener) expire(id int, text string, age time.Duration) {
	l.commit(id)

	if age == 0 {
		l.logf("not running %q: the message has no date to compare with the replay window", commandWord(text))
	} else {
		l.logf("not running %q: posted %s ago, the replay window is %s", commandWord(text), age.Round(time.Second), l.replayWindow)
	}

	expiry, ok := l.handler.(ExpiryHandler)
	if !ok {
		return
	}

	defer l.recoverPanic("expired callback")

	expiry.Expired(text, age)
}

// fetchHistory returns the newest stored messages of the command app, as
// GET /application/{id}/message answers them: newest first. Gotify's since=
// parameter pages towards older messages, so it is no use here; the caller
// keeps the ones above its last ID.
func (l *Listener) fetchHistory(ctx context.Context) ([]GotifyMessage, error) {
	address := l.historyURL()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}

	// The same header as the stream, so the token stays out of the URL.
	request.Header.Set("X-Gotify-Key", l.clientToken)

	response, err := l.historyClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", address, response.Status)
	}

	var page struct {
		Messages []GotifyMessage `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxHistoryBytes)).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode %s: %w", address, err)
	}

	return page.Messages, nil
}

// historyURL is the message list of the command app on the server URL.
func (l *Listener) historyURL() string {
	base := strings.TrimRight(l.serverURL, "/")

	return base + "/application/" + strconv.Itoa(l.commandAppID) + "/message?limit=" + strconv.Itoa(historyPageSize)
}
