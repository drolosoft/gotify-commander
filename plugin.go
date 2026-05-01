package main

import (
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	plugin "github.com/gotify/plugin-api"

	"github.com/drolosoft/gotify-commander/internal/commander"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/stream"
)

// GetGotifyPluginInfo returns plugin metadata used by the Gotify server for identification.
func GetGotifyPluginInfo() plugin.Info {
	return plugin.Info{
		ModulePath:  "github.com/drolosoft/gotify-commander",
		Version:     "0.2.0",
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
	listener       *stream.Listener
	enabled        bool
	mu             sync.Mutex
	startTime      time.Time
	webhookPath    string // full path to the control panel UI
}

// NewGotifyPluginInstance is the factory function called by Gotify when loading the plugin.
func NewGotifyPluginInstance(ctx plugin.UserContext) plugin.Plugin {
	return &Plugin{userCtx: ctx}
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
	p.mu.Lock()
	p.cfg = cfg
	p.mu.Unlock()
	return nil
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

	// Build executors.
	localExec := executor.NewLocalExecutor()

	// Create commander (Plugin itself is the Sender).
	// SSH wiring for Mac Mini is handled automatically inside commander.New.
	cmdr := commander.New(cfg, localExec, p)

	// Set the web UI URL so help can show it
	if p.webhookPath != "" {
		if cfg.Gotify.BaseURL != "" {
			cfg.WebURL = strings.TrimRight(cfg.Gotify.BaseURL, "/") + p.webhookPath + "/"
		} else {
			cfg.WebURL = p.webhookPath + "/"
		}
	}

	p.cmdr = cmdr
	p.startTime = time.Now()

	// Create and start the stream listener if Gotify connection details are present.
	if cfg.Gotify.ServerURL != "" && cfg.Gotify.ClientToken != "" {
		p.listener = stream.NewListener(
			cfg.Gotify.ServerURL,
			cfg.Gotify.ClientToken,
			cfg.Gotify.CommandAppID,
			func(text string) {
				p.cmdr.HandleCommand(text)
			},
		)
		p.listener.Start()
	}

	p.enabled = true

	// Announce online — done outside the lock via goroutine to avoid deadlock
	// if Send tries to acquire the lock (it only reads msgHandler).
	panelURL := p.webhookPath + "/"
	go p.Send("🤖 Commander Online",
		fmt.Sprintf("gotify-commander is ready.\n\nSend `help` for commands.\n\n**Control Panel:** [Open](%s)", panelURL),
		3, true)

	return nil
}

// Disable stops the stream listener and marks the plugin as disabled.
func (p *Plugin) Disable() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.listener != nil {
		p.listener.Stop()
		p.listener = nil
	}

	p.enabled = false
	return nil
}

// ─── Webhooker interface ──────────────────────────────────────────────────────

// executeRequest is the JSON body for POST /execute.
type executeRequest struct {
	Command string `json:"command"`
}

// checkAuth validates the web_password from config. Returns true if auth passes.
func (p *Plugin) checkAuth(c *gin.Context) bool {
	p.mu.Lock()
	pwd := ""
	if p.cfg != nil {
		pwd = p.cfg.WebPassword
	}
	p.mu.Unlock()

	// No password configured = no auth required
	if pwd == "" {
		return true
	}

	// Check Authorization header: "Bearer <password>"
	auth := c.GetHeader("Authorization")
	if auth == "Bearer "+pwd {
		return true
	}

	// Check query param: ?token=<password>
	if c.Query("token") == pwd {
		return true
	}

	c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
	return false
}

// RegisterWebhook registers HTTP endpoints under the plugin's base path.
func (p *Plugin) RegisterWebhook(basePath string, g *gin.RouterGroup) {
	p.webhookPath = g.BasePath()
	log.Printf("[plugin] RegisterWebhook called: basePath=%q path=%q", basePath, p.webhookPath)

	// GET / — command UI (always served, auth happens on API calls)
	g.GET("/", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(commandUI))
	})

	// GET /icon — header icon
	g.GET("/icon", func(c *gin.Context) {
		b, _ := base64.StdEncoding.DecodeString(headerIconB64)
		c.Data(http.StatusOK, "image/png", b)
	})

	// GET /health — liveness + stats
	g.GET("/health", func(c *gin.Context) {
		if !p.checkAuth(c) { return }
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
		})
	})

	// GET /config — returns plugin configuration for dynamic UI menus
	g.GET("/config", func(c *gin.Context) {
		if !p.checkAuth(c) { return }
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

		c.JSON(http.StatusOK, gin.H{
			"machines":   machines,
			"services":   services,
			"categories": categories,
		})
	})

	// POST /execute — run a command directly via the webhook
	g.POST("/execute", func(c *gin.Context) {
		if !p.checkAuth(c) { return }
		p.mu.Lock()
		enabled := p.enabled
		cmdr := p.cmdr
		p.mu.Unlock()

		if !enabled || cmdr == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "plugin not enabled"})
			return
		}

		var req executeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("invalid JSON: %s", err.Error())})
			return
		}

		if req.Command == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "command is required"})
			return
		}

		resp := cmdr.Execute(req.Command)

		c.JSON(http.StatusOK, gin.H{
			"title":   resp.Title,
			"message": resp.Message,
		})
	})
}

// ─── Displayer interface ──────────────────────────────────────────────────────

// GetDisplay returns Markdown shown on the plugin details page.
func (p *Plugin) GetDisplay(location *url.URL) string {
	p.mu.Lock()
	enabled := p.enabled
	cmdr := p.cmdr
	startTime := p.startTime
	p.mu.Unlock()

	statusIcon := "❌"
	if enabled {
		statusIcon = "✅"
	}

	if cmdr == nil {
		return fmt.Sprintf("## gotify-commander\n\n**Status:** %s Disabled\n", statusIcon)
	}

	count, last, _, uptime := cmdr.Stats()

	lastCmd := last
	if lastCmd == "" {
		lastCmd = "—"
	}

	_ = startTime // startTime tracked but uptime comes from commander

	panelLink := ""
	if p.webhookPath != "" {
		panelLink = fmt.Sprintf("\n**Control Panel:** [Open](%s/)  \n", p.webhookPath)
	}

	return fmt.Sprintf(
		"## 🤖 Gotify Commander\n\n"+
			"**Status:** %s %s  \n"+
			"**Uptime:** %s  \n"+
			"**Commands executed:** %d  \n"+
			"**Last command:** `%s`  \n"+
			"%s\n"+
			"Send `help` to the command app to see available commands.",
		statusIcon,
		map[bool]string{true: "Online", false: "Offline"}[enabled],
		uptime.Round(time.Second).String(),
		count,
		lastCmd,
		panelLink,
	)
}

func main() {
	panic("gotify-commander must be built as a Go plugin: go build -buildmode=plugin")
}

