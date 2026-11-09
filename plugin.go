package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	plugin "github.com/gotify/plugin-api"

	"github.com/drolosoft/gotify-commander/internal/command"
	"github.com/drolosoft/gotify-commander/internal/commander"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/ratelimit"
	"github.com/drolosoft/gotify-commander/internal/stream"
)

// version is the plugin version. The Makefile sets it from git describe with
// -ldflags "-X main.version=..."; a build without the flag says "dev".
var version = "dev"

// init hands the version to the command package, which sends it in the
// User-Agent of locate and cannot import main to read it.
func init() {
	command.Version = version
}

// GetGotifyPluginInfo returns plugin metadata used by the Gotify server for identification.
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		ModulePath:  "github.com/drolosoft/gotify-commander",
		Version:     version,
		Author:      "Drolosoft",
		Name:        "Gotify Commander",
		Website:     "https://drolosoft.com",
		Description: "Manage your Gotify server from your phone via bidirectional commands.",
		License:     "MIT",
	}
}

// Plugin is the plugin instance created per Gotify user.
// It implements Plugin, Messenger, Storager, Configurer, Webhooker, and Displayer.
type Plugin struct {
	userCtx        plugin.UserContext
	msgHandler     plugin.MessageHandler
	storageHandler plugin.StorageHandler
	cfg            *config.Config
	cmdr           *commander.Commander
	listener       streamListener
	enabled        bool
	mu             sync.Mutex
	startTime      time.Time
	webhookPath    string // full path to the control panel UI

	// relay is the handler the running listener holds. A configuration
	// saved with the same Gotify connection points it at a new commander
	// instead of reconnecting.
	relay *commandRelay

	// digest sends the daily health summary when the configuration enables
	// it. It is started with the commander and stopped with it.
	digest *command.Digest

	// panelPassword is the generated control panel password, used only
	// while web_password is empty. It is shown on the plugin page and kept
	// in the plugin storage so it survives restarts.
	panelPassword string

	// executeSlots is a semaphore for POST /execute and executeLimiter its
	// rate limit. Commands run real programs on the host, so the panel
	// must not be able to start an unbounded number of them. The limiter
	// is the panel's own: commands sent as Gotify messages are counted by
	// the commander in a separate budget.
	executeSlots   chan struct{}
	executeLimiter *ratelimit.Limiter

	// newListener builds the stream listener for a configuration. It is a
	// field so the tests can check the lock order without a Gotify server.
	newListener func(cfg *config.Config, handler stream.Handler, checkpoint stream.Checkpoint) streamListener
}

// streamListener is the part of stream.Listener the plugin uses.
type streamListener interface {
	Start()
	Stop()
}

// newGotifyListener builds the real listener on the Gotify stream of cfg,
// handing every command to handler. On every connection it replays the
// commands posted since the one in checkpoint, up to the replay window old.
func newGotifyListener(cfg *config.Config, handler stream.Handler, checkpoint stream.Checkpoint) streamListener {
	return stream.NewListenerWithHandler(cfg.Gotify.ServerURL, cfg.Gotify.ClientToken, cfg.Gotify.CommandAppID, handler).
		WithCheckpoint(checkpoint).
		WithReplayWindow(cfg.Defaults.ReplayWindow)
}

// commandRelay is the stream.Handler the plugin gives its listener. It hands
// every command to the commander of the current configuration, so saving the
// configuration in the Gotify UI swaps the commander without dropping the
// websocket.
type commandRelay struct {
	mu        sync.RWMutex
	commander *commander.Commander

	// sender answers the commands that expired before they could run; the
	// commander never sees them.
	sender commander.Sender
}

// newCommandRelay returns a relay that hands commands to cmdr and answers
// the expired ones through sender.
func newCommandRelay(cmdr *commander.Commander, sender commander.Sender) *commandRelay {
	return &commandRelay{commander: cmdr, sender: sender}
}

// set makes cmdr the commander of every command that arrives from now on. A
// command already running finishes on the commander it started on.
func (relay *commandRelay) set(cmdr *commander.Commander) {
	relay.mu.Lock()
	defer relay.mu.Unlock()

	relay.commander = cmdr
}

// current returns the commander commands go to now.
func (relay *commandRelay) current() *commander.Commander {
	relay.mu.RLock()
	defer relay.mu.RUnlock()

	return relay.commander
}

// Handle implements stream.Handler by running text on the current commander.
func (relay *commandRelay) Handle(ctx context.Context, text string) {
	relay.current().Handle(ctx, text)
}

