package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// queueCapacity is how many commands may wait while one runs. A command can
// take two minutes (traffic), and nobody types sixteen commands in that time
// on purpose, so a full queue means a flood and the rest are refused.
const queueCapacity = 16

// Default timings of the connection. Gotify drops a client that does not
// answer its pings for about a minute; we use the same minute in the other
// direction, and ping at half of it so one lost pong is not fatal.
const (
	defaultPingInterval = 30 * time.Second
	defaultPongWait     = 60 * time.Second
	writeWait           = 10 * time.Second
)

// defaultStopTimeout bounds how long Stop waits for a running command. The
// commands do not all honour cancellation yet, so an unbounded wait could
// hang Gotify's disable call for the length of a command.
const defaultStopTimeout = 5 * time.Second

// Lifecycle states of a Listener. A listener is single-use: once stopped it
// does not start again, the plugin creates a new one on every Enable.
const (
	stateIdle = iota
	stateRunning
	stateStopped
)

// GotifyMessage represents an incoming Gotify push message.
type GotifyMessage struct {
	ID      int                    `json:"id"`
	AppID   int                    `json:"appid"`
	Title   string                 `json:"title"`
	Message string                 `json:"message"`
	Extras  map[string]interface{} `json:"extras"`

	// Date is when Gotify stored the message. The replay compares it with
	// the replay window; a live message is run whatever it says.
	Date time.Time `json:"date"`
}

// pendingCommand is one command waiting in the queue. id is the Gotify
// message ID, or 0 when the message carried none.
type pendingCommand struct {
	id   int
	text string
}

// MessageHandler is the callback invoked for every matching message.
type MessageHandler func(text string)

// Handler receives the commands read from the stream.
//
// Handle runs on the listener's worker goroutine, one command at a time and
// in the order they arrived. Its context is cancelled when the listener
// stops. Busy runs on the read goroutine when the queue is full and must
// return quickly; it is the place to tell the user to try again later.
//
// The listener recovers a panic in either method so the Gotify process
// survives, but it cannot answer the user: a handler that wants to report
// an internal error recovers on its own and sends the reply itself.
type Handler interface {
	Handle(ctx context.Context, text string)
	Busy(text string)
}

// ExpiryHandler is the optional part of a Handler that is told about the
// commands the replay does not run.
//
// Expired runs on the read goroutine, like Busy, for a command posted while
// the listener was not connected that is older than the replay window when
// it connects again. The command is not run; age is how old it is, or 0 when
// the message carried no date. A Handler without Expired gets nothing, and
// the listener only logs the command word.
//
// It is a separate interface so the handlers written before the replay keep
// satisfying Handler.
type ExpiryHandler interface {
	Expired(text string, age time.Duration)
}

// funcHandler adapts the original MessageHandler callback to Handler.
type funcHandler struct {
	handle MessageHandler
}

// Handle calls the wrapped function, which knows nothing about contexts.
func (handler funcHandler) Handle(_ context.Context, text string) {
	if handler.handle != nil {
		handler.handle(text)
	}
}

// Busy has nobody to tell, so the refusal is only logged by the listener.
func (handler funcHandler) Busy(string) {}