// commandUI is the HTML page served at the plugin's webhook root.
const commandUI = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, maximum-scale=1, user-scalable=no">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<meta name="theme-color" content="#22272e">
<title>Gotify Commander</title>
<link rel="icon" href="data:image/x-icon;base64,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2/9N2P//Ttf//03U/f8+yPz/K7v8/x2x+/8Qrf3/Bqj4/wCm/f8Ap///AKf//wCm/f8Gp/j/EKz9/x2x+f8ru/v/Psj8/03T/f9O1///Tdj//0y72/9Hn7cgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA5+fnC////wRPRzcgTlpO/3qzrf9p1+//QNH8/zjR//830///N9L//zfO//8zx/3/Lsj+/y7I//8syP//LMj+/zLH/f83zv//N9L//zfT//840f//QNH8/2vX7/95s63/TlpO/09PNyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADn5+cL////BE9HNyBIQzH/w61e//7ngP/I2ab/jNPP/1nO6f89zPP/LMz6/x7M//8Sy///Dcv//w3L//8Sy///HMz//yzM+v89zPP/Wc7p/4zTz//G2qb//ud+/8OtXv9IQzH/T083IAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAOfn5wv///8ET0c3IEhFM//DrmD//+h5///hc///4XD/8uCC/87Znf+w1a//m9S9/4zTyf+J08z/idPM/4zTyf+a07//sNWv/8zZnv/y4IL//+Fw///hc///6Hn/w65g/0hFM/9PTzcgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA5+fnC////wRPRzcgSEUz/8OuYP//6Hn//uF2//7hdv//4XT//+Fy///hcP//42//+NNj/+fEW//oxFv/+NRj///jb///4XD//+Fy///hdP/+4Xb//uF2///oef/DrmD/SEUz/09PNyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADn5+cL////BE9HNyBIRTP/w65g///oef/+4Xb//uF2//7hdv/+4Xb//uF2///hcP/z5LT/9PLt//Lw5//x36T//+Fy//7hdv/+4Xb//uF2//7hdv/+4Xb//+h5/8OuYP9IRTP/T083IAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAOfn5wv///8ET0c3IEhFM//DrmD//+h5//7hdv/+4Xb//uF2//7hdv/+4HP/6cxk/9vVwf/n6/X/5uny/9rPrv/v0mP//+F0//7hdv/+4Xb//uF2//7hdv//6Hn/w65g/0hFM/9PTzcgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA5+fnC////wRPRzcgSEUz/8OuYP//6Hn//uF2//7hdf/+4HL//+Fw/+7UeP+Fj6P/gJS//4aay/+Gmsv/gpO5/4+Vof/v1nr//+Fx//7gdP/+4Xb//uF2///oef/DrmD/SEUz/09PNyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADn5+cL////BE9HNyBIRTP/w65g///oef/+4HT//uOA///ut///88n/8ue9/5akxv9acaL/QlFw/0VTc/9cc6j/lKTG//LmuP//77j//uia//7hd//+4XX//+h5/8OuYP9IRTP/T083IAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAOfn5wv///8ET0c3IEhFM//DrmD//+h3//7om///+ej///79////////////+vn0/2xrZv8VFRr/FxgY/2hrcv/u8fT///79//////////////fN//7ifv//6Hn/w65g/0hFM/9PTzcgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA5+fnC////wRNRTYhR0Mx/8OuYP//6YP///ji/////////////////9bW2P/Ly8v/0s7C/5uOW/+fkWf/39zT////////////6+zs/8LCxP/e3+H//eqq///nef/DrmD/R0Mx/09PNyAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADn5+cL////BJOGhhNkYlfKxa5b///yq////v3////////////Ly8v/Ky0w/wUGCv+oqaz//+6g///2wf////////////v7+/90dXf/AQIF/y8xNf/o3r3//+p9/8OtXv9bWUzzdnZkHAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAOfn5wv///8D/+z/DoB9dLGVhUj/9OSr/////f///////////7Gysv8PEhX/AAEC/4eJj//65aD///LF////////////+fn5/1VVWP8FBgr/AgQI/+DYu//+43r/qphV/3NyasLW1tYTAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA39/fCOvr6w2CgHWNgHVJ/Na+Zv/64pH///35////////////9vb3/4SFh/9kZWj/4N/a///pkv//66f///79////////////u7u9/1BRVP+Tlpv/+Om5//3gdv/MtmT/cWdE+4eDd5Xl//8KAAAAAAAAAAAAAAAAAAAAAAAAAAD///8CsK2oXnhtSPvRuWX/18Bp//zedP//9sr/////////////////////////////8Lz//uF2//7ifv/+9ND/////////////////////////9P//6pT/9tlx/8y3ZP/UvGf/aWFA/Y+NgnkAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACamJB/hXhN/fnddP/bw2n/0bll/9G8a///76////jg///77v//993//+2z//7heP/+4XT//uF0//7ifv//77f///fc///43v//88P//+mP/9W9Zf/gyGz/0bxm/+fNbv9uZUP/VU89xwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAMbBvDJiXETtn49S/7+qXv9yaUH/ZFw9/6iVUv/r03r/996E//7jgf/+4Xb//+N2///leP//5Xj//+N1//7hdv/84YD/+uGB/+DIbv+DdUP/YVo+/3FnQf+sm1j/gndI/1dROv9rZlW5AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA////ApqWj3deWEP9XVhA/2JcSfBtaVm/X1lF4VtVPP91akD/t6RY/7ikW//QuWX/3MNp/9zDaf/QuWX/uaZd/6GQUP+Nf0n/VlA4/1pWRPd7eGqvYF1J611XQf9dWET/Z2JO1ru7tjwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2trsDoqDeXZpZFCbpKGWWv///wO2tq04b2xbp2NfTrZhXE73WldG/05JNv9OSTb/Tkk2/09KN/9QSjf/ZV9Q4mdjU8pTSzObj4p7Z////wiuq55PaWRQm3h1ZonPz8QwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAACqqv8G6+v/DZSPgTd5dWE/UkcyR1JPOUdSTzlHUk85R1JPMkeurqcm8vLlFAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA////A7+/vwT///8E////Aqqq/wO/v/8Ev7//BL+//wS/v/8Ev7//BKqq/wP///8D1NTUBtTU1AbU1NQG1NTUBtTU1AbU1NQG1NTUBtTU1AbU1NQG1NTUBtTU1AYAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" type="image/x-icon">
<link rel="apple-touch-icon" href="data:image/png;base64,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">
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/@picocss/pico@2/css/pico.min.css">
<style>
/* ── Pico overrides: accent color ──────────────────────── */
:root {
  /* Softer dark: charcoal instead of pitch black */
  --pico-background-color: #22272e;
  --pico-card-background-color: #2d333b;
  --pico-card-sectioning-background-color: #373e47;
  --pico-muted-border-color: #444c56;
  --pico-color: #cdd9e5;
  --pico-muted-color: #768390;
  --pico-primary: #4a90d9;
  --pico-primary-hover: #3a7bc8;
  --pico-primary-focus: rgba(74, 144, 217, 0.25);
  --pico-primary-inverse: #fff;
  --pico-primary-background: #4a90d9;
  --pico-primary-background-hover: #3a7bc8;
  --pico-primary-underline: rgba(74, 144, 217, 0.5);
  --gc-accent: #4a90d9;
  --gc-accent-hover: #3a7bc8;
  --gc-danger: #ff6b7a;
  --gc-danger-bg: rgba(255, 107, 122, 0.1);
  --gc-success: #57d98f;
  --gc-chip-border: #444c56;
}

[data-theme="light"] {
  /* Softer light: warm gray instead of pure white */
  --pico-background-color: #f6f8fa;
  --pico-card-background-color: #ffffff;
  --pico-card-sectioning-background-color: #f0f2f5;
  --pico-muted-border-color: #d8dee4;
  --pico-color: #2d333b;
  --pico-muted-color: #656d76;
  --gc-accent: #2563eb;
  --gc-accent-hover: #1d4ed8;
  --gc-danger: #cf222e;
  --gc-danger-bg: rgba(207, 34, 46, 0.06);
  --gc-success: #1a7f37;
  --gc-chip-border: #d8dee4;
}