// Busy implements stream.Handler by letting the current commander answer
// that text was refused.
func (relay *commandRelay) Busy(text string) {
	relay.current().Busy(text)
}

// expiredPriority is the priority of the answer to an expired command: the
// same as a refusal for a full queue, since in both cases nothing ran.
const expiredPriority = 5

// Expired implements stream.ExpiryHandler by telling the user that a command
// posted while the plugin was disconnected was too old to run. Only the
// command word is repeated: the arguments can carry data the user would not
// want pushed to every client again.
func (relay *commandRelay) Expired(text string, age time.Duration) {
	message := "⌛ Command expired, not executed: " + firstWord(text)
	if age > 0 {
		message += fmt.Sprintf(" (sent %s ago).", age.Round(time.Second))
	}

	relay.sender.Send("⌛ Command expired", message, expiredPriority, false)
}

// firstWord returns the first word of text, or an empty string for a blank
// one.
func firstWord(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return ""
	}

	return fields[0]
}

// sameConnection reports whether the listener of previous would read the
// same stream as one built for next: same server, token and command app,
// and the same replay window, which the listener takes when it is built.
func sameConnection(previous, next *config.Config) bool {
	if previous == nil {
		return false
	}

	return previous.Gotify.ServerURL == next.Gotify.ServerURL &&
		previous.Gotify.ClientToken == next.Gotify.ClientToken &&
		previous.Gotify.CommandAppID == next.Gotify.CommandAppID &&
		previous.Defaults.ReplayWindow == next.Defaults.ReplayWindow
}

// pluginState is what the plugin keeps in its Gotify storage blob. It is JSON
// so later fields can be added without breaking the stored password.
type pluginState struct {
	PanelPassword string `json:"panel_password"`

	// Stream is the checkpoint of the stream listener. A blob written
	// before it existed has none, which the listener takes as a first run.
	Stream *streamState `json:"stream,omitempty"`
}

// streamState is the last command the listener took from one command app of
// one Gotify server. Message IDs only mean something on the server that
// gave them, so the checkpoint is kept with the server and the app.
type streamState struct {
	ServerURL     string `json:"server_url"`
	CommandAppID  int    `json:"command_app_id"`
	LastMessageID int    `json:"last_message_id"`
}

// streamCheckpoint is the stream.Checkpoint of one listener, kept in the
// plugin storage blob next to the generated password.
type streamCheckpoint struct {
	plugin       *Plugin
	serverURL    string
	commandAppID int
}

// streamCheckpoint returns the checkpoint of a listener on the stream of
// cfg.
func (p *Plugin) streamCheckpoint(cfg *config.Config) *streamCheckpoint {
	return &streamCheckpoint{
		plugin:       p,
		serverURL:    cfg.Gotify.ServerURL,
		commandAppID: cfg.Gotify.CommandAppID,
	}
}

// Load implements stream.Checkpoint. A checkpoint saved for another server
// or command app gives 0, the first run, which replays nothing.
func (checkpoint *streamCheckpoint) Load() (int, error) {
	p := checkpoint.plugin

	// The listener calls this from its own goroutine. p.mu orders it with
	// the password, which reads and writes the same blob, and nobody holds
	// p.mu while waiting for the listener (Disable stops it outside).
	p.mu.Lock()
	defer p.mu.Unlock()

	state, err := p.readState()
	if err != nil {
		return 0, err
	}

	if state.Stream == nil || state.Stream.ServerURL != checkpoint.serverURL || state.Stream.CommandAppID != checkpoint.commandAppID {
		return 0, nil
	}
	return state.Stream.LastMessageID, nil
}

// Save implements stream.Checkpoint. The blob is read first and written
// back with the new ID; when it cannot be read nothing is written, since a
// blank state would drop the stored password.
func (checkpoint *streamCheckpoint) Save(id int) error {
	p := checkpoint.plugin

	p.mu.Lock()
	defer p.mu.Unlock()

	state, err := p.readState()
	if err != nil {
		return err
	}

	state.Stream = &streamState{
		ServerURL:     checkpoint.serverURL,
		CommandAppID:  checkpoint.commandAppID,
		LastMessageID: id,
	}
	return p.saveState(state)
}

// panelPasswordBytes is the amount of randomness in a generated password:
// 24 bytes are 192 bits, far beyond any online guessing.
const panelPasswordBytes = 24