// Listener connects to a Gotify WebSocket stream and dispatches messages
// for a specific application ID to a handler.
type Listener struct {
	serverURL    string
	clientToken  string
	commandAppID int
	handler      Handler
	backoff      *Backoff
	queue        chan pendingCommand

	// checkpoint keeps the ID of the last command taken, so a listener made
	// after a Gotify restart knows which stored messages are new. It may be
	// nil: the listener then remembers it only while it lives.
	checkpoint Checkpoint

	// replayWindow is how old a pending command may be and still run on
	// connect. 0 turns the replay off.
	replayWindow time.Duration

	// historyClient reads the stored messages on connect.
	historyClient *http.Client

	// lastSeenID is the highest message ID taken from the stream or the
	// history, and baselined says whether the checkpoint was read into it.
	// Only the read loop touches them, across all its connections.
	lastSeenID int
	baselined  bool

	// checkpointMu orders the saves of the read loop (expired and refused
	// commands) and of the worker (commands about to run), and savedID is
	// the highest ID saved, so the checkpoint never moves back.
	checkpointMu sync.Mutex
	savedID      int

	// The timings are fields so tests can shrink them.
	pingInterval time.Duration
	pongWait     time.Duration
	stopTimeout  time.Duration

	// mu guards everything below. Stop runs on the caller's goroutine while
	// the read loop replaces conn on every reconnect.
	mu     sync.Mutex
	state  int
	conn   *websocket.Conn
	cancel context.CancelFunc

	// running counts the read loop and the worker, so Stop can wait for both.
	running sync.WaitGroup
}

// NewListener creates a Listener that calls handler for every command. It
// is kept for callers that only need a function; NewListenerWithHandler also
// gives the handler a context and a way to say the queue is full.
func NewListener(serverURL, clientToken string, commandAppID int, handler MessageHandler) *Listener {
	return NewListenerWithHandler(serverURL, clientToken, commandAppID, funcHandler{handle: handler})
}

// NewListenerWithHandler creates a Listener. Call Start() to begin receiving
// messages and Stop() to end it. It replays pending commands up to
// DefaultReplayWindow old and keeps its checkpoint in memory only; use
// WithCheckpoint and WithReplayWindow before Start to change that.
func NewListenerWithHandler(serverURL, clientToken string, commandAppID int, handler Handler) *Listener {
	return &Listener{
		serverURL:     serverURL,
		clientToken:   clientToken,
		commandAppID:  commandAppID,
		handler:       handler,
		backoff:       NewBackoff(time.Second, 60*time.Second).WithJitter(0.2),
		queue:         make(chan pendingCommand, queueCapacity),
		replayWindow:  DefaultReplayWindow,
		historyClient: &http.Client{Timeout: historyTimeout},
		pingInterval:  defaultPingInterval,
		pongWait:      defaultPongWait,
		stopTimeout:   defaultStopTimeout,
	}
}

// WithCheckpoint makes the listener keep the ID of the last command it took
// in checkpoint, so the commands it took before a restart are not replayed.
// Call it before Start.
func (l *Listener) WithCheckpoint(checkpoint Checkpoint) *Listener {
	l.checkpoint = checkpoint
	return l
}

// WithReplayWindow sets how old a command posted while the listener was not
// connected may be and still run when it connects; 0 turns the replay off.
// Call it before Start.
func (l *Listener) WithReplayWindow(window time.Duration) *Listener {
	l.replayWindow = window
	return l
}

// Start spawns the read loop and the worker that runs the commands. Calling
// it on a listener that is running or already stopped does nothing.
func (l *Listener) Start() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.state != stateIdle {
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	l.state = stateRunning

	l.running.Add(2)
	go l.listenLoop(ctx)
	go l.work(ctx)
}

// Stop cancels the context (which every running command receives), closes
// the WebSocket connection and waits up to stopTimeout for the read loop and
// the worker to finish. It is safe to call more than once.
func (l *Listener) Stop() {
	l.mu.Lock()
	if l.state != stateRunning {
		l.state = stateStopped
		l.mu.Unlock()
		return
	}

	// Cancelling under the lock pairs with connect: either it stored its
	// connection before this point and we close it here, or it sees the
	// cancelled context when it takes the lock and closes it itself.
	l.state = stateStopped
	l.cancel()
	conn := l.conn
	l.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}

	finished := make(chan struct{})
	go func() {
		l.running.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(l.stopTimeout):
		l.logf("stop: a command is still running after %s, leaving it to finish on its own", l.stopTimeout)
	}
}