/* Light mode text color fixes */
[data-theme="light"] .action-card { color: #2d333b; }
[data-theme="light"] .action-card.danger { color: #cf222e; }
[data-theme="light"] .chip { color: #2d333b; }
[data-theme="light"] .svc-chip { color: #2d333b !important; }
[data-theme="light"] .history-toolbar a { color: #2d333b; }
[data-theme="light"] .target-btn { color: #2d333b;   min-height: 44px;
  border-radius: 12px;
}
[data-theme="light"] .theme-btn { color: #2d333b; }

[data-theme="light"] body {
  background-image: none;
}

/* Light mode: softer inset highlights (white bg doesn't need white insets) */
[data-theme="light"] .action-card,
[data-theme="light"] .svc-chip,
[data-theme="light"] .target-btn {
  box-shadow:
    0 2px 4px rgba(0,0,0,0.08),
    0 1px 2px rgba(0,0,0,0.05);
}
[data-theme="light"] .action-card:active,
[data-theme="light"] .svc-chip:active,
[data-theme="light"] .target-btn:active {
  box-shadow: inset 0 2px 4px rgba(0,0,0,0.1);
}
[data-theme="light"] header.gc-header {
  box-shadow: 0 2px 8px rgba(0,0,0,0.06);
}
[data-theme="light"] footer.gc-footer {
  box-shadow: 0 -3px 12px rgba(0,0,0,0.06);
}
[data-theme="light"] .h-entry {
  box-shadow:
    0 2px 6px rgba(0,0,0,0.05),
    0 1px 2px rgba(0,0,0,0.03);
}
[data-theme="light"] .input-bar-inner button {
  box-shadow:
    0 3px 8px rgba(37,99,235,0.25),
    0 1px 3px rgba(37,99,235,0.15);
}
[data-theme="light"] .category-tab.active {
  box-shadow:
    0 2px 6px rgba(37,99,235,0.2);
}




/* ── Category toolbar ─────────────────────────────────── */
.category-toolbar {
  display: flex;
  align-items: center;
  padding: 8px 16px;
  gap: 8px;
  max-width: 800px;
  margin: 0 auto;
  width: 100%;
  overflow-x: auto;
  -webkit-overflow-scrolling: touch;
  scrollbar-width: none;
}
.category-toolbar::-webkit-scrollbar { display: none; }
.category-tabs {
  display: flex;
  gap: 6px;
  flex-shrink: 0;
}
.category-tab {
  padding: 4px 12px;
  font-size: 12px;
  font-weight: 600;
  border-radius: 16px;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-background-color) 100%, white 6%), color-mix(in srgb, var(--pico-card-background-color) 100%, black 4%));
  border: 1px solid var(--pico-muted-border-color);
  color: var(--pico-muted-color);
  cursor: pointer;
  white-space: nowrap;
  text-decoration: none;
  box-shadow:
    0 1px 3px rgba(0,0,0,0.12),
    inset 0 1px 0 rgba(255,255,255,0.05);
  transition: box-shadow 0.15s, transform 0.1s, background 0.15s;
}
.category-tab:active {
  opacity: 0.85;
  box-shadow:
    0 0 1px rgba(0,0,0,0.1),
    inset 0 1px 3px rgba(0,0,0,0.15);
  transform: translateY(0.5px);
}

/* ── Active tab ───────────────────────────────────────── */
.category-tab.active {
  background: linear-gradient(to bottom, color-mix(in srgb, var(--gc-accent) 100%, white 12%), var(--gc-accent));
  color: #fff;
  border-color: var(--gc-accent);
  box-shadow:
    0 2px 6px rgba(74,144,217,0.3),
    inset 0 1px 0 rgba(255,255,255,0.15);
}
#categoriesContainer {
  display: flex;
  align-items: flex-start;
  overflow-x: auto;
  scroll-snap-type: x mandatory;
  -webkit-overflow-scrolling: touch;
  scrollbar-width: none;
}
#categoriesContainer::-webkit-scrollbar { display: none; }

.category-section {
  min-width: 100%;
  flex-shrink: 0;
  scroll-snap-align: start;
  background: var(--pico-card-sectioning-background-color);
  border-radius: 12px;
  padding: 12px 0;
  margin: 0 16px;
}

.category-actions {
  display: flex;
  gap: 10px;
  font-size: 12px;
  flex-shrink: 0;
  margin-left: auto;
}
.category-actions a {
  color: var(--pico-muted-color);
  text-decoration: none;
  white-space: nowrap;
}
.category-actions a:active { opacity: 0.7; }

/* ── Collapsible sections ─────────────────────────────── */
.section-toggle {
  cursor: pointer;
  user-select: none;
  -webkit-user-select: none;
}
.section-toggle:active { opacity: 0.7; }
.section-chevron {
  display: inline-block;
  transition: transform 0.2s;
  font-size: 12px;
}
.section-collapsed .section-chevron {
  transform: rotate(-90deg);
}
.section-collapsed + .card-grid,
.section-collapsed + .card-grid + .expand-panel {
  display: none;
}

/* ── Favicon for services ─────────────────────────────── */
.svc-favicon {
  width: 16px;
  height: 16px;
  border-radius: 3px;
  vertical-align: middle;
  margin-right: 4px;
  display: inline-block;
}
.svc-favicon.missing { display: none; }




/* ── Category toolbar ─────────────────────────────────── */
.category-toolbar {
  display: flex;
  align-items: center;
  padding: 8px 16px;
  gap: 8px;
  max-width: 800px;
  margin: 0 auto;
  width: 100%;
  overflow-x: auto;
  -webkit-overflow-scrolling: touch;
  scrollbar-width: none;
}
.category-toolbar::-webkit-scrollbar { display: none; }
.category-tabs {
  display: flex;
  gap: 6px;
  flex-shrink: 0;
}
.category-tab {
  padding: 4px 12px;
  font-size: 12px;
  font-weight: 600;
  border-radius: 16px;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-background-color) 100%, white 6%), color-mix(in srgb, var(--pico-card-background-color) 100%, black 4%));
  border: 1px solid var(--pico-muted-border-color);
  color: var(--pico-muted-color);
  cursor: pointer;
  white-space: nowrap;
  text-decoration: none;
  box-shadow:
    0 1px 3px rgba(0,0,0,0.12),
    inset 0 1px 0 rgba(255,255,255,0.05);
  transition: box-shadow 0.15s, transform 0.1s, background 0.15s;
}
.category-tab:active {
  opacity: 0.85;
  box-shadow:
    0 0 1px rgba(0,0,0,0.1),
    inset 0 1px 3px rgba(0,0,0,0.15);
  transform: translateY(0.5px);
}

/* ── Active tab ───────────────────────────────────────── */
.category-tab.active {
  background: linear-gradient(to bottom, color-mix(in srgb, var(--gc-accent) 100%, white 12%), var(--gc-accent));
  color: #fff;
  border-color: var(--gc-accent);
  box-shadow:
    0 2px 6px rgba(74,144,217,0.3),
    inset 0 1px 0 rgba(255,255,255,0.15);
}
#categoriesContainer {
  display: flex;
  align-items: flex-start;
  overflow-x: auto;
  scroll-snap-type: x mandatory;
  -webkit-overflow-scrolling: touch;
  scrollbar-width: none;
}
#categoriesContainer::-webkit-scrollbar { display: none; }

.category-section {
  min-width: 100%;
  flex-shrink: 0;
  scroll-snap-align: start;
  background: var(--pico-card-sectioning-background-color);
  border-radius: 12px;
  padding: 12px 0;
  margin: 0 16px;
}

.category-actions {
  display: flex;
  gap: 10px;
  font-size: 12px;
  flex-shrink: 0;
  margin-left: auto;
}
.category-actions a {
  color: var(--pico-muted-color);
  text-decoration: none;
  white-space: nowrap;
}
.category-actions a:active { opacity: 0.7; }

/* ── Collapsible sections ─────────────────────────────── */
.section-toggle {
  cursor: pointer;
  user-select: none;
  -webkit-user-select: none;
}
.section-toggle:active { opacity: 0.7; }
.section-chevron {
  display: inline-block;
  transition: transform 0.2s;
  font-size: 12px;
}
.section-collapsed .section-chevron {
  transform: rotate(-90deg);
}
.section-collapsed + .card-grid,
.section-collapsed + .card-grid + .expand-panel {
  display: none;
}

/* ── Favicon for services ─────────────────────────────── */
.svc-favicon {
  width: 16px;
  height: 16px;
  border-radius: 3px;
  vertical-align: middle;
  margin-right: 4px;
}

html { height: 100%; height: -webkit-fill-available; scroll-behavior: smooth; }

body {
  min-height: 100%;
  min-height: 100dvh;
  min-height: -webkit-fill-available;
  display: flex;
  flex-direction: column;
  padding: 0;
  margin: 0;
  -webkit-tap-highlight-color: transparent;
  -webkit-overflow-scrolling: touch;
  overflow-x: hidden;
}

/* ── Desktop max-width constraint ────────────────────── */

.gc-container {
  max-width: 800px;
  margin: 0 auto;
  width: 100%;
}

main.gc-container {
  padding: 8px 0 !important;
}

/* ── Header ──────────────────────────────────────────── */

header.gc-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 16px;
  background: var(--pico-card-background-color);
  border-bottom: none;
  box-shadow: 0 2px 8px rgba(0,0,0,0.12);
  position: sticky;
  top: 0;
  z-index: 100;
  margin: 0;
}

.gc-header-inner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
}

header.gc-header h1 {
  font-size: 18px;
  font-weight: 700;
  letter-spacing: -0.3px;
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 6px;
  margin: 0;
  padding: 0;
  color: var(--pico-color);
  flex: 1;
  text-align: center;
  overflow: visible;
  line-height: 1;
}
header.gc-header h1 img {
  display: block;
  flex-shrink: 0;
}

header.gc-header h1 .icon { font-size: 22px; }

/* ── Back to Gotify link ──────────────────────────────── */

.gc-back {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  border-radius: 10px;
  color: var(--pico-color);
  text-decoration: none;
  flex-shrink: 0;
  -webkit-tap-highlight-color: transparent;
}

.gc-back:hover {
  background: var(--pico-card-sectioning-background-color);
}

.gc-back:active { opacity: 0.7; }

.theme-btn {
  width: 40px;
  height: 40px;
  border-radius: 10px;
  border: 1px solid var(--pico-muted-border-color);
  border-bottom-width: 2px;
  border-bottom-color: color-mix(in srgb, var(--pico-muted-border-color) 100%, black 25%);
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-sectioning-background-color) 100%, white 6%), var(--pico-card-sectioning-background-color));
  color: var(--pico-color);
  font-size: 18px;
  cursor: pointer;
  display: flex;
  align-items: center;
  justify-content: center;
  -webkit-appearance: none;
  padding: 0;
  margin: 0;
  line-height: 1;
  box-shadow:
    0 1px 3px rgba(0,0,0,0.12),
    inset 0 1px 0 rgba(255,255,255,0.05);
  transition: box-shadow 0.15s, transform 0.1s;
}

.theme-btn:active {
  opacity: 0.85;
  box-shadow: inset 0 1px 3px rgba(0,0,0,0.15);
  border-bottom-width: 1px;
  transform: translateY(0.5px);
}