// Limits for POST /execute. A command line is a few words, so 64 KiB is
// generous. Two executions at once and ten a minute are more than a person
// tapping buttons on a phone needs, and they stop a script from turning the
// panel into a fork bomb against the server.
const (
	maxExecuteBody       = 64 << 10
	maxConcurrentExecute = 2
	executeRateLimit     = 10
	executeRateWindow    = time.Minute
)

// NewGotifyPluginInstance is the factory function called by Gotify when loading the plugin.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
	// Gotify makes an instance for every user, and every command runs on
	// the server as the Gotify process. That is the administrator's call
	// to make, so any other user gets an instance that only says so.
	if !ctx.Admin {
		return disabledPlugin{}
	}

	return &Plugin{
		userCtx:        ctx,
		executeSlots:   make(chan struct{}, maxConcurrentExecute),
		executeLimiter: ratelimit.New(executeRateLimit, executeRateWindow),
		newListener:    newGotifyListener,
	}
}

// ─── Users who are not administrators ────────────────────────────────────────

// adminOnly is the reason a user who is not a Gotify administrator sees, on
// the plugin page and when trying to enable the plugin.
const adminOnly = "Only Gotify administrators can use gotify-commander"

// disabledPlugin is the instance of a user who is not a Gotify administrator.
// It implements only Plugin and Displayer: Gotify calls the other interfaces
// only when a plugin implements them, so this one gets no webhook routes, no
// configuration, no message handler and no stream.
type disabledPlugin struct{}

// Enable refuses with the reason, which Gotify shows to the user and which
// keeps the plugin disabled for them.
func (disabledPlugin) Enable() error {
	return errors.New("only Gotify administrators can use gotify-commander")
}

// Disable has nothing to stop.
func (disabledPlugin) Disable() error {
	return nil
}

// GetDisplay explains on the plugin page why the plugin does nothing.
func (disabledPlugin) GetDisplay(*url.URL) string {
	return "## gotify-commander\n\n" +
		"**" + adminOnly + ".**\n\n" +
		"It runs commands on this server, so only a Gotify administrator can enable it.\n"
}

// ─── commander.Sender implementation ─────────────────────────────────────────

// Send implements commander.Sender so the Plugin can be passed directly to
// commander.New as the notification sink.
func (p *Plugin) Send(title, message string, priority int, markdown bool) {
	p.mu.Lock()
	h := p.msgHandler
	p.mu.Unlock()

	if h == nil {
		return
	}
	extras := map[string]interface{}{
		"commander::response": true,
	}
	if markdown {
		extras["client::display"] = map[string]interface{}{
			"contentType": "text/markdown",
		}
	}
	h.SendMessage(plugin.Message{
		Title:    title,
		Message:  message,
		Priority: priority,
		Extras:   extras,
	})
}

// ─── Messenger interface ──────────────────────────────────────────────────────

// SetMessageHandler records the MessageHandler so the plugin can send messages.
func (p *Plugin) SetMessageHandler(h plugin.MessageHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgHandler = h
}

// ─── Storager interface ───────────────────────────────────────────────────────

// SetStorageHandler records the StorageHandler for persistent state.
func (p *Plugin) SetStorageHandler(h plugin.StorageHandler) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.storageHandler = h
}

// ─── Configurer interface ─────────────────────────────────────────────────────

// DefaultConfig returns the default configuration. Gotify will serialize this
// to display/edit in the UI, then pass the same type to ValidateAndSetConfig.
func (p *Plugin) DefaultConfig() interface{} {
	return config.DefaultConfig()
}