// listenLoop keeps connecting until the context is cancelled.
func (l *Listener) listenLoop(ctx context.Context) {
	defer l.running.Done()
	defer l.recoverPanic("read loop")

	for {
		if ctx.Err() != nil {
			return
		}

		err := l.connectSafely(ctx)
		if err == nil || ctx.Err() != nil {
			continue
		}

		wait := l.backoff.Next()
		l.logf("connection error: %v — retrying in %s", err, wait.Round(time.Millisecond))

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// connectSafely runs connect and turns a panic into an ordinary connection
// error, so the loop backs off and dials again instead of ending quietly.
func (l *Listener) connectSafely(ctx context.Context) (err error) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}

		l.logf("panic in connection: %v\n%s", recovered, debug.Stack())
		err = fmt.Errorf("panic: %v", recovered)
	}()

	return l.connect(ctx)
}

// connect dials the WebSocket, resets the backoff on success, and reads
// messages until the connection drops or the context is cancelled. It only
// reads: commands go to the worker through the queue, so a long command
// never stops the pongs that keep the connection alive.
func (l *Listener) connect(ctx context.Context) error {
	url := l.wsURL()

	// Gotify accepts the client token in this header as well as in the
	// query string; in the header it stays out of proxy and access logs.
	header := http.Header{"X-Gotify-Key": []string{l.clientToken}}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, header)
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	defer conn.Close()

	if !l.publish(ctx, conn) {
		return nil
	}
	defer l.publish(ctx, nil)

	l.backoff.Reset()
	l.logf("connected to %s", url)

	// The socket is open before the history is read, so a command posted
	// in between is in the history, on the socket or in both, never in
	// neither; the IDs keep it from running twice.
	l.replay(ctx)

	// Any sign of life from the server (a pong to our ping, a ping of its
	// own, a message) moves the deadline; silence for pongWait ends the read
	// with a timeout and the loop dials again.
	l.extendDeadline(conn)
	conn.SetPongHandler(func(string) error {
		l.extendDeadline(conn)
		return nil
	})
	answerPing := conn.PingHandler()
	conn.SetPingHandler(func(appData string) error {
		l.extendDeadline(conn)
		return answerPing(appData)
	})

	// The ping loop ends with this connection, and connect does not return
	// until it has, so no goroutine outlives the connection it pings.
	pingCtx, pingCancel := context.WithCancel(ctx)
	var pinging sync.WaitGroup
	pinging.Add(1)
	go l.pingLoop(pingCtx, conn, &pinging)
	defer pinging.Wait()
	defer pingCancel()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("read: %w", err)
		}
		l.extendDeadline(conn)

		message, text, ok := l.commandText(data)
		if !ok {
			continue
		}

		// The replay may have queued this message already.
		if !l.claim(message.ID) {
			continue
		}

		l.enqueue(pendingCommand{id: message.ID, text: text})
	}
}

// publish stores conn (or clears it with nil) for Stop to close. It returns
// false when the listener is already stopping, so a connection that was
// dialled at that very moment is not used.
func (l *Listener) publish(ctx context.Context, conn *websocket.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if conn != nil && ctx.Err() != nil {
		return false
	}

	l.conn = conn
	return true
}

// extendDeadline gives the server another pongWait to show it is alive.
func (l *Listener) extendDeadline(conn *websocket.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(l.pongWait))
}

// commandText decodes one stream message and returns it with the command it
// carries, or false when the message is not a command for this plugin.
func (l *Listener) commandText(data []byte) (GotifyMessage, string, bool) {
	var msg GotifyMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		l.logf("could not parse message: %v", err)
		return msg, "", false
	}

	text, ok := l.commandOf(msg)
	return msg, text, ok
}

// commandOf returns the command msg carries, or false when it is not a
// command for this plugin: another application, a reply of the plugin
// itself or an empty message.
func (l *Listener) commandOf(msg GotifyMessage) (string, bool) {
	if msg.AppID != l.commandAppID {
		return "", false
	}

	// Skip messages sent by the plugin itself (responses, not commands)
	if msg.Extras != nil {
		if _, isResponse := msg.Extras["commander::response"]; isResponse {
			return "", false
		}
	}

	text := msg.Message
	if text == "" {
		text = msg.Title
	}

	return text, text != ""
}