/* ── Command Input Bar ─────────────────────────────────── */

.input-bar {
  padding: 10px 16px;
  background: var(--pico-card-background-color);
  border-bottom: none;
  box-shadow: 0 2px 6px rgba(0,0,0,0.08);
  display: flex;
  gap: 8px;
  position: sticky;
  top: 57px;
  z-index: 99;
}

.input-bar-inner {
  display: flex;
  gap: 8px;
  max-width: 800px;
  margin: 0 auto;
  width: 100%;
}

.input-bar-inner input[type="text"] {
  flex: 1;
  margin: 0;
  height: auto;
  padding: 12px 14px;
  font-size: 17px;
}

.input-bar-inner button {
  margin: 0;
  padding: 12px 18px;
  font-size: 16px;
  font-weight: 700;
  min-width: 64px;
  width: auto;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--gc-accent) 100%, white 15%), var(--gc-accent), color-mix(in srgb, var(--gc-accent) 100%, black 10%));
  border: 1.5px solid color-mix(in srgb, var(--gc-accent) 100%, black 15%);
  border-bottom-width: 3px;
  border-bottom-color: color-mix(in srgb, var(--gc-accent) 100%, black 30%);
  color: #fff;
  border-radius: 12px;
  box-shadow:
    0 3px 8px rgba(74,144,217,0.35),
    0 1px 3px rgba(74,144,217,0.2),
    inset 0 1px 0 rgba(255,255,255,0.2);
  transition: box-shadow 0.15s, transform 0.1s;
  text-shadow: 0 1px 2px rgba(0,0,0,0.2);
}

.input-bar-inner button:active {
  box-shadow:
    0 0 4px rgba(74,144,217,0.2),
    inset 0 2px 4px rgba(0,0,0,0.2);
  border-bottom-width: 1.5px;
  transform: translateY(1.5px);
}

/* ── Categories container ────────────────────────────── */
#categoriesContainer {
  padding-bottom: 8px;
  overflow: hidden;
  transition: height 0.2s ease;
}

/* ── Section Labels ────────────────────────────────────── */

.section-label {
  padding: 10px 16px 6px;
  font-size: 13px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--pico-muted-color);
  margin: 0;
  border-left: 3px solid var(--gc-accent);
  padding-left: 10px;
}

/* ── Card Grid ─────────────────────────────────────────── */

.card-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(95px, 1fr));
  gap: 8px;
  margin: 0 0 6px;
  padding: 8px;
  align-content: start;
  background: rgba(0, 0, 0, 0.08);
  border-radius: 10px;
  box-shadow: inset 0 1px 3px rgba(0, 0, 0, 0.15);
}

[data-theme="light"] .card-grid {
  background: rgba(0, 0, 0, 0.03);
  box-shadow: inset 0 1px 3px rgba(0, 0, 0, 0.06);
}

[data-theme="light"] .category-section {
  background: #f0f2f5;
}

.action-card {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 12px 8px;
  font-size: 14px;
  font-weight: 600;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-background-color) 100%, white 8%), var(--pico-card-background-color));
  color: var(--pico-color);
  border: 1.5px solid var(--pico-muted-border-color);
  border-bottom-width: 3px;
  border-bottom-color: color-mix(in srgb, var(--pico-muted-border-color) 100%, black 30%);
  border-radius: 12px;
  cursor: pointer;
  -webkit-appearance: none;
  transition: border-color 0.15s, background 0.15s, box-shadow 0.15s, transform 0.1s;
  min-height: 44px;
  text-align: center;
  position: relative;
  margin: 0;
  width: auto;
  box-shadow:
    0 2px 4px rgba(0,0,0,0.15),
    0 1px 2px rgba(0,0,0,0.1),
    inset 0 1px 0 rgba(255,255,255,0.06);
}

.action-card:active {
  background: var(--pico-card-sectioning-background-color);
  box-shadow:
    0 0 2px rgba(0,0,0,0.1),
    inset 0 2px 4px rgba(0,0,0,0.15);
  border-bottom-width: 1.5px;
  transform: translateY(1px);
}
.action-card.active {
  border-color: var(--gc-accent);
  background: var(--pico-card-sectioning-background-color);
  box-shadow:
    0 0 2px rgba(0,0,0,0.1),
    0 0 8px rgba(74,144,217,0.15),
    inset 0 2px 4px rgba(0,0,0,0.1);
  border-bottom-width: 1.5px;
}

.action-card.danger { color: var(--gc-danger); border-color: color-mix(in srgb, var(--gc-danger) 40%, var(--pico-muted-border-color)); }
.action-card.danger.active {
  border-color: var(--gc-danger);
  background: var(--gc-danger-bg);
  box-shadow:
    0 0 2px rgba(0,0,0,0.1),
    0 0 8px rgba(255,107,122,0.15),
    inset 0 2px 4px rgba(0,0,0,0.1);
}

.action-card.running {
  animation: pulse-card 1.2s ease-in-out infinite;
}

@keyframes pulse-card {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.55; }
}

/* ── Expanded Panel ──────────────────────────────────── */

.expand-panel {
  display: none;
  padding: 10px 16px 14px;
  animation: slide-down 0.15s ease-out;
}

.expand-panel.visible { display: block; }

@keyframes slide-down {
  from { opacity: 0; transform: translateY(-6px); }
  to { opacity: 1; transform: translateY(0); }
}

.target-row {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}

.target-btn {
  flex: 1;
  min-width: 100px;
  padding: 12px 16px;
  font-size: 15px;
  font-weight: 700;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-background-color) 100%, white 8%), var(--pico-card-background-color));
  color: var(--pico-color);
  border: 1.5px solid var(--pico-muted-border-color);
  border-bottom-width: 3px;
  border-bottom-color: color-mix(in srgb, var(--pico-muted-border-color) 100%, black 30%);
  border-radius: 10px;
  cursor: pointer;
  -webkit-appearance: none;
  text-align: center;
  min-height: 44px;
  transition: border-color 0.15s, background 0.15s, box-shadow 0.15s, transform 0.1s;
  margin: 0;
  width: auto;
  box-shadow:
    0 2px 4px rgba(0,0,0,0.15),
    0 1px 2px rgba(0,0,0,0.1),
    inset 0 1px 0 rgba(255,255,255,0.06);
}

.target-btn:active {
  border-color: var(--gc-accent);
  background: var(--pico-card-sectioning-background-color);
  box-shadow:
    0 0 2px rgba(0,0,0,0.1),
    inset 0 2px 4px rgba(0,0,0,0.15);
  border-bottom-width: 1.5px;
  transform: translateY(1px);
}

.target-btn.danger { border-color: var(--gc-danger); color: var(--gc-danger); }
.target-btn.danger:active { background: var(--gc-danger-bg); }

.target-btn.running {
  animation: pulse-card 1.2s ease-in-out infinite;
}

/* ── Service Chips ───────────────────────────────────── */

.machine-group { margin-bottom: 10px; }
.machine-group:last-child { margin-bottom: 0; }

.machine-label {
  font-size: 12px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--pico-muted-color);
  padding: 4px 0 8px;
  display: flex;
  align-items: center;
  gap: 6px;
}

.machine-label::before,
.machine-label::after {
  content: "";
  flex: 1;
  height: 1px;
  background: var(--pico-muted-border-color);
}

.chip-wrap {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(95px, 1fr));
  gap: 8px;
  padding: 4px 0;
}

.svc-chip {
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 12px 8px;
  font-size: 14px;
  font-weight: 600;
  background: linear-gradient(to bottom, color-mix(in srgb, var(--pico-card-background-color) 100%, white 8%), var(--pico-card-background-color));
  color: var(--pico-color);
  border: 1.5px solid var(--pico-muted-border-color);
  border-bottom-width: 3px;
  border-bottom-color: color-mix(in srgb, var(--pico-muted-border-color) 100%, black 30%);
  border-radius: 12px;
  cursor: pointer;
  -webkit-appearance: none;
  min-height: 44px;
  text-align: center;
  margin: 0;
  width: auto;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  box-shadow:
    0 2px 4px rgba(0,0,0,0.15),
    0 1px 2px rgba(0,0,0,0.1),
    inset 0 1px 0 rgba(255,255,255,0.06);
  transition: border-color 0.15s, box-shadow 0.15s, transform 0.1s;
}

.svc-chip:active {
  border-color: var(--gc-accent);
  box-shadow:
    0 0 2px rgba(0,0,0,0.1),
    inset 0 2px 4px rgba(0,0,0,0.15);
  border-bottom-width: 1.5px;
  transform: translateY(1px);
}

.svc-chip .port {
  font-size: 11px;
  font-weight: 500;
  color: var(--pico-muted-color);
}