// ValidateAndSetConfig validates and applies user-provided configuration.
// Gotify unmarshals the YAML from the WebUI into the same *config.Config type
// that DefaultConfig returned, so we just type-assert and validate.
func (p *Plugin) ValidateAndSetConfig(conf interface{}) error {
	cfg, ok := conf.(*config.Config)
	if !ok {
		return fmt.Errorf("unexpected config type: %T", conf)
	}
	if err := config.Validate(cfg); err != nil {
		return err
	}
	if err := config.ValidateCategoryCommands(cfg, commander.CommandNames()); err != nil {
		return err
	}

	p.mu.Lock()
	previous := p.cfg
	p.cfg = cfg

	// Gotify applies a new configuration without calling Enable again. If
	// the user just emptied web_password on a running plugin, the panel
	// needs the generated password now, not after the next restart.
	if p.enabled && cfg.WebPassword == "" {
		p.ensurePanelPassword()
	}

	if !p.enabled {
		p.mu.Unlock()
		return nil
	}

	// Gotify calls this on every save in its UI, most of the time with the
	// same connection. The running listener is kept, and only the commander
	// it hands commands to is replaced. Without a listener (another save is
	// between stopping and starting one) the slow path below starts it.
	if p.listener != nil && sameConnection(previous, cfg) {
		p.replaceCommander(cfg)
		p.mu.Unlock()
		return nil
	}

	// The running listener still has the old server, token and command app.
	// It is stopped outside the lock, as in Disable, and before the new one
	// starts, so a command is never read and run by both.
	old := p.listener
	p.listener = nil
	p.relay = nil
	p.mu.Unlock()

	if old != nil {
		old.Stop()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// While the lock was free a Disable, an Enable or a newer configuration
	// may have run; each of them leaves the plugin as it should be.
	if !p.enabled || p.cfg != cfg || p.listener != nil {
		return nil
	}

	p.replaceCommander(cfg)
	p.startListener(cfg)
	return nil
}

// ─── Control panel password ──────────────────────────────────────────────────

// ensurePanelPassword makes sure a generated control panel password exists:
// the one already in memory, else the one in the plugin storage, else a new
// one that is saved there. The caller must hold p.mu.
func (p *Plugin) ensurePanelPassword() {
	if p.panelPassword != "" {
		return
	}

	state := p.loadState()
	if state.PanelPassword != "" {
		p.panelPassword = state.PanelPassword
		return
	}

	state.PanelPassword = generatePanelPassword()
	p.panelPassword = state.PanelPassword

	// A failed save is not fatal: the panel keeps working with the password
	// in memory and a new one is made on the next start. The error says
	// nothing about the password itself, so it is safe to log.
	if err := p.saveState(state); err != nil {
		log.Printf("[plugin] could not store the control panel password: %v", err)
	}
}

// loadState reads the plugin storage blob. Missing, empty or unreadable
// storage gives an empty state, which makes the caller generate a password.
// The caller must hold p.mu.
func (p *Plugin) loadState() pluginState {
	state, err := p.readState()
	if err != nil {
		log.Printf("[plugin] %v", err)
		return pluginState{}
	}
	return state
}

// readState reads the plugin storage blob. Missing or empty storage is an
// empty state; storage that cannot be read or is not JSON is an error, so a
// caller about to write the blob back can leave it alone instead. The caller
// must hold p.mu.
func (p *Plugin) readState() (pluginState, error) {
	var state pluginState
	if p.storageHandler == nil {
		return state, nil
	}

	blob, err := p.storageHandler.Load()
	if err != nil {
		return state, fmt.Errorf("could not read the plugin storage: %w", err)
	}
	if len(blob) == 0 {
		return state, nil
	}

	if err := json.Unmarshal(blob, &state); err != nil {
		return pluginState{}, fmt.Errorf("plugin storage is not valid JSON, starting over: %w", err)
	}
	return state, nil
}

// saveState writes the plugin storage blob.
func (p *Plugin) saveState(state pluginState) error {
	if p.storageHandler == nil {
		return fmt.Errorf("no storage handler")
	}

	blob, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return p.storageHandler.Save(blob)
}

// generatePanelPassword returns panelPasswordBytes random bytes as base64url
// without padding, so it can be copied from the plugin page as one word.
// crypto/rand.Read never fails since Go 1.24; it aborts the process instead.
func generatePanelPassword() string {
	random := make([]byte, panelPasswordBytes)
	rand.Read(random)
	return base64.RawURLEncoding.EncodeToString(random)
}

// effectivePassword returns the password the panel API accepts: web_password
// from the configuration when it is set, the generated one otherwise. An
// empty result means no password exists yet and nothing is accepted.
func (p *Plugin) effectivePassword() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cfg != nil && p.cfg.WebPassword != "" {
		return p.cfg.WebPassword
	}
	return p.panelPassword
}

// ─── Plugin lifecycle ─────────────────────────────────────────────────────────

