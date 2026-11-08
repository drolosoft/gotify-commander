package main

import (
	"bytes"
	"context"
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
	"github.com/gorilla/websocket"
	plugin "github.com/gotify/plugin-api"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/stream"
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

// waitForTitle waits for a message whose title contains want and returns it,
// failing on any message whose title contains one of refused on the way.
func (r *recordingMessages) waitForTitle(t *testing.T, want string, refused ...string) plugin.Message {
	t.Helper()

	for {
		message := r.waitForMessage(t)
		for _, title := range refused {
			if strings.Contains(message.Title, title) {
				t.Fatalf("unexpected message %q: %s", message.Title, message.Message)
			}
		}
		if strings.Contains(message.Title, want) {
			return message
		}
	}
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

	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Name: "admin", Admin: true}).(*Plugin)
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
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Admin: true}).(*Plugin)
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

// ─── POST /execute ───────────────────────────────────────────────────────────

func TestExecuteFromThePanel(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	response := panel.serve(executeRequestFor("ping", testPassword))
	if response.Code != http.StatusOK {
		t.Fatalf("ping from the panel: got %d, want 200 (%s)", response.Code, response.Body)
	}
	if !strings.Contains(response.Body.String(), "pong") {
		t.Errorf("ping answered %s, want pong", response.Body)
	}

	// Older browsers send no Sec-Fetch-Site; a matching Origin is enough.
	request := executeRequestFor("ping", testPassword)
	request.Header.Del("Sec-Fetch-Site")
	if code := panel.serve(request).Code; code != http.StatusOK {
		t.Errorf("same Origin without Sec-Fetch-Site: got %d, want 200", code)
	}

	// A charset parameter is still JSON.
	request = executeRequestFor("ping", testPassword)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	if code := panel.serve(request).Code; code != http.StatusOK {
		t.Errorf("application/json with charset: got %d, want 200", code)
	}
}

func TestExecuteRejectsWrongContentType(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	for _, contentType := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		request := executeRequestFor("ping", testPassword)
		request.Header.Set("Content-Type", contentType)
		if code := panel.serve(request).Code; code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: got %d, want 415", contentType, code)
		}
	}
}

func TestExecuteRejectsOversizedBody(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	// Valid JSON that is far too big, so the limit and not the parser is
	// what stops it.
	huge := `{"command":"` + strings.Repeat("a", 1<<20) + `"}`
	request := executeRequestFor("ping", testPassword)
	request.Body = io.NopCloser(strings.NewReader(huge))
	request.ContentLength = int64(len(huge))
	if code := panel.serve(request).Code; code != http.StatusRequestEntityTooLarge {
		t.Errorf("1 MiB JSON body: got %d, want 413", code)
	}

	// Garbage of the same size is also refused by the limit.
	request = executeRequestFor("ping", testPassword)
	request.Body = io.NopCloser(bytes.NewReader(make([]byte, 1<<20)))
	if code := panel.serve(request).Code; code != http.StatusRequestEntityTooLarge {
		t.Errorf("1 MiB of zeros: got %d, want 413", code)
	}
}

func TestExecuteRejectsCrossOrigin(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	cases := map[string]map[string]string{
		"foreign Origin": {"Origin": "https://evil.example", "Sec-Fetch-Site": ""},
		"foreign Origin, same-origin fetch metadata": {"Origin": "https://evil.example"},
		"cross-site fetch metadata":                  {"Sec-Fetch-Site": "cross-site", "Origin": ""},
		"null Origin":                                {"Origin": "null", "Sec-Fetch-Site": ""},
		"http Origin on an https request": {
			"Origin": "http://example.com", "X-Forwarded-Proto": "https", "Sec-Fetch-Site": "",
		},
	}
	for name, headers := range cases {
		request := executeRequestFor("ping", testPassword)
		for header, value := range headers {
			if value == "" {
				request.Header.Del(header)
			} else {
				request.Header.Set(header, value)
			}
		}
		if code := panel.serve(request).Code; code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", name, code)
		}
	}

	// TLS ended at the proxy without X-Forwarded-Proto: an https Origin for
	// the same host is still the panel itself.
	request := executeRequestFor("ping", testPassword)
	request.Header.Set("Origin", "https://example.com")
	if code := panel.serve(request).Code; code != http.StatusOK {
		t.Errorf("https Origin behind a TLS proxy: got %d, want 200", code)
	}
}