.svc-chip.running {
  animation: pulse-card 1.2s ease-in-out infinite;
}

/* ── Command History ─────────────────────────────────── */

section.gc-history {
  padding-bottom: 50px;
  flex: 1;
  padding: 0 16px 120px;
}

.history-header {
  display: flex;
  align-items: center;
  padding: 16px 0 6px;
  gap: 8px;
  margin-top: 12px;
  border-top: 1px solid var(--pico-muted-border-color);
  padding-top: 16px;
}

.history-header h3 {
  font-size: 13px;
  font-weight: 700;
  text-transform: uppercase;
  letter-spacing: 0.5px;
  color: var(--pico-muted-color);
  margin: 0;
  padding: 2px 0 2px 10px;
  border-left: 3px solid var(--gc-accent);
}

.history-toolbar {
  display: flex;
  align-items: center;
  gap: 6px;
  margin-left: auto;
}

.history-toolbar a {
  margin: 0;
  padding: 4px 0;
  font-size: 12px;
  font-weight: 500;
  cursor: pointer;
  color: var(--pico-muted-color);
  text-decoration: none;
  line-height: 1;
  min-height: 44px;
  display: flex;
  align-items: center;
}

.history-toolbar a:hover { text-decoration: underline; }
.history-toolbar a:active { opacity: 0.5; }

.history-toolbar .clear-btn {
  color: var(--gc-danger);
  font-weight: 600;
  margin-left: 8px;
}

.history-list {
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.history-list:empty::before {
  content: "No commands yet";
  display: block;
  text-align: center;
  padding: 40px 0;
  color: var(--pico-muted-color);
  font-size: 14px;
}


/* ── Trash button on history entries ──────────────────── */
.h-delete {
  position: absolute;
  top: 6px;
  right: 6px;
  width: 28px;
  height: 28px;
  border: none;
  background: transparent;
  color: var(--pico-muted-color);
  font-size: 14px;
  cursor: pointer;
  border-radius: 6px;
  display: flex;
  align-items: center;
  justify-content: center;
  opacity: 0.5;
  padding: 0;
  margin: 0;
  line-height: 1;
}
.h-delete:hover, .h-delete:active { opacity: 1; color: var(--gc-danger); }
.h-entry { position: relative; }

.h-entry {
  border-radius: 10px;
  overflow: hidden;
  border: 1px solid var(--pico-muted-border-color);
  margin: 0;
  padding: 0;
  background: transparent;
  box-shadow:
    0 2px 6px rgba(0,0,0,0.1),
    0 1px 2px rgba(0,0,0,0.06);
}

.h-cmd {
  padding: 8px 12px;
  font-size: 13px;
  font-family: "SF Mono", SFMono-Regular, Menlo, Monaco, monospace;
  font-weight: 600;
  background: var(--pico-card-sectioning-background-color);
  color: var(--gc-accent);
  display: flex;
  align-items: center;
  gap: 6px;
  cursor: pointer;
  user-select: none;
}

.h-cmd:active { opacity: 0.7; }

.h-cmd-chevron {
  font-size: 11px;
  color: var(--pico-muted-color);
  transition: transform 0.15s;
}

.h-entry.collapsed .h-cmd-chevron {
  transform: rotate(0deg);
}

.h-entry:not(.collapsed) .h-cmd-chevron {
  transform: rotate(90deg);
}

.h-cmd-prefix {
  color: var(--pico-muted-color);
  font-weight: 400;
  user-select: none;
}

.h-cmd-time {
  margin-left: auto;
  font-size: 11px;
  font-weight: 400;
  color: var(--pico-muted-color);
  font-family: inherit;
}

.h-resp {
  font-family: "SF Mono", Monaco, "Cascadia Code", "Courier New", monospace;
  font-size: 13px;
  padding: 10px 12px;
  font-size: 13px;
  line-height: 1.5;
  background: var(--pico-card-background-color);
  white-space: pre-wrap;
  word-break: break-word;
}

.h-entry.collapsed .h-resp {
  display: none;
}

.h-resp-title {
  font-weight: 700;
  font-size: 14px;
  margin-bottom: 3px;
}

.h-entry.error .h-resp { color: var(--gc-danger); }
.h-entry.error .h-cmd { color: var(--gc-danger); }

.h-entry.loading .h-resp {
  color: var(--pico-muted-color);
  font-style: italic;
}

/* ── Back to Top Button ──────────────────────────────── */

.back-to-top {
  position: fixed;
  bottom: 60px;
  right: 16px;
  width: 44px;
  height: 44px;
  border-radius: 50%;
  border: 1px solid var(--pico-muted-border-color);
  background: var(--pico-card-background-color);
  color: var(--pico-color);
  font-size: 18px;
  cursor: pointer;
  -webkit-appearance: none;
  display: flex;
  align-items: center;
  justify-content: center;
  z-index: 99;
  opacity: 0;
  pointer-events: none;
  transition: opacity 0.2s;
  box-shadow:
    0 3px 10px rgba(0,0,0,0.2),
    0 1px 3px rgba(0,0,0,0.1),
    inset 0 1px 0 rgba(255,255,255,0.08);
  border-bottom-width: 2px;
  border-bottom-color: color-mix(in srgb, var(--pico-muted-border-color) 100%, black 25%);
  margin: 0;
  padding: 0;
  line-height: 1;
}

.back-to-top.visible {
  opacity: 0.85;
  pointer-events: auto;
}

[data-theme="light"] .back-to-top {
  background: #ffffff;
  border-color: #c8ccd0;
  color: #2d333b;
  box-shadow: 0 3px 10px rgba(0,0,0,0.12), 0 1px 3px rgba(0,0,0,0.06);
}

.back-to-top:active {
  opacity: 1;
  box-shadow: inset 0 2px 4px rgba(0,0,0,0.15);
  transform: translateY(1px);
}

@supports(padding: env(safe-area-inset-bottom)) {
  .back-to-top { bottom: calc(60px + env(safe-area-inset-bottom)); }
}

/* ── Footer ──────────────────────────────────────────── */

footer.gc-footer {
  position: fixed;
  bottom: 0;
  left: 0;
  right: 0;
  display: flex;
  align-items: center;
  justify-content: center;
  padding: 10px 16px;
  padding-bottom: calc(10px + env(safe-area-inset-bottom, 0px));
  font-size: 12px;
  font-weight: 600;
  background: var(--pico-card-background-color);
  border-top: none;
  box-shadow: 0 -3px 12px rgba(0,0,0,0.15);
  color: var(--pico-muted-color);
  z-index: 100;
  margin: 0;
}

.gc-footer-inner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  width: 100%;
}

.status-dot {
  display: inline-block;
  width: 8px;
  height: 8px;
  border-radius: 50%;
  margin-right: 6px;
  background: var(--gc-success);
}

.status-dot.offline { background: var(--gc-danger); }

.footer-left, .footer-right {
  display: flex;
  align-items: center;
}

/* ── Scrollbar styling ─────────────────────────────────── */
::-webkit-scrollbar { width: 4px; }
::-webkit-scrollbar-track { background: transparent; }
::-webkit-scrollbar-thumb { background: var(--pico-muted-border-color); border-radius: 4px; }

/* ── Safe area insets for modern iPhones ───────────────── */
@supports(padding: env(safe-area-inset-bottom)) {
  footer.gc-footer { padding-bottom: calc(10px + env(safe-area-inset-bottom)); }
  section.gc-history {
  padding-bottom: 50px; padding-bottom: calc(120px + env(safe-area-inset-bottom)); }
}
</style>
</head>
<body>
<style>


</style>




<!-- Header -->
<header class="gc-header">
  <div class="gc-header-inner">
    <a href="/" class="gc-back" title="Back to Gotify" aria-label="Back to Gotify">
        <svg width="20" height="20" viewBox="0 0 20 20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">
          <polyline points="12,16 6,10 12,4"></polyline>
        </svg>
      </a>
      <h1><img src="assets/header-icon.png"$ICON_B64""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"" alt="" style="width:auto;height:40px;vertical-align:middle;"> Gotify Commander</h1>
    <button class="theme-btn" id="themeToggle" aria-label="Toggle theme">&#x25D0;</button>
  </div>
</header>

<!-- Command Input -->
<div class="input-bar" role="group">
  <div class="input-bar-inner">
    <input id="cmdInput" type="text" placeholder="status"
      autocapitalize="none" autocorrect="off" spellcheck="false" autofocus>
    <button id="runBtn">Run</button>
  </div>
</div>