// Enable starts the commander and the Gotify stream listener.
func (p *Plugin) Enable() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	cfg := p.cfg
	if cfg == nil {
		cfg = config.DefaultConfig()
		p.cfg = cfg
	}

	// cfg.WebURL is left empty on purpose. help prints it when it is set,
	// and the panel URL carries the plugin token, which Gotify does not
	// check: anyone who reads a help reply could open the panel. The link
	// lives only on the plugin page in Gotify (GetDisplay).

	if cfg.WebPassword == "" {
		p.ensurePanelPassword()
	}

	p.startTime = time.Now()
	p.startCommander(cfg)
	p.enabled = true

	// Announce online — done outside the lock via goroutine to avoid deadlock
	// if Send tries to acquire the lock (it only reads msgHandler). Messages
	// reach every client of the user, so neither the panel URL nor its
	// password goes in them.
	go p.Send("🤖 Commander Online",
		"gotify-commander is ready.\n\nSend `help` for commands.\n\nOpen the control panel from the plugin page in Gotify.",
		3, true)

	return nil
}

// Disable stops the stream listener and marks the plugin as disabled.
func (p *Plugin) Disable() error {
	p.mu.Lock()
	listener := p.listener
	digest := p.digest
	p.listener = nil
	p.relay = nil
	p.digest = nil
	p.enabled = false
	p.mu.Unlock()

	// The digest goroutine may be sending through p.Send right now, so it is
	// stopped outside the lock for the same reason as the listener.
	if digest != nil {
		digest.Stop()
	}

	// Stop waits for the command in flight, and that command ends with
	// Send, which takes p.mu. Under the lock, Stop would wait for itself
	// until its timeout and the reply would be lost.
	if listener != nil {
		listener.Stop()
	}
	return nil
}

// startCommander builds a commander for cfg with fresh stats and, when the
// Gotify connection is configured, starts a stream listener that hands it
// every command. Enable calls it, so the uptime counts from here. The caller
// must hold p.mu.
func (p *Plugin) startCommander(cfg *config.Config) {
	p.cmdr = newCommander(cfg, p)
	p.startListener(cfg)
	p.restartDigest(cfg)
}

// restartDigest stops the running digest, if any, and starts one for cfg
// when digest.enabled is set. A bad time or timezone is logged and leaves
// the plugin without a digest rather than without commands. The caller must
// hold p.mu.
func (p *Plugin) restartDigest(cfg *config.Config) {
	if p.digest != nil {
		p.digest.Stop()
		p.digest = nil
	}

	cmdr := p.cmdr

	// The summary goes through Execute so it keeps the commander's panic
	// recovery and stays outside the message rate limit.
	summary := func() command.Response {
		return cmdr.Execute("digest")
	}

	send := func(r command.Response) {
		p.Send(r.Title, r.Message, r.Priority, r.Markdown)
	}

	digest, err := command.NewDigest(cfg.Digest, summary, send)
	if err != nil {
		log.Printf("[plugin] digest not started: %v", err)
		return
	}

	digest.Start(context.Background())
	p.digest = digest
}

// replaceCommander builds a commander for cfg that carries on the stats of
// the current one, and points the running listener, if any, at it. The
// caller must hold p.mu.
func (p *Plugin) replaceCommander(cfg *config.Config) {
	cmdr := newCommander(cfg, p)

	// The plugin page and /health read these counters; a saved
	// configuration is not a restart, so they go on from where they were.
	if p.cmdr != nil {
		cmdr.Inherit(p.cmdr)
	}
	p.cmdr = cmdr
	p.restartDigest(cfg)

	if p.relay != nil {
		p.relay.set(cmdr)
	}
}

// startListener starts a stream listener on the Gotify connection of cfg
// that hands every command to p.cmdr through a relay. Without a server URL
// or a client token there is nothing to listen to. The caller must hold
// p.mu.
func (p *Plugin) startListener(cfg *config.Config) {
	if cfg.Gotify.ServerURL == "" || cfg.Gotify.ClientToken == "" {
		return
	}

	p.relay = newCommandRelay(p.cmdr, p)
	p.listener = p.newListener(cfg, p.relay, p.streamCheckpoint(cfg))
	p.listener.Start()
}

// newCommander builds the commander for cfg. The Plugin itself is the
// Sender, and the SSH executor for the mac target is wired inside
// commander.New.
func newCommander(cfg *config.Config, sender commander.Sender) *commander.Commander {
	return commander.New(cfg, executor.NewLocalExecutor(), sender)
}

// ─── Webhooker interface ──────────────────────────────────────────────────────

// executeRequest is the JSON body for POST /execute.
type executeRequest struct {
	Command string `json:"command"`
}