func TestExecuteNeedsAuthBeforeAnythingElse(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	request := executeRequestFor("ping", "wrong")
	request.Header.Set("Content-Type", "text/plain")
	if code := panel.serve(request).Code; code != http.StatusUnauthorized {
		t.Errorf("wrong password and wrong type: got %d, want 401", code)
	}
}

func TestExecuteRateLimit(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	for attempt := 1; attempt <= executeRateLimit; attempt++ {
		if code := panel.serve(executeRequestFor("ping", testPassword)).Code; code != http.StatusOK {
			t.Fatalf("execution %d of %d: got %d, want 200", attempt, executeRateLimit, code)
		}
	}

	response := panel.serve(executeRequestFor("ping", testPassword))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("execution %d: got %d, want 429", executeRateLimit+1, response.Code)
	}
	if response.Header().Get("Retry-After") == "" {
		t.Errorf("429 without Retry-After")
	}
}

func TestExecuteConcurrencyLimit(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	// Two executions in flight hold every slot.
	for range cap(panel.plugin.executeSlots) {
		panel.plugin.executeSlots <- struct{}{}
	}

	response := panel.serve(executeRequestFor("ping", testPassword))
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("third concurrent execution: got %d, want 429", response.Code)
	}
	if !strings.Contains(response.Body.String(), "busy") {
		t.Errorf("busy response says %s, want busy", response.Body)
	}

	// A busy answer must not spend the minute's budget.
	<-panel.plugin.executeSlots
	if code := panel.serve(executeRequestFor("ping", testPassword)).Code; code != http.StatusOK {
		t.Errorf("after a slot frees up: got %d, want 200", code)
	}
}

func TestRateLimiterWindow(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	limiter := newRateLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }

	if allowed, _ := limiter.Allow(); !allowed {
		t.Fatal("first call refused")
	}
	now = now.Add(20 * time.Second)
	if allowed, _ := limiter.Allow(); !allowed {
		t.Fatal("second call refused")
	}

	allowed, retryAfter := limiter.Allow()
	if allowed {
		t.Fatal("third call within the minute allowed")
	}
	if retryAfter != 40*time.Second {
		t.Errorf("retry after %s, want 40s (when the first call leaves the window)", retryAfter)
	}

	// Once the first call is a minute old, one more fits.
	now = now.Add(40 * time.Second)
	if allowed, _ := limiter.Allow(); !allowed {
		t.Error("call refused after the oldest one left the window")
	}
	if allowed, _ := limiter.Allow(); allowed {
		t.Error("window holds more calls than the limit")
	}
}

// ─── Security headers and CSP ────────────────────────────────────────────────

// noncePattern extracts the nonce from the Content-Security-Policy header.
var noncePattern = regexp.MustCompile(`script-src 'nonce-([A-Za-z0-9_-]+)'`)