<!-- Quick Actions: dynamic categories -->
<main class="gc-container" style="padding:0;">

  <div class="category-toolbar" id="categoryToolbar">
    <div class="category-tabs" id="categoryTabs"></div>
    
  </div>
  <div id="categoriesContainer"></div>

  <!-- History -->
  <section class="gc-history">
    <div class="history-header">
      <h3>&#x1F4CB; History</h3>
      <div class="history-toolbar">
        <a href="#" id="collapseAllBtn" title="Collapse all">⊟ Collapse</a>
        <a href="#" id="expandAllBtn" title="Expand all">⊞ Expand</a>
        <a href="#" id="clearHistoryBtn" class="clear-btn" title="Clear history">✕ Clear</a>
      </div>
    </div>
    <div class="history-list" id="historyList"></div>
  </section>

</main>

<!-- Back to Top -->
<button class="back-to-top" id="backToTop" aria-label="Back to top">&#x2191;</button>

<!-- Footer -->
<footer class="gc-footer">
  <div class="gc-footer-inner">
    <div class="footer-left">
      <span class="status-dot" id="statusDot"></span>
      <span id="statusText">Connecting...</span>
    </div>
    <div class="footer-right">
      <span id="cmdCount">--</span>
    </div>
  </div>
</footer>

<script>
/* ── Base Path ───────────────────────────────────────── */
var basePath = window.location.pathname.replace(/\/+$/, "");

/* ── DOM refs ────────────────────────────────────────── */
var cmdInput = document.getElementById("cmdInput");
var runBtn = document.getElementById("runBtn");
var categoriesContainer = document.getElementById("categoriesContainer");
var historyList = document.getElementById("historyList");
var statusDot = document.getElementById("statusDot");
var statusText = document.getElementById("statusText");
var cmdCount = document.getElementById("cmdCount");
var themeToggle = document.getElementById("themeToggle");
var collapseAllBtn = document.getElementById("collapseAllBtn");
var expandAllBtn = document.getElementById("expandAllBtn");
var clearHistoryBtn = document.getElementById("clearHistoryBtn");
var backToTop = document.getElementById("backToTop");

/* ── State ───────────────────────────────────────────── */
var configData = null;
var expandedCard = null;
var expandedPanel = null;
var runningElements = [];
var HISTORY_KEY = "gc_history";
var THEME_KEY = "gc_theme";
var MAX_HISTORY = 20;

/* ── Theme System (Pico data-theme) ──────────────────── */
var THEMES = ["dark", "light", "system"];
var THEME_ICONS = { dark: "\uD83C\uDF15", light: "\uD83C\uDF11", system: "\u25D0" };

function getStoredTheme() {
  try { return localStorage.getItem(THEME_KEY) || "dark"; } catch(e) { return "dark"; }
}

function setTheme(mode) {
  try { localStorage.setItem(THEME_KEY, mode); } catch(e) {}
  applyTheme(mode);
  themeToggle.textContent = THEME_ICONS[mode] || THEME_ICONS.system;
}

function applyTheme(mode) {
  var root = document.documentElement;
  if (mode === "system") {
    var prefersDark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    root.setAttribute("data-theme", prefersDark ? "dark" : "light");
  } else {
    root.setAttribute("data-theme", mode);
  }
}

function cycleTheme() {
  var current = getStoredTheme();
  var idx = THEMES.indexOf(current);
  var next = THEMES[(idx + 1) % THEMES.length];
  setTheme(next);
}

/* Init theme */
(function() {
  var stored = getStoredTheme();
  setTheme(stored);
  /* Listen for OS theme changes when in system mode */
  try {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", function() {
      if (getStoredTheme() === "system") applyTheme("system");
    });
  } catch(e) {}
})();

themeToggle.addEventListener("click", cycleTheme);

/* ── HTML Escaping ───────────────────────────────────── */


function collapseAllSections() {
  var labels = document.querySelectorAll(".section-toggle");
  for (var i = 0; i < labels.length; i++) {
    labels[i].classList.add("section-collapsed");
  }
}
function expandAllSections() {
  var labels = document.querySelectorAll(".section-toggle");
  for (var i = 0; i < labels.length; i++) {
    labels[i].classList.remove("section-collapsed");
  }
}
function buildCategoryTabs(categories) {
  var tabs = document.getElementById("categoryTabs");
  if (!tabs || !categories) return;
  tabs.innerHTML = "";
  for (var i = 0; i < categories.length; i++) {
    var cat = categories[i];
    var tab = document.createElement("a");
    tab.className = "category-tab" + (i === 0 ? " active" : "");
    tab.href = "#cat-" + i;
    tab.textContent = cat.label;
    (function(idx) {
      tab.addEventListener("click", function(ev) {
        ev.preventDefault();
        var allTabs = document.querySelectorAll(".category-tab");
        for (var t = 0; t < allTabs.length; t++) allTabs[t].classList.remove("active");
        this.classList.add("active");
        var sec = document.getElementById("cat-section-" + idx);
        if (sec) {
          categoriesContainer.scrollTo({ left: sec.offsetLeft, behavior: "smooth" });
          setTimeout(resizeContainerToActive, 350);
        }
      });
    })(i);
    tabs.appendChild(tab);
  }
}

/* ── Resize container to active section height (mobile) ── */
function resizeContainerToActive() {
  if (window.innerWidth > 600) {
    categoriesContainer.style.height = "";
    return;
  }
  var scrollLeft = categoriesContainer.scrollLeft;
  var width = categoriesContainer.offsetWidth;
  var idx = Math.round(scrollLeft / width);
  var sections = categoriesContainer.querySelectorAll(".category-section");
  if (sections[idx]) {
    categoriesContainer.style.height = sections[idx].offsetHeight + "px";
  }
}

/* ── Swipe sync: update active tab on scroll ──────── */
var swipeTimeout = null;
categoriesContainer.addEventListener("scroll", function() {
  if (swipeTimeout) clearTimeout(swipeTimeout);
  swipeTimeout = setTimeout(function() {
    var scrollLeft = categoriesContainer.scrollLeft;
    var width = categoriesContainer.offsetWidth;
    var idx = Math.round(scrollLeft / width);
    var allTabs = document.querySelectorAll(".category-tab");
    for (var t = 0; t < allTabs.length; t++) allTabs[t].classList.remove("active");
    if (allTabs[idx]) allTabs[idx].classList.add("active");
    resizeContainerToActive();
  }, 100);
}, {passive: true});
window.addEventListener("resize", resizeContainerToActive);

function removeFromHistoryStorage(idx) {
  try {
    var key = "gc-history";
    var arr = JSON.parse(localStorage.getItem(key) || "[]");
    var i = parseInt(idx, 10);
    if (!isNaN(i) && i >= 0 && i < arr.length) {
      arr.splice(i, 1);
      localStorage.setItem(key, JSON.stringify(arr));
    }
  } catch(e) {}
}


function getLocation() {
  if (!navigator.geolocation) {
    cmdInput.value = "locate (no GPS)";
    return;
  }
  cmdInput.value = "locating...";
  navigator.geolocation.getCurrentPosition(
    function(pos) {
      var lat = pos.coords.latitude.toFixed(6);
      var lon = pos.coords.longitude.toFixed(6);
      var cmd = "locate " + lat + " " + lon;
      cmdInput.value = cmd;
      executeCommand(cmd, null);
    },
    function(err) {
      cmdInput.value = "locate (GPS denied)";
    },
    {enableHighAccuracy: true, timeout: 10000}
  );
}

function toggleSection(labelEl) {
  labelEl.classList.toggle("section-collapsed");
}

function esc(s) {
  if (!s) return "";
  var d = document.createElement("div");
  d.textContent = s;
  return d.innerHTML;
}

/* Enrich response text: replace service names with favicon + name */
function enrichWithFavicons(text) {
  if (!configData || !configData.services) return esc(text);
  var result = esc(text);
  var svcs = configData.services;
  for (var name in svcs) {
    if (svcs[name].domain) {
      var favicon = '<img class="svc-favicon" src="https://' + esc(svcs[name].domain) + '/favicon.ico" alt="" onerror="this.style.display=\'none\'" style="width:14px;height:14px;border-radius:2px;vertical-align:middle;margin-right:2px;">';
      var escaped = esc(name);
      var okPattern = "\u2705 " + escaped;
      if (result.indexOf(okPattern) >= 0) {
        result = result.split(okPattern).join(favicon + escaped);
      } else {
        result = result.split(escaped).join(favicon + escaped);
      }
    }
  }
  return result;
}


/* ── History Persistence ─────────────────────────────── */
function loadHistory() {
  try {
    var raw = localStorage.getItem(HISTORY_KEY);
    if (raw) return JSON.parse(raw);
  } catch(e) {}
  return [];
}

function saveHistory(entries) {
  try {
    localStorage.setItem(HISTORY_KEY, JSON.stringify(entries.slice(0, MAX_HISTORY)));
  } catch(e) {}
}

