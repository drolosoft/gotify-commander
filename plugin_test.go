package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	plugin "github.com/gotify/plugin-api"

	"github.com/drolosoft/gotify-commander/internal/config"
)

// TestMain silences the plugin's log lines so test output stays readable.
// The test that inspects the log installs its own writer.
func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// panelBase is the path Gotify mounts a plugin's webhook group on. The last
// segment is the plugin token, which Gotify does not authenticate.
const panelBase = "/plugin/1/custom/PLUGINTOKEN/"

// testPassword is the web_password used by tests that configure one.
const testPassword = "correct horse battery staple"

// memoryStorage is a plugin.StorageHandler that keeps the blob in memory, the
// way Gotify keeps it in its database row.
type memoryStorage struct {
	mu   sync.Mutex
	data []byte
}

// Save stores a copy of the blob.
func (s *memoryStorage) Save(b []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.data = append([]byte(nil), b...)
	return nil
}

// Load returns a copy of the stored blob.
func (s *memoryStorage) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]byte(nil), s.data...), nil
}

// failingStorage is a plugin.StorageHandler whose database is unavailable.
type failingStorage struct{}

// Save always fails.
func (failingStorage) Save([]byte) error { return errors.New("database is locked") }

// Load always fails.
func (failingStorage) Load() ([]byte, error) { return nil, errors.New("database is locked") }

// recordingMessages is a plugin.MessageHandler that records every message so a
// test can inspect what the plugin would have pushed to the user's phone.
type recordingMessages struct {
	mu       sync.Mutex
	messages []plugin.Message
	sent     chan struct{}
}

// newRecordingMessages returns an empty recorder.
func newRecordingMessages() *recordingMessages {
	return &recordingMessages{sent: make(chan struct{}, 16)}
}

// SendMessage records the message and signals the waiting test.
func (r *recordingMessages) SendMessage(msg plugin.Message) error {
	r.mu.Lock()
	r.messages = append(r.messages, msg)
	r.mu.Unlock()

	r.sent <- struct{}{}
	return nil
}

// waitForMessage blocks until one more message arrives and returns it. Enable
// sends its announcement from a goroutine, so the test has to wait for it.
func (r *recordingMessages) waitForMessage(t *testing.T) plugin.Message {
	t.Helper()

	select {
	case <-r.sent:
	case <-time.After(2 * time.Second):
		t.Fatal("no message was sent")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.messages[len(r.messages)-1]
}

// lockedBuffer is a bytes.Buffer safe to share with goroutines that log
// while the test reads it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

// Write appends to the buffer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.Write(p)
}

// String returns everything written so far.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buffer.String()
}

// testPanel bundles a plugin instance mounted on a real gin engine.
type testPanel struct {
	plugin   *Plugin
	engine   *gin.Engine
	storage  plugin.StorageHandler
	messages *recordingMessages
}

// newTestPanel builds a plugin the way Gotify does: handlers first, then the
// configuration, then the webhook group, then Enable. The configuration is set
// directly because config.Validate requires a Gotify server, and Enable would
// then start a stream listener against it.
func newTestPanel(t *testing.T, cfg *config.Config, storage plugin.StorageHandler) *testPanel {
	t.Helper()
	gin.SetMode(gin.TestMode)

	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Name: "admin"}).(*Plugin)
	messages := newRecordingMessages()
	instance.SetMessageHandler(messages)
	instance.SetStorageHandler(storage)
	instance.cfg = cfg

	engine := gin.New()
	instance.RegisterWebhook(panelBase, engine.Group(panelBase))

	if err := instance.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	t.Cleanup(func() { instance.Disable() })

	return &testPanel{plugin: instance, engine: engine, storage: storage, messages: messages}
}

// configWithPassword returns a default configuration with web_password set.
func configWithPassword(password string) *config.Config {
	cfg := config.DefaultConfig()
	cfg.WebPassword = password
	return cfg
}

// serve runs one request through the engine and returns the recorded response.
func (tp *testPanel) serve(request *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	tp.engine.ServeHTTP(recorder, request)
	return recorder
}

// get builds a GET request for a path under the panel, with an optional bearer
// password.
func get(path, password string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, panelBase+path, nil)
	if password != "" {
		request.Header.Set("Authorization", "Bearer "+password)
	}
	return request
}

// executeRequestFor builds the POST /execute request the panel's own page
// sends: JSON, same origin, with the bearer password.
func executeRequestFor(command, password string) *http.Request {
	body, _ := json.Marshal(map[string]string{"command": command})
	request := httptest.NewRequest(http.MethodPost, panelBase+"execute", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+password)
	request.Header.Set("Origin", "http://example.com")
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return request
}