func TestPageSecurityHeaders(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	response := panel.serve(get("", ""))
	if response.Code != http.StatusOK {
		t.Fatalf("GET /: got %d", response.Code)
	}

	headers := response.Header()
	policy := headers.Get("Content-Security-Policy")
	match := noncePattern.FindStringSubmatch(policy)
	if match == nil {
		t.Fatalf("CSP has no script nonce: %q", policy)
	}
	nonce := match[1]

	wantPolicy := "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'nonce-" + nonce + "'; " +
		"img-src 'self' data: https:; connect-src 'self'; font-src 'self' data:; " +
		"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
	if policy != wantPolicy {
		t.Errorf("CSP\n got: %s\nwant: %s", policy, wantPolicy)
	}

	expected := map[string]string{
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
		"X-Content-Type-Options": "nosniff",
	}
	for header, want := range expected {
		if got := headers.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// Every script and style block carries this response's nonce, and no
	// block is left without it.
	html := response.Body.String()
	scripts := strings.Count(html, "<script")
	styles := strings.Count(html, "<style")
	if scripts == 0 || styles == 0 {
		t.Fatalf("page has %d scripts and %d styles", scripts, styles)
	}
	if got := strings.Count(html, `<script nonce="`+nonce+`">`); got != scripts {
		t.Errorf("%d of %d <script> tags carry the nonce", got, scripts)
	}
	if got := strings.Count(html, `<style nonce="`+nonce+`">`); got != styles {
		t.Errorf("%d of %d <style> tags carry the nonce", got, styles)
	}
	if strings.Contains(html, "{{") {
		t.Errorf("page still has an unreplaced {{placeholder}}")
	}

	// Pico is inlined, so the page needs no CDN.
	if !strings.Contains(html, "Pico CSS") {
		t.Errorf("page does not inline Pico CSS")
	}
	for _, forbidden := range []string{"cdn.jsdelivr", "unpkg", "<link rel=\"stylesheet\"", "@import"} {
		if strings.Contains(html, forbidden) {
			t.Errorf("page references %q", forbidden)
		}
	}
}

func TestPageHasNothingTheCSPBlocks(t *testing.T) {
	// The CSP has no 'unsafe-inline', so inline event handlers and style
	// attributes would be dropped by the browser. The JS builds markup in
	// strings too, which is why the source is checked and not only the DOM.
	inlineHandler := regexp.MustCompile(`\son[a-z]+\s*=`)
	if found := inlineHandler.FindString(commandUI); found != "" {
		t.Errorf("commandUI has an inline event handler: %q", found)
	}
	if strings.Contains(commandUI, "style=") {
		t.Errorf("commandUI has a style attribute")
	}
	if strings.Contains(commandUI, "javascript:") {
		t.Errorf("commandUI has a javascript: URL")
	}
}

func TestNonceChangesPerRequest(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	first := noncePattern.FindStringSubmatch(panel.serve(get("", "")).Header().Get("Content-Security-Policy"))
	second := noncePattern.FindStringSubmatch(panel.serve(get("", "")).Header().Get("Content-Security-Policy"))
	if first == nil || second == nil || first[1] == second[1] {
		t.Errorf("nonce did not change between requests: %v %v", first, second)
	}
}

func TestSecurityHeadersOnEveryRoute(t *testing.T) {
	panel := newTestPanel(t, configWithPassword(testPassword), &memoryStorage{})

	requests := map[string]*http.Request{
		"GET /icon":      get("icon", ""),
		"GET /config":    get("config", testPassword),
		"GET /health":    get("health", testPassword),
		"401 on /config": get("config", ""),
		"POST /execute":  executeRequestFor("ping", testPassword),
		"415 on POST /execute": func() *http.Request {
			request := executeRequestFor("ping", testPassword)
			request.Header.Set("Content-Type", "text/plain")
			return request
		}(),
	}
	for name, request := range requests {
		headers := panel.serve(request).Header()
		if !strings.HasPrefix(headers.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("%s: no CSP", name)
		}
		if headers.Get("X-Frame-Options") != "DENY" || headers.Get("Cache-Control") != "no-store" ||
			headers.Get("X-Content-Type-Options") != "nosniff" || headers.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s: missing security headers: %v", name, headers)
		}
	}
}

func TestCommandUICleanups(t *testing.T) {
	if strings.Contains(strings.ToLower(commandUI), "seed history") {
		t.Errorf("commandUI still seeds a fake history")
	}
	if strings.Contains(commandUI, "user-scalable=no") {
		t.Errorf("commandUI still blocks zoom with user-scalable=no")
	}
	if strings.Contains(commandUI, "?token=") {
		t.Errorf("commandUI still sends the password in the URL")
	}
	if !strings.Contains(commandUI, `"Authorization"`) || !strings.Contains(commandUI, "gc_token") {
		t.Errorf("commandUI does not send the stored password as a Bearer header")
	}
	if !strings.Contains(commandUI, "<dialog") {
		t.Errorf("commandUI has no password dialog")
	}
}

// ─── Administrators only ─────────────────────────────────────────────────────