function renderSavedHistory() {
  var entries = loadHistory();
  for (var i = 0; i < entries.length; i++) {
    var e = entries[i];
    appendHistoryDOM(e.command, e.title, e.message, e.error, e.time, false);
  }
}

function addToHistory(command, title, message, isError) {
  var entries = loadHistory();
  var entry = {
    command: command,
    title: title || "",
    message: message || "",
    error: !!isError,
    time: new Date().toLocaleTimeString([], {hour: "2-digit", minute: "2-digit"})
  };
  entries.unshift(entry);
  if (entries.length > MAX_HISTORY) entries = entries.slice(0, MAX_HISTORY);
  saveHistory(entries);
  updateFooterCount();
  return entry;
}

function appendHistoryDOM(command, title, message, isError, timeStr, prepend) {
  var div = document.createElement("div");
  div.className = "h-entry" + (isError ? " error" : "");

  var cmdDiv = document.createElement("div");
  cmdDiv.className = "h-cmd";
  cmdDiv.innerHTML = '<span class="h-cmd-chevron">\u25BE</span>' +
    '<span class="h-cmd-prefix">&gt; </span>' + esc(command) +
    '<span class="h-cmd-time">' + esc(timeStr || "") + '</span>';
  cmdDiv.addEventListener("click", function() {
    div.classList.toggle("collapsed");
  });

  var respDiv = document.createElement("div");
  respDiv.className = "h-resp";
  if (title || message) {
    respDiv.setAttribute("data-raw", message || "");
    respDiv.innerHTML = '<div class="h-resp-title">' + esc(title) + '</div>' + enrichWithFavicons(message);
  }

  div.appendChild(cmdDiv);
  div.appendChild(respDiv);

  if (prepend || prepend === undefined) {
    historyList.insertBefore(div, historyList.firstChild);
  } else {
    historyList.appendChild(div);
  }
  return div;
}

function refreshHistoryFavicons() {
  var resps = historyList.querySelectorAll(".h-resp");
  for (var i = 0; i < resps.length; i++) {
    var el = resps[i];
    var titleEl = el.querySelector(".h-resp-title");
    var title = titleEl ? titleEl.textContent : "";
    var raw = el.getAttribute("data-raw");
    if (raw) {
      el.innerHTML = '<div class="h-resp-title">' + esc(title) + '</div>' + enrichWithFavicons(raw);
    }
  }
}

function createLoadingEntry(command) {
  var div = document.createElement("div");
  div.className = "h-entry loading";

  var cmdDiv = document.createElement("div");
  cmdDiv.className = "h-cmd";
  var timeNow = new Date().toLocaleTimeString([], {hour: "2-digit", minute: "2-digit"});
  cmdDiv.innerHTML = '<span class="h-cmd-chevron">\u25BE</span>' +
    '<span class="h-cmd-prefix">&gt; </span>' + esc(command) +
    '<span class="h-cmd-time">' + esc(timeNow) + '</span>';
  cmdDiv.addEventListener("click", function() {
    div.classList.toggle("collapsed");
  });

  var respDiv = document.createElement("div");
  respDiv.className = "h-resp";
  respDiv.textContent = "running...";

  div.appendChild(cmdDiv);
  div.appendChild(respDiv);
  historyList.insertBefore(div, historyList.firstChild);
  return div;
}

/* ── Execute Command ─────────────────────────────────── */
function executeCommand(command, triggerEl) {
  if (!command || !command.trim()) return;
  command = command.trim();
  cmdInput.value = command;

  /* Show loading entry */
  var entry = createLoadingEntry(command);
  setTimeout(function() {
    var headerH = document.querySelector(".gc-header") ? document.querySelector(".gc-header").offsetHeight : 0;
    var inputH = document.querySelector(".input-bar") ? document.querySelector(".input-bar").offsetHeight : 0;
    var offset = headerH + inputH + 16;
    var top = entry.getBoundingClientRect().top + window.scrollY - offset;
    window.scrollTo({top: top, behavior: "smooth"});
  }, 100);

  /* Mark trigger element as running */
  if (triggerEl) {
    triggerEl.classList.add("running");
    runningElements.push(triggerEl);
  }
  runBtn.disabled = true;
  runBtn.setAttribute("aria-busy", "true");

  fetch(basePath + "/execute", {
    method: "POST",
    headers: {"Content-Type": "application/json"},
    body: JSON.stringify({command: command})
  })
  .then(function(res) { return res.json(); })
  .then(function(data) {
    entry.classList.remove("loading");
    var respDiv = entry.querySelector(".h-resp");
    if (data.error) {
      entry.classList.add("error");
      respDiv.innerHTML = '<div class="h-resp-title">' + esc("Error") + '</div>' + esc(data.error);
      addToHistory(command, "Error", data.error, true);
    } else {
      respDiv.innerHTML = '<div class="h-resp-title">' + esc(data.title || "") + '</div>' + enrichWithFavicons(data.message || "");
      addToHistory(command, data.title, data.message, false);
    }
  })
  .catch(function(err) {
    entry.classList.remove("loading");
    entry.classList.add("error");
    var respDiv = entry.querySelector(".h-resp");
    respDiv.innerHTML = '<div class="h-resp-title">Network Error</div>' + esc(err.message || "Request failed");
    addToHistory(command, "Network Error", err.message || "Request failed", true);
  })
  .finally(function() {
    runBtn.disabled = false;
    runBtn.removeAttribute("aria-busy");
    /* Remove running state from all triggers */
    for (var i = 0; i < runningElements.length; i++) {
      runningElements[i].classList.remove("running");
    }
    runningElements = [];
    cmdInput.focus();
  });
}

/* ── Command Input Handlers ──────────────────────────── */
cmdInput.addEventListener("keydown", function(e) {
  if (e.key === "Enter") {
    e.preventDefault();
    submitInput();
  }
});

runBtn.addEventListener("click", function() { submitInput(); });

function submitInput() {
  var cmd = cmdInput.value.trim();
  if (!cmd) return;
  var firstWord = cmd.split(" ")[0].toLowerCase();
  if (isDangerCmd(firstWord)) {
    if (!confirm("Are you sure you want to run: " + cmd + "?")) return;
  }
  cmdInput.value = "";
  collapseAll();
  executeCommand(cmd, null);
}

/* ── Command Type Mapping ────────────────────────────── */
var SERVICE_COMMANDS = ["restart", "stop", "start", "logs"];
var TARGET_COMMANDS  = ["free", "df", "uptime", "who", "ping", "top", "ports", "ip", "connections", "reboot", "shutdown"];
/* Everything else (status, services, help, updates, certs, traffic) runs directly */

function getCommandType(cmd) {
  if (SERVICE_COMMANDS.indexOf(cmd) >= 0) return "service";
  if (TARGET_COMMANDS.indexOf(cmd) >= 0)  return "target";
  return "direct";
}

/* ── Expand / Collapse Logic ─────────────────────────── */
function collapseAll() {
  if (expandedCard) {
    expandedCard.classList.remove("active");
    expandedCard = null;
  }
  if (expandedPanel) {
    expandedPanel.classList.remove("visible");
    expandedPanel.innerHTML = "";
    expandedPanel = null;
  }
}

function isDangerCmd(cmd) {
  if (configData && configData.categories) {
    for (var ci = 0; ci < configData.categories.length; ci++) {
      var cat = configData.categories[ci];
      if (cat.danger && cat.commands && cat.commands.indexOf(cmd) >= 0) return true;
    }
  }
  /* Fallback: hardcoded danger commands */
  return ["reboot", "shutdown"].indexOf(cmd) >= 0;
}

function showTargetPanel(panel, cmd) {
  var isDanger = isDangerCmd(cmd);
  var html = '<div class="target-row">';
  var machines = (configData && configData.machines) ? configData.machines : ["vps", "mac"];
  for (var i = 0; i < machines.length; i++) {
    var m = machines[i];
    html += '<button class="target-btn' + (isDanger ? " danger" : "") + '" data-cmd="' + esc(cmd) + '" data-target="' + esc(m) + '">';
    html += esc(capitalize(m));
    html += "</button>";
  }
  html += "</div>";
  panel.innerHTML = html;
  panel.classList.add("visible");
  var btns = panel.querySelectorAll(".target-btn");
  for (var j = 0; j < btns.length; j++) {
    btns[j].addEventListener("click", handleTargetClick);
  }
}