// claim records id as taken and reports whether it is new. IDs only grow in
// Gotify, so anything at or below the last one taken was seen already. A
// message without an ID (0) cannot be told apart and is always new.
func (l *Listener) claim(id int) bool {
	if id == 0 {
		return true
	}

	if id <= l.lastSeenID {
		return false
	}

	l.lastSeenID = id
	return true
}

// enqueue hands pending to the worker, or tells the handler it is busy when
// the queue is full. It never blocks the read loop.
func (l *Listener) enqueue(pending pendingCommand) {
	select {
	case l.queue <- pending:
	default:
		// Only the command word goes to the log: the arguments can carry
		// coordinates or other data the user did not mean to leave a trace.
		l.logf("queue full (%d commands waiting), refusing %q", queueCapacity, commandWord(pending.text))

		// The user was told it did not run; replaying it after a restart
		// would run it behind their back.
		l.commit(pending.id)
		l.busy(pending.text)
	}
}

// busy calls the handler's Busy, surviving a panic in it.
func (l *Listener) busy(text string) {
	defer l.recoverPanic("busy callback")

	l.handler.Busy(text)
}

// work runs the queued commands one at a time until the context ends.
// One worker keeps the commands in the order the user sent them: "stop x"
// followed by "start x" must not run the other way round.
func (l *Listener) work(ctx context.Context) {
	defer l.running.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case pending := <-l.queue:
			// The ID is saved before the command runs, not after: a
			// "reboot" or "restart gotify" ends the process before Handle
			// returns, and must not run again from the replay on the way
			// back up.
			l.commit(pending.id)
			l.dispatch(ctx, pending.text)
		}
	}
}

// dispatch runs one command, surviving a panic in the handler.
func (l *Listener) dispatch(ctx context.Context, text string) {
	defer l.recoverPanic("command handler")

	l.handler.Handle(ctx, text)
}

// recoverPanic is deferred at the top of every goroutine and callback the
// package runs. A panic in a Gotify plugin would otherwise end the whole
// Gotify server; here it is logged with its stack and the goroutine goes on
// (or ends, for the read loop, which then stops reconnecting).
func (l *Listener) recoverPanic(where string) {
	recovered := recover()
	if recovered == nil {
		return
	}

	l.logf("panic in %s: %v\n%s", where, recovered, debug.Stack())
}

// pingLoop sends a WebSocket ping every pingInterval to keep the connection
// alive, and tells pinging when it is done.
func (l *Listener) pingLoop(ctx context.Context, conn *websocket.Conn, pinging *sync.WaitGroup) {
	defer pinging.Done()
	defer l.recoverPanic("ping loop")

	ticker := time.NewTicker(l.pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// WriteControl may run alongside the reads and Close, unlike
			// WriteMessage, which needs a single writer.
			deadline := time.Now().Add(writeWait)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				l.logf("ping error: %v", err)
				return
			}
		}
	}
}

// logf writes one line to the standard logger with the client token and any
// token= value removed. Every log line of the package goes through it.
func (l *Listener) logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	log.Print("[stream] " + redact(line, l.clientToken))
}

// wsURL converts the server URL's http/https scheme to ws/wss and appends
// the stream endpoint. The client token travels in a header, not here.
func (l *Listener) wsURL() string {
	u := l.serverURL
	u = strings.TrimRight(u, "/")

	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + u[len("http://"):]
	}

	return u + "/stream"
}

// commandWord returns the first word of a command line, or the whole line
// when it has no spaces.
func commandWord(text string) string {
	fields := strings.Fields(text)

	if len(fields) == 0 {
		return ""
	}

	return fields[0]
}