func TestNonAdminGetsDisabledPlugin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 2, Name: "guest", Admin: false})

	if _, ok := instance.(*Plugin); ok {
		t.Fatal("a non-admin user got the full plugin")
	}

	// Gotify only calls the optional interfaces a plugin implements. Without
	// Webhooker there is no panel, without Configurer no settings to fill.
	if _, ok := instance.(plugin.Webhooker); ok {
		t.Error("non-admin instance implements Webhooker")
	}
	if _, ok := instance.(plugin.Configurer); ok {
		t.Error("non-admin instance implements Configurer")
	}
	if _, ok := instance.(plugin.Messenger); ok {
		t.Error("non-admin instance implements Messenger")
	}

	// Mount it the way Gotify would and check that nothing is served.
	engine := gin.New()
	if webhooker, ok := instance.(plugin.Webhooker); ok {
		webhooker.RegisterWebhook(panelBase, engine.Group(panelBase))
	}
	if routes := engine.Routes(); len(routes) != 0 {
		t.Errorf("non-admin instance mounted %d routes: %v", len(routes), routes)
	}

	displayer, ok := instance.(plugin.Displayer)
	if !ok {
		t.Fatal("non-admin instance has no plugin page")
	}
	if display := displayer.GetDisplay(nil); !strings.Contains(display, "Only Gotify administrators can use gotify-commander") {
		t.Errorf("display does not say who may use the plugin:\n%s", display)
	}

	// Enabling fails with the reason, which Gotify shows next to the switch.
	err := instance.Enable()
	if err == nil || !strings.Contains(err.Error(), "administrators") {
		t.Errorf("Enable() = %v, want an error about administrators", err)
	}
	if err := instance.Disable(); err != nil {
		t.Errorf("Disable() = %v, want nil", err)
	}
}

func TestAdminGetsFullPlugin(t *testing.T) {
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Name: "admin", Admin: true})

	if _, ok := instance.(*Plugin); !ok {
		t.Fatalf("an admin got %T, want *Plugin", instance)
	}
	if _, ok := instance.(plugin.Webhooker); !ok {
		t.Error("admin instance does not implement Webhooker")
	}
}

// ─── Stream listener lifecycle ───────────────────────────────────────────────

// stoppingListener is a streamListener whose Stop runs stop, standing in for
// the in-flight command a real Stop waits for.
type stoppingListener struct {
	stop func()
}

// Start does nothing: the fake has no connection.
func (l *stoppingListener) Start() {}

// Stop runs the in-flight work and returns when it is done.
func (l *stoppingListener) Stop() { l.stop() }

func TestDisableDoesNotHoldLockWhileStopping(t *testing.T) {
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Admin: true}).(*Plugin)
	messages := newRecordingMessages()
	instance.SetMessageHandler(messages)
	instance.SetStorageHandler(&memoryStorage{})
	instance.cfg = configWithPasswordAndServer(testPassword)

	// stream.Listener.Stop waits for the running command, and the command
	// ends by sending its reply through Plugin.Send, which takes p.mu. The
	// fake does the same thing synchronously.
	instance.newListener = func(_ *config.Config, handler stream.Handler) streamListener {
		return &stoppingListener{stop: func() {
			handler.Handle(context.Background(), "ping")
		}}
	}

	if err := instance.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	messages.waitForTitle(t, "Commander Online")

	disabled := make(chan error, 1)
	go func() { disabled <- instance.Disable() }()

	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("Disable: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disable deadlocked: it holds p.mu while the listener waits for a Send")
	}

	messages.waitForTitle(t, "Ping")
}

// fakeStream is a websocket endpoint shaped like Gotify's /stream. Every
// accepted connection is handed to the test with its client token.
type fakeStream struct {
	server      *httptest.Server
	connections chan *streamConnection
}

// streamConnection is one client connected to the fake stream.
type streamConnection struct {
	token  string
	conn   *websocket.Conn
	closed chan struct{}
}

// newFakeStream starts a fake Gotify stream closed when the test ends.
func newFakeStream(t *testing.T) *fakeStream {
	t.Helper()

	fake := &fakeStream{connections: make(chan *streamConnection, 8)}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)

	return fake
}