function showServicePanel(panel, cmd) {
  if (!configData || !configData.services) {
    panel.innerHTML = '<div style="padding:8px;color:var(--pico-muted-color);">No services loaded</div>';
    panel.classList.add("visible");
    return;
  }
  /* Group services by machine */
  var groups = {};
  var svcNames = Object.keys(configData.services);
  svcNames.sort();
  for (var i = 0; i < svcNames.length; i++) {
    var name = svcNames[i];
    var svc = configData.services[name];
    var machine = svc.machine || "unknown";
    if (!groups[machine]) groups[machine] = [];
    groups[machine].push({name: name, port: svc.port, desc: svc.description, domain: svc.domain || ""});
  }
  var html = "";
  var machineNames = Object.keys(groups);
  machineNames.sort();
  for (var m = 0; m < machineNames.length; m++) {
    var mName = machineNames[m];
    var svcs = groups[mName];
    html += '<div class="machine-group">';
    html += '<div class="machine-label">' + esc(capitalize(mName)) + " (" + svcs.length + ")</div>";
    html += '<div class="chip-wrap">';
    for (var s = 0; s < svcs.length; s++) {
      var sv = svcs[s];
      html += '<button class="svc-chip" data-cmd="' + esc(cmd) + '" data-svc="' + esc(sv.name) + '">';
      if (sv.domain) html += '<img class="svc-favicon" src="https://' + esc(sv.domain) + '/favicon.ico" alt="" onerror="this.style.display=\'none\'">';
      html += esc(sv.desc || sv.name);
      html += "</button>";
    }
    html += "</div></div>";
  }
  panel.innerHTML = html;
  panel.classList.add("visible");
  var chips = panel.querySelectorAll(".svc-chip");
  for (var c = 0; c < chips.length; c++) {
    chips[c].addEventListener("click", handleChipClick);
  }
}

function handleTargetClick(e) {
  var btn = e.currentTarget;
  var cmd = btn.getAttribute("data-cmd");
  var target = btn.getAttribute("data-target");
  var fullCmd = cmd + " " + target;
  if (isDangerCmd(cmd)) {
    if (!confirm("Are you sure you want to " + cmd + " " + target + "?")) return;
  }
  executeCommand(fullCmd, btn);
}

function handleChipClick(e) {
  var chip = e.currentTarget;
  var cmd = chip.getAttribute("data-cmd");
  var svc = chip.getAttribute("data-svc");
  var fullCmd = cmd + " " + svc;
  cmdInput.value = fullCmd;
  executeCommand(fullCmd, chip);
}

function toggleCard(cardEl, cmd, panel) {
  /* Same card tapped again: collapse */
  if (expandedCard === cardEl) {
    collapseAll();
    return;
  }
  collapseAll();

  var type = cardEl.getAttribute("data-type") || "direct";

  /* Direct commands run immediately */
  if (type === "direct") {
    if (cmd === "locate") {
      getLocation();
      return;
    }
    executeCommand(cmd, cardEl);
    return;
  }

  expandedCard = cardEl;
  expandedPanel = panel;
  cardEl.classList.add("active");
  cmdInput.value = cmd;

  if (type === "machine") {
    showTargetPanel(panel, cmd);
  } else {
    showServicePanel(panel, cmd);
  }
}

/* ── Build Categories ────────────────────────────────── */
function buildCategories(data) {
  categoriesContainer.innerHTML = "";
  if (!data || !data.categories || !data.categories.length) {
    /* Fallback: two default categories */
    data = {
      machines: (data && data.machines) || ["vps", "mac"],
      services: (data && data.services) || {},
      categories: [
        {label: "\uD83D\uDCCA System", commands: ["status", "free", "df", "uptime", "who", "ping", "reboot", "shutdown", "top", "ports", "ip", "updates", "certs", "connections"], danger: false},
        {label: "\uD83D\uDD27 Services", commands: ["restart", "stop", "start", "logs"], danger: false}
      ]
    };
  }

  for (var ci = 0; ci < data.categories.length; ci++) {
    var cat = data.categories[ci];
    /* Unique slug for IDs */
    var slug = "cat" + ci;

    /* Section header */
    var header = document.createElement("h3");
    header.className = "section-label";
    header.id = "cat-" + ci;
    header.setAttribute("data-slug", slug);
    header.innerHTML = esc(cat.label);


    /* Card grid */
    var grid = document.createElement("div");
    grid.className = "card-grid";
    grid.id = slug + "Grid";

    /* Expand panel */
    var panel = document.createElement("div");
    panel.className = "expand-panel";
    panel.id = slug + "Panel";

    /* Build cards */
    var commands = cat.commands || [];
    for (var i = 0; i < commands.length; i++) {
      var cmd = commands[i];
      var btn = document.createElement("button");
      var isDanger = cat.danger || isDangerCmd(cmd);
      btn.className = "action-card" + (isDanger ? " danger" : "");
      btn.textContent = cmd;
      btn.setAttribute("data-cmd", cmd);
      btn.setAttribute("data-type", cat.type || "direct");
      (function(c, b, p) {
        b.addEventListener("click", function() { toggleCard(b, c, p); });
      })(cmd, btn, panel);
      grid.appendChild(btn);
    }

    var section = document.createElement("div");
    section.className = "category-section" + (ci === 0 ? " active" : "");
    section.id = "cat-section-" + ci;
    section.appendChild(header);
    section.appendChild(grid);
    section.appendChild(panel);
    categoriesContainer.appendChild(section);
  }
}

/* ── Load Config ─────────────────────────────────────── */
function loadConfig() {
  fetch(basePath + "/config")
    .then(function(res) {
      if (!res.ok) throw new Error("Config fetch failed: " + res.status);
      return res.json();
    })
    .then(function(data) {
      configData = data;
      buildCategories(data);
      buildCategoryTabs(data.categories);
      refreshHistoryFavicons();
      setTimeout(resizeContainerToActive, 100);
    })
    .catch(function() {
      buildCategories(null);
      buildCategoryTabs([
        {label: "\uD83D\uDCCA System"},
        {label: "\uD83D\uDD27 Services"}
      ]);
    });
}

/* ── Footer Count ───────────────────────────────────── */
function updateFooterCount() {
  var count = loadHistory().length;
  cmdCount.textContent = count + " command" + (count !== 1 ? "s" : "");
}

/* ── Health Polling ──────────────────────────────────── */
var healthOk = false;

function pollHealth() {
  fetch(basePath + "/health")
    .then(function(res) {
      if (!res.ok) throw new Error("Health check failed");
      return res.json();
    })
    .then(function(data) {
      healthOk = true;
      statusDot.className = "status-dot";
      statusText.textContent = "Connected";
    })
    .catch(function() {
      healthOk = false;
      statusDot.className = "status-dot offline";
      statusText.textContent = "Disconnected";
    });
}

/* ── Helpers ──────────────────────────────────────────── */
function capitalize(s) {
  if (!s) return "";
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/* ── History Toolbar ─────────────────────────────────── */
collapseAllBtn.addEventListener("click", function(e) {
  e.preventDefault();
  var entries = historyList.querySelectorAll(".h-entry");
  for (var i = 0; i < entries.length; i++) {
    entries[i].classList.add("collapsed");
  }
});

expandAllBtn.addEventListener("click", function(e) {
  e.preventDefault();
  var entries = historyList.querySelectorAll(".h-entry");
  for (var i = 0; i < entries.length; i++) {
    entries[i].classList.remove("collapsed");
  }
});

clearHistoryBtn.addEventListener("click", function(e) {
  e.preventDefault();
  historyList.innerHTML = "";
  try { localStorage.removeItem(HISTORY_KEY); } catch(e) {}
  updateFooterCount();
});

/* ── Back to Top ────────────────────────────────────── */
window.addEventListener("scroll", function() {
  if (window.scrollY > 200) {
    backToTop.classList.add("visible");
  } else {
    backToTop.classList.remove("visible");
  }
}, {passive: true});

backToTop.addEventListener("click", function() {
  window.scrollTo({top: 0, behavior: "smooth"});
});

/* ── Init ────────────────────────────────────────────── */
(function init() {
  renderSavedHistory();
  /* Dev: seed history if empty */
  if (!loadHistory().length) {
    addToHistory("free vps", "\uD83D\uDCBE Memory", "\uD83D\uDCBE Vps Memory\n              total        used        free\nMem:         7.8Gi       3.3Gi       933Mi\nSwap:        2.0Gi       768Mi       1.3Gi", false);
    addToHistory("restart drolosoft", "Restart Drolosoft", "\u2705 drolosoft restarted on VPS (:2005)", false);
    addToHistory("df vps", "\uD83D\uDCBF Disk", "Filesystem      Size  Used Avail Use% Mounted\n/dev/sda1        78G   42G   33G  57% /", false);
    renderSavedHistory();
  }
  updateFooterCount();
  
/* ── Authentication ──────────────────────────────────── */

















function initApp() {
  loadConfig();
  pollHealth();
  setInterval(pollHealth, 30000);
}
initApp();
})();
</script>
</body>
</html>`

// headerIconB64 is the base64-encoded header icon PNG.
const headerIconB64 = "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"