// storedPassword reads the generated password back from the storage blob.
func storedPassword(t *testing.T, storage plugin.StorageHandler) string {
	t.Helper()

	blob, err := storage.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var state pluginState
	if err := json.Unmarshal(blob, &state); err != nil {
		t.Fatalf("storage blob is not JSON: %v (%q)", err, blob)
	}
	return state.PanelPassword
}

// ─── Authentication ──────────────────────────────────────────────────────────

func TestAPIRoutesRequireBearer(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	requests := map[string]*http.Request{
		"GET /config": get("config", ""),
		"GET /health": get("health", ""),
		"POST /execute": httptest.NewRequest(http.MethodPost, panelBase+"execute",
			strings.NewReader(`{"command":"ping"}`)),
	}
	for name, request := range requests {
		response := panel.serve(request)
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s without Authorization: got %d, want 401", name, response.Code)
		}
	}
}

func TestBearerPassword(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	if code := panel.serve(get("config", testPassword)).Code; code != http.StatusOK {
		t.Errorf("right Bearer on /config: got %d, want 200", code)
	}
	if code := panel.serve(get("health", testPassword)).Code; code != http.StatusOK {
		t.Errorf("right Bearer on /health: got %d, want 200", code)
	}
	if code := panel.serve(get("config", "wrong password")).Code; code != http.StatusUnauthorized {
		t.Errorf("wrong Bearer on /config: got %d, want 401", code)
	}
	if code := panel.serve(get("config", testPassword+"x")).Code; code != http.StatusUnauthorized {
		t.Errorf("Bearer with an extra character: got %d, want 401", code)
	}

	// The password in the query string ends up in proxy logs and browser
	// history, so it must not be accepted any more.
	byQuery := get("config?token="+url.QueryEscape(testPassword), "")
	if code := panel.serve(byQuery).Code; code != http.StatusUnauthorized {
		t.Errorf("?token= on /config: got %d, want 401", code)
	}

	// Basic or a bare password is not the Bearer scheme.
	basic := get("config", "")
	basic.Header.Set("Authorization", testPassword)
	if code := panel.serve(basic).Code; code != http.StatusUnauthorized {
		t.Errorf("Authorization without Bearer: got %d, want 401", code)
	}
}

func TestPageAndIconNeedNoAuth(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	if code := panel.serve(get("", "")).Code; code != http.StatusOK {
		t.Errorf("GET / without auth: got %d, want 200", code)
	}
	if code := panel.serve(get("icon", "")).Code; code != http.StatusOK {
		t.Errorf("GET /icon without auth: got %d, want 200", code)
	}
}

func TestAuthFailsClosedWithoutPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// A plugin that was never enabled has no password yet. An empty Bearer
	// must not match the empty password.
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1}).(*Plugin)
	engine := gin.New()
	instance.RegisterWebhook(panelBase, engine.Group(panelBase))

	request := get("config", "")
	request.Header.Set("Authorization", "Bearer ")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Errorf("empty Bearer before Enable: got %d, want 401", recorder.Code)
	}
}

// ─── Generated password ──────────────────────────────────────────────────────

