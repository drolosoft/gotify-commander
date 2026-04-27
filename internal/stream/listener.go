package stream

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// GotifyMessage represents an incoming Gotify push message.
type GotifyMessage struct {
	ID      int                    `json:"id"`
	AppID   int                    `json:"appid"`
	Title   string                 `json:"title"`
	Message string                 `json:"message"`
	Extras  map[string]interface{} `json:"extras"`
}

// MessageHandler is the callback invoked for every matching message.
type MessageHandler func(text string)

// Listener connects to a Gotify WebSocket stream and dispatches messages
// for a specific application ID to a handler.
type Listener struct {
	serverURL    string
	clientToken  string
	commandAppID int
	handler      MessageHandler
	backoff      *Backoff
	conn         *websocket.Conn
	cancel       context.CancelFunc
}

// NewListener creates a Listener. Call Start() to begin receiving messages.
func NewListener(serverURL, clientToken string, commandAppID int, handler MessageHandler) *Listener {
	return &Listener{
		serverURL:    serverURL,
		clientToken:  clientToken,
		commandAppID: commandAppID,
		handler:      handler,
		backoff:      NewBackoff(time.Second, 60*time.Second),
	}
}

// Start spawns the listen loop in a background goroutine.
func (l *Listener) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	l.cancel = cancel
	go l.listenLoop(ctx)
}

// Stop cancels the context and closes the WebSocket connection.
func (l *Listener) Stop() {
	if l.cancel != nil {
		l.cancel()
	}
	if l.conn != nil {
		_ = l.conn.Close()
	}
}

// listenLoop keeps connecting until the context is cancelled.
func (l *Listener) listenLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err := l.connect(ctx); err != nil {
			wait := l.backoff.Next()
			log.Printf("[stream] connection error: %v — retrying in %s", err, wait)

			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
		}
	}
}

// connect dials the WebSocket, resets the backoff on success, and reads
// messages until the connection drops or the context is cancelled.
func (l *Listener) connect(ctx context.Context) error {
	url := l.wsURL()
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.DialContext(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	defer conn.Close()

	l.conn = conn
	l.backoff.Reset()
	log.Printf("[stream] connected to %s", url)

	// Ping loop runs while we read.
	pingCtx, pingCancel := context.WithCancel(ctx)
	defer pingCancel()
	go l.pingLoop(pingCtx, conn)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}

		var msg GotifyMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Printf("[stream] could not parse message: %v", err)
			continue
		}

		if msg.AppID != l.commandAppID {
			continue
		}

		// Skip messages sent by the plugin itself (responses, not commands)
		if msg.Extras != nil {
			if _, isResponse := msg.Extras["commander::response"]; isResponse {
				continue
			}
		}

		text := msg.Message
		if text == "" {
			text = msg.Title
		}
		if text == "" {
			continue
		}

		l.handler(text)
	}
}

// pingLoop sends a WebSocket ping every 30 seconds to keep the connection alive.
func (l *Listener) pingLoop(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				log.Printf("[stream] ping error: %v", err)
				return
			}
		}
	}
}

// wsURL converts the server URL's http/https scheme to ws/wss and appends
// the stream endpoint with the client token.
func (l *Listener) wsURL() string {
	u := l.serverURL
	u = strings.TrimRight(u, "/")

	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + u[len("https://"):]
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + u[len("http://"):]
	}

	return fmt.Sprintf("%s/stream?token=%s", u, l.clientToken)
}