// checkAuth reports whether the request carries "Authorization: Bearer
// <password>" with the current panel password. The password is never read
// from the query string: URLs end up in proxy logs and browser history.
func (p *Plugin) checkAuth(r *http.Request) bool {
	want := p.effectivePassword()

	// Fail closed: before Enable there is no password, and an empty Bearer
	// must not match it.
	if want == "" {
		return false
	}

	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}

	// Comparing digests keeps the comparison constant-time and also hides
	// the password length, which ConstantTimeCompare alone would leak.
	gotDigest := sha256.Sum256([]byte(got))
	wantDigest := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(gotDigest[:], wantDigest[:]) == 1
}

// requireAuth is the gin middleware for every panel API route.
func (p *Plugin) requireAuth(c *gin.Context) {
	if !p.checkAuth(c.Request) {
		c.Header("WWW-Authenticate", `Bearer realm="gotify-commander"`)
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	c.Next()
}

// RegisterWebhook registers HTTP endpoints under the plugin's base path.
func (p *Plugin) RegisterWebhook(basePath string, g *gin.RouterGroup) {
	// The trailing slash is trimmed so links built as webhookPath + "/" do
	// not end in "//".
	p.mu.Lock()
	p.webhookPath = strings.TrimRight(g.BasePath(), "/")
	p.mu.Unlock()

	// The path is not logged: its last segment is the plugin token, and the
	// Gotify log is read by more people than the panel should be.
	log.Printf("[plugin] RegisterWebhook called")

	// Every route gets the security headers. The group is ours, so Gotify's
	// own group is left as it was handed to us.
	panel := g.Group("", securityHeaders)

	// Every route below needs the panel password except the page itself and
	// its icon, which hold no data. Grouping them makes a new route
	// protected by default.
	api := panel.Group("", p.requireAuth)

	// GET / — command UI (always served, auth happens on API calls)
	panel.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(renderPanel(c.GetString(cspNonceKey))))
	})

	// GET /icon — the panel's icon
	panel.GET("/icon", func(c *gin.Context) {
		c.Data(http.StatusOK, "image/png", panelIcon)
	})

	// GET /health — liveness + stats
	api.GET("/health", func(c *gin.Context) {
		p.mu.Lock()
		enabled := p.enabled
		cmdr := p.cmdr
		p.mu.Unlock()

		if !enabled || cmdr == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "disabled"})
			return
		}

		count, last, lastTime, uptime := cmdr.Stats()

		c.JSON(http.StatusOK, gin.H{
			"status":            "ok",
			"commands_executed": count,
			"last_command":      last,
			"last_command_time": lastTime.Format(time.RFC3339),
			"uptime":            uptime.String(),
			"version":           version,
		})
	})

	// GET /config — returns plugin configuration for dynamic UI menus
	api.GET("/config", func(c *gin.Context) {
		p.mu.Lock()
		enabled := p.enabled
		cfg := p.cfg
		p.mu.Unlock()

		if !enabled || cfg == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin not enabled"})
			return
		}

		// Collect unique machine names from the services map.
		machineSet := make(map[string]struct{})
		for _, svc := range cfg.Services {
			if svc.Machine != "" {
				machineSet[svc.Machine] = struct{}{}
			}
		}
		machines := make([]string, 0, len(machineSet))
		for m := range machineSet {
			machines = append(machines, m)
		}

		// Build services map with only the fields the UI needs.
		type serviceInfo struct {
			Description string   `json:"description"`
			Machine     string   `json:"machine"`
			Port        int      `json:"port"`
			Domain      string   `json:"domain,omitempty"`
			Aliases     []string `json:"aliases"`
		}
		services := make(map[string]serviceInfo, len(cfg.Services))
		for name, svc := range cfg.Services {
			aliases := svc.Aliases
			if aliases == nil {
				aliases = []string{}
			}
			services[name] = serviceInfo{
				Description: svc.Description,
				Machine:     svc.Machine,
				Port:        svc.Port,
				Domain:      svc.Domain,
				Aliases:     aliases,
			}
		}

		// Build categories for the UI
		type categoryInfo struct {
			Label    string   `json:"label"`
			Type     string   `json:"type"`
			Commands []string `json:"commands"`
			Danger   bool     `json:"danger,omitempty"`
		}
		categories := make([]categoryInfo, 0, len(cfg.Categories))
		for _, cat := range cfg.Categories {
			catType := cat.Type
			if catType == "" {
				catType = "direct"
			}
			categories = append(categories, categoryInfo{
				Label:    cat.Label,
				Type:     catType,
				Commands: cat.Commands,
				Danger:   cat.Danger,
			})
		}
		// Fallback if no categories configured
		if len(categories) == 0 {
			categories = []categoryInfo{
				{Label: "🖥️ Machine", Type: "machine", Commands: []string{"free", "df", "uptime", "who", "top", "ports", "ip", "connections", "updates", "ping"}},
				{Label: "🌐 Sites", Type: "service", Commands: []string{"restart", "stop", "start", "logs", "traffic"}},
				{Label: "📈 Monitoring", Type: "direct", Commands: []string{"status", "analytics", "certs", "services"}},
				{Label: "📍 Location", Type: "direct", Commands: []string{"locate"}},
				{Label: "ℹ️ Info", Type: "direct", Commands: []string{"help"}},
				{Label: "⚠️ Danger", Type: "machine", Commands: []string{"reboot", "shutdown"}, Danger: true},
			}
		}

		// The allowed containers are what "docker restart" and "docker logs"
		// accept, so the prompt can suggest them.
		containers := cfg.Docker.AllowedContainers
		if containers == nil {
			containers = []string{}
		}

		c.JSON(http.StatusOK, gin.H{
			"machines":   machines,
			"services":   services,
			"categories": categories,
			"commands":   commander.PanelCommands(cfg),
			"containers": containers,
		})
	})

	// POST /execute — run a command directly via the webhook
	api.POST("/execute", p.handleExecute)
}