// serve upgrades the request and reads until the client goes away. Reading
// is what makes gorilla answer the client's pings.
func (fake *fakeStream) serve(w http.ResponseWriter, r *http.Request) {
	upgrader := websocket.Upgrader{}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	connection := &streamConnection{
		token:  r.Header.Get("X-Gotify-Key"),
		conn:   conn,
		closed: make(chan struct{}),
	}
	fake.connections <- connection
	defer close(connection.closed)

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

// next waits for the next client to connect.
func (fake *fakeStream) next(t *testing.T) *streamConnection {
	t.Helper()

	select {
	case connection := <-fake.connections:
		return connection
	case <-time.After(3 * time.Second):
		t.Fatal("no client connected to the fake stream")
		return nil
	}
}

// push sends a message from appID down the connection, as Gotify does when
// a client posts to that application.
func (connection *streamConnection) push(t *testing.T, appID int, text string) {
	t.Helper()

	message := stream.GotifyMessage{ID: 1, AppID: appID, Message: text}
	if err := connection.conn.WriteJSON(message); err != nil {
		t.Fatalf("push %q: %v", text, err)
	}
}

func TestConfigChangeRestartsListener(t *testing.T) {
	fake := newFakeStream(t)

	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Admin: true}).(*Plugin)
	messages := newRecordingMessages()
	instance.SetMessageHandler(messages)
	instance.SetStorageHandler(&memoryStorage{})

	first := configWithPasswordAndServer(testPassword)
	first.Gotify.ServerURL = fake.server.URL
	first.Gotify.ClientToken = "token-one"
	first.Gotify.CommandAppID = 1
	if err := instance.ValidateAndSetConfig(first); err != nil {
		t.Fatalf("first config: %v", err)
	}
	if err := instance.Enable(); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	t.Cleanup(func() { instance.Disable() })

	old := fake.next(t)
	if old.token != "token-one" {
		t.Fatalf("first connection used token %q, want token-one", old.token)
	}

	// Gotify applies a new configuration without calling Enable again.
	second := configWithPasswordAndServer(testPassword)
	second.Gotify.ServerURL = fake.server.URL
	second.Gotify.ClientToken = "token-two"
	second.Gotify.CommandAppID = 7
	if err := instance.ValidateAndSetConfig(second); err != nil {
		t.Fatalf("second config: %v", err)
	}

	current := fake.next(t)
	if current.token != "token-two" {
		t.Fatalf("connection after the change used token %q, want token-two", current.token)
	}

	// Two listeners on the same stream would run every command twice.
	select {
	case <-old.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("the listener of the old configuration is still connected")
	}

	// A message from the old command app is ignored, one from the new app
	// runs. The worker keeps the order, so if the first one ran its reply
	// would come before the pong.
	current.push(t, 1, "services")
	current.push(t, 7, "ping")
	messages.waitForTitle(t, "Ping", "Services")
}

func TestConfigWithUnknownCommandIsRefused(t *testing.T) {
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Admin: true}).(*Plugin)

	cfg := configWithPasswordAndServer(testPassword)
	cfg.Categories = []config.Category{{Label: "Machine", Type: "machine", Commands: []string{"free", "dff"}}}

	err := instance.ValidateAndSetConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), `"dff"`) {
		t.Errorf("ValidateAndSetConfig = %v, want an error naming dff", err)
	}
	if instance.cfg != nil {
		t.Errorf("a refused configuration was applied")
	}
}

// ─── Version ─────────────────────────────────────────────────────────────────

func TestVersionComesFromBuild(t *testing.T) {
	if got := GetGotifyPluginInfo().Version; got != version {
		t.Errorf("plugin info version = %q, want %q", got, version)
	}
	if command.Version != version {
		t.Errorf("command.Version = %q, want %q (User-Agent of locate)", command.Version, version)
	}
}

// ─── Example configuration ───────────────────────────────────────────────────

func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile("config.example.yaml")
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}

	cfg, err := config.ParseYAML(data)
	if err != nil {
		t.Fatalf("parse config.example.yaml: %v", err)
	}

	// The instance is not enabled, so this only validates and stores it.
	instance := NewGotifyPluginInstance(plugin.UserContext{ID: 1, Admin: true}).(*Plugin)
	if err := instance.ValidateAndSetConfig(cfg); err != nil {
		t.Errorf("config.example.yaml is refused: %v", err)
	}

	if cfg.Gotify.ResponseAppToken != "" {
		t.Errorf("the example still fills the deprecated response_app_token")
	}
	if cfg.SSHTargets["mac"].KnownHostsFile == "" {
		t.Errorf("the example does not show known_hosts_file")
	}
}