// generatedPasswordPattern is 24 random bytes in base64url without padding.
var generatedPasswordPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32}$`)

func TestGeneratedPasswordIsStoredAndDisplayed(t *testing.T) {
	storage := &memoryStorage{}
	panel := newTestPanel(t, config.DefaultConfig(), storage)

	password := storedPassword(t, storage)
	if !generatedPasswordPattern.MatchString(password) {
		t.Fatalf("stored password %q is not 24 bytes of base64url", password)
	}

	display := panel.plugin.GetDisplay(nil)
	if !strings.Contains(display, "Control panel password") {
		t.Errorf("display has no password section:\n%s", display)
	}
	if !strings.Contains(display, password) {
		t.Errorf("display does not show the generated password:\n%s", display)
	}

	if code := panel.serve(get("config", password)).Code; code != http.StatusOK {
		t.Errorf("generated password on /config: got %d, want 200", code)
	}
}

func TestGeneratedPasswordIsReused(t *testing.T) {
	storage := &memoryStorage{}
	first := newTestPanel(t, config.DefaultConfig(), storage)
	password := storedPassword(t, storage)

	// Disable and Enable on the same instance keep it.
	first.plugin.Disable()
	if err := first.plugin.Enable(); err != nil {
		t.Fatalf("second Enable: %v", err)
	}
	if got := storedPassword(t, storage); got != password {
		t.Errorf("second Enable changed the password: %q -> %q", password, got)
	}

	// A new instance over the same storage is what a Gotify restart looks
	// like; it must read the stored password instead of making a new one.
	second := newTestPanel(t, config.DefaultConfig(), storage)
	if !strings.Contains(second.plugin.GetDisplay(nil), password) {
		t.Errorf("restarted instance does not show the stored password")
	}
	if code := second.serve(get("config", password)).Code; code != http.StatusOK {
		t.Errorf("stored password after restart: got %d, want 200", code)
	}
}

func TestConfiguredPasswordWins(t *testing.T) {
	storage := &memoryStorage{}
	panel := newTestPanel(t, configWithPassword(testPassword), storage)

	if code := panel.serve(get("config", testPassword)).Code; code != http.StatusOK {
		t.Errorf("configured password: got %d, want 200", code)
	}

	// The user typed this one, so the screen does not need to echo it back.
	if strings.Contains(panel.plugin.GetDisplay(nil), testPassword) {
		t.Errorf("display shows the configured password")
	}
}

func TestConfiguredPasswordReplacesGeneratedOne(t *testing.T) {
	storage := &memoryStorage{}
	panel := newTestPanel(t, config.DefaultConfig(), storage)
	generated := storedPassword(t, storage)

	if err := panel.plugin.ValidateAndSetConfig(configWithPasswordAndServer(testPassword)); err != nil {
		t.Fatalf("ValidateAndSetConfig: %v", err)
	}

	if code := panel.serve(get("config", generated)).Code; code != http.StatusUnauthorized {
		t.Errorf("generated password after web_password was set: got %d, want 401", code)
	}
	if code := panel.serve(get("config", testPassword)).Code; code != http.StatusOK {
		t.Errorf("new web_password: got %d, want 200", code)
	}
}

func TestClearingConfiguredPasswordKeepsPanelUsable(t *testing.T) {
	storage := &memoryStorage{}
	panel := newTestPanel(t, configWithPassword(testPassword), storage)

	// The user empties web_password while the plugin runs. Gotify does not
	// re-enable the plugin on a config change, so the password has to be
	// ready right away or the panel would lock everyone out.
	if err := panel.plugin.ValidateAndSetConfig(configWithPasswordAndServer("")); err != nil {
		t.Fatalf("ValidateAndSetConfig: %v", err)
	}

	generated := storedPassword(t, storage)
	if !generatedPasswordPattern.MatchString(generated) {
		t.Fatalf("no password was generated after web_password was cleared")
	}
	if code := panel.serve(get("config", generated)).Code; code != http.StatusOK {
		t.Errorf("generated password after clearing web_password: got %d, want 200", code)
	}
}

func TestStorageFailureStillProtectsPanel(t *testing.T) {
	panel := newTestPanel(t, config.DefaultConfig(), failingStorage{})

	display := panel.plugin.GetDisplay(nil)
	match := regexp.MustCompile("`([A-Za-z0-9_-]{32})`").FindStringSubmatch(display)
	if match == nil {
		t.Fatalf("display shows no password when storage fails:\n%s", display)
	}
	if code := panel.serve(get("config", match[1])).Code; code != http.StatusOK {
		t.Errorf("in-memory password: got %d, want 200", code)
	}
	if code := panel.serve(get("config", "")).Code; code != http.StatusUnauthorized {
		t.Errorf("no password: got %d, want 401", code)
	}
}

func TestPasswordAndPanelPathStayOutOfMessagesAndLogs(t *testing.T) {
	logs := &lockedBuffer{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(io.Discard) })

	storage := &memoryStorage{}
	panel := newTestPanel(t, config.DefaultConfig(), storage)
	password := storedPassword(t, storage)

	online := panel.messages.waitForMessage(t)
	if !strings.Contains(online.Title, "Commander Online") {
		t.Fatalf("first message is %q, want the Commander Online announcement", online.Title)
	}

	everything := online.Title + online.Message + logs.String()
	if strings.Contains(everything, password) {
		t.Errorf("the generated password leaked into a message or a log line")
	}
	if strings.Contains(everything, "PLUGINTOKEN") {
		t.Errorf("the panel path leaked into a message or a log line:\n%s\n%s", online.Message, logs.String())
	}
	if !strings.Contains(online.Message, "plugin page") {
		t.Errorf("announcement does not say where the panel is:\n%s", online.Message)
	}

	// help builds its panel link from cfg.WebURL, so it must stay empty.
	if panel.plugin.cfg.WebURL != "" {
		t.Errorf("cfg.WebURL = %q, want empty so help does not print the panel URL", panel.plugin.cfg.WebURL)
	}
}

// configWithPasswordAndServer returns a configuration that passes
// config.Validate, which ValidateAndSetConfig runs.
func configWithPasswordAndServer(password string) *config.Config {
	cfg := configWithPassword(password)
	cfg.Gotify.ServerURL = "http://localhost:1"
	cfg.Gotify.ClientToken = "client-token"
	cfg.Gotify.CommandAppID = 1
	cfg.Gotify.ResponseAppToken = "response-token"
	return cfg
}