// handleExecute runs one command for POST /execute. The checks go from the
// cheapest to the most expensive, and nothing runs until the request is known
// to come from the panel page with a body of the right shape and size.
func (p *Plugin) handleExecute(c *gin.Context) {
	if !sameOriginRequest(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "cross-origin request refused"})
		return
	}

	if !isJSONRequest(c.Request) {
		c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Content-Type must be application/json"})
		return
	}

	// The whole body is read under the limit before parsing, so an oversized
	// body is a 413 whatever it contains, not a parse error halfway through.
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxExecuteBody))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "could not read the request body"})
		return
	}

	var req executeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid JSON: %s", err.Error())})
		return
	}

	if req.Command == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "command is required"})
		return
	}

	p.mu.Lock()
	enabled := p.enabled
	cmdr := p.cmdr
	p.mu.Unlock()

	if !enabled || cmdr == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin not enabled"})
		return
	}

	// Take a slot before the rate limit, so a request turned away because
	// the server is busy does not spend the minute's budget.
	select {
	case p.executeSlots <- struct{}{}:
		defer func() { <-p.executeSlots }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "busy: another command is still running, try again in a moment"})
		return
	}

	allowed, retryAfter := p.executeLimiter.Allow()
	if !allowed {
		seconds := int(math.Ceil(retryAfter.Seconds()))
		c.Header("Retry-After", strconv.Itoa(seconds))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": fmt.Sprintf("too many commands, try again in %d s", seconds)})
		return
	}

	resp := cmdr.Execute(req.Command)

	// markdown tells the panel to draw the message as Markdown (the help
	// table) instead of as the plain output of a command.
	c.JSON(http.StatusOK, gin.H{
		"title":    resp.Title,
		"message":  resp.Message,
		"markdown": resp.Markdown,
	})
}

// isJSONRequest reports whether the request body is declared as JSON. Only
// JSON is accepted because a cross-site HTML form can send text/plain or a
// form encoding without a CORS preflight, but never application/json.
func isJSONRequest(r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

// sameOriginRequest reports whether a state-changing request comes from the
// panel's own page. Both browser signals are checked when present:
//
//   - Sec-Fetch-Site must be same-origin, same-site or none.
//   - Origin must name the host the request was sent to.
//
// The Origin scheme is compared only when the request is known to be HTTPS
// (TLS here, or X-Forwarded-Proto from the proxy): then an http Origin is
// refused. When TLS ends at a proxy that sends no X-Forwarded-Proto, the
// request looks like plain HTTP while the page is HTTPS, so an https Origin
// for the same host is accepted. The header could be forged, but only by a
// client that is not a browser, and such a client can forge Origin too:
// these checks stop other web pages, the Bearer password stops the rest.
func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "same-site", "none":
	default:
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}

	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" {
		return false
	}
	if !strings.EqualFold(parsed.Host, r.Host) {
		return false
	}

	switch parsed.Scheme {
	case "https":
		return true
	case "http":
		return !isHTTPSRequest(r)
	default:
		return false
	}
}

// isHTTPSRequest reports whether the client reached the server over HTTPS,
// directly or through a proxy that says so.
func isHTTPSRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ─── Security headers ────────────────────────────────────────────────────────

// cspNonceKey is the gin context key that carries the request's CSP nonce
// from securityHeaders to the page handler.
const cspNonceKey = "cspNonce"

// newCSPNonce returns 16 random bytes in base64url, a fresh nonce per
// response as CSP requires.
func newCSPNonce() string {
	random := make([]byte, 16)
	rand.Read(random)
	return base64.RawURLEncoding.EncodeToString(random)
}

// contentSecurityPolicy returns the policy for one response. The page runs
// only its own nonced script and style. Images may come from https: because
// the panel shows each service's favicon from the service's own domain, and
// from data: for the page's own icons, which are inlined. connect-src
// 'self' covers the fetches to the panel API.
func contentSecurityPolicy(nonce string) string {
	return "default-src 'none'; " +
		"script-src 'nonce-" + nonce + "'; " +
		"style-src 'nonce-" + nonce + "'; " +
		"img-src 'self' data: https:; " +
		"connect-src 'self'; " +
		"font-src 'self' data:; " +
		"base-uri 'none'; " +
		"form-action 'none'; " +
		"frame-ancestors 'none'"
}

// securityHeaders is the gin middleware for every route the plugin registers.
// The panel runs commands on the server, so it must not be framed (no
// clickjacking), cached (responses hold command output), sniffed or leak its
// URL, which carries the plugin token, through the Referer header. The one
// exception is in web/panel.js: the map tile of "locate" is asked for with
// the origin alone (scheme and host, no path), because the tile server
// refuses a browser that sends no Referer at all.
func securityHeaders(c *gin.Context) {
	nonce := newCSPNonce()
	c.Set(cspNonceKey, nonce)

	header := c.Writer.Header()
	header.Set("Content-Security-Policy", contentSecurityPolicy(nonce))
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")

	c.Next()
}

// ─── Displayer interface ──────────────────────────────────────────────────────

// GetDisplay returns Markdown shown on the plugin details page.
func (p *Plugin) GetDisplay(location *url.URL) string {
	p.mu.Lock()
	enabled := p.enabled
	cmdr := p.cmdr
	startTime := p.startTime
	webhookPath := p.webhookPath
	p.mu.Unlock()

	passwordSection := p.passwordSection()

	statusIcon := "❌"
	if enabled {
		statusIcon = "✅"
	}

	if cmdr == nil {
		return fmt.Sprintf("## gotify-commander\n\n**Status:** %s Disabled\n%s", statusIcon, passwordSection)
	}

	count, last, _, uptime := cmdr.Stats()

	lastCmd := last
	if lastCmd == "" {
		lastCmd = "—"
	}

	_ = startTime // startTime tracked but uptime comes from commander

	panelLink := ""
	if webhookPath != "" {
		panelLink = fmt.Sprintf("\n**Control Panel:** [Open](%s/)  \n", webhookPath)
	}

	return fmt.Sprintf(
		"## 🤖 Gotify Commander\n\n"+
			"**Status:** %s %s  \n"+
			"**Uptime:** %s  \n"+
			"**Commands executed:** %d  \n"+
			"**Last command:** `%s`  \n"+
			"%s\n"+
			"Send `help` to the command app to see available commands.\n"+
			"%s",
		statusIcon,
		map[bool]string{true: "Online", false: "Offline"}[enabled],
		uptime.Round(time.Second).String(),
		count,
		lastCmd,
		panelLink,
		passwordSection,
	)
}

// passwordSection returns the Markdown that tells the user which password the
// control panel asks for. This screen is the only place the generated
// password is shown: it is never logged and never sent as a message.
func (p *Plugin) passwordSection() string {
	p.mu.Lock()
	configured := p.cfg != nil && p.cfg.WebPassword != ""
	generated := p.panelPassword
	p.mu.Unlock()

	section := "\n### 🔑 Control panel password\n\n"

	// The user typed a configured password themselves, so it is not echoed.
	if configured {
		return section + "Set by `web_password` in the plugin configuration.\n"
	}
	if generated == "" {
		return section + "Enable the plugin to generate it.\n"
	}
	return section +
		"`" + generated + "`\n\n" +
		"Enter it when the control panel asks for it. It was generated for this plugin " +
		"and is kept in the plugin storage. To use your own, set `web_password` in the " +
		"plugin configuration; empty it to go back to this one.\n"
}

func main() {
	panic("gotify-commander must be built as a Go plugin: go build -buildmode=plugin")
}
