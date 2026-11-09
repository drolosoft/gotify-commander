//go:build preview

package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/drolosoft/gotify-commander/internal/commander"
	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/ratelimit"
)

// The control panel only exists inside a running Gotify, which needs Docker
// and a plugin built for Linux. "make preview" serves the same page from this
// test instead, so a change to web/ can be looked at in a browser in seconds.
// Nothing it shows is real: every command gets a canned answer and no command
// is ever run on this machine. The one exception is locate, which runs no
// command but does ask the real geocoder for the address.

// previewAddress is where the preview listens, unless PREVIEW_ADDR says
// another address. It is the loopback interface only.
const previewAddress = "127.0.0.1:8377"

// previewPassword is the control panel password of the preview.
const previewPassword = "preview"

// previewDelay is how long every canned command takes, so the running state
// of an entry can be seen.
const previewDelay = 600 * time.Millisecond

// previewRateLimit is how many commands a minute the preview accepts.
const previewRateLimit = 600

// previewDisk is what duf prints for one disk.
const previewDisk = `╭───────────────────────────────────────────────────────────────────╮
│ 1 local device                                                    │
├────────────┬───────┬───────┬───────┬────────┬──────┬─────────────┤
│ MOUNTED ON │  SIZE │  USED │ AVAIL │  USE%  │ TYPE │ FILESYSTEM  │
├────────────┼───────┼───────┼───────┼────────┼──────┼─────────────┤
│ /          │ 96.0G │ 53.2G │ 38.0G │  55.3% │ ext4 │ /dev/sda    │
╰────────────┴───────┴───────┴───────┴────────┴──────┴─────────────╯`

// previewLog is what journalctl prints for a service.
const previewLog = `Nov 09 10:41:02 vps nginx[812]: 2026/11/09 10:41:02 [notice] 812#812: signal process started
Nov 09 10:41:02 vps systemd[1]: Reloaded nginx.service - A high performance web server and a reverse proxy server.
Nov 09 10:44:19 vps nginx[812]: 203.0.113.7 - - "GET /health HTTP/2.0" 200 17 "-" "kuma/2.0"
Nov 09 10:45:19 vps nginx[812]: 203.0.113.7 - - "GET /health HTTP/2.0" 200 17 "-" "kuma/2.0"`

// previewExecutor answers every command from a table instead of running it.
type previewExecutor struct{}

// Run returns the canned output of the first tool named in the command line.
// One that is not in the table answers "ok", and a failure is never
// simulated except for the service called "broken".
func (previewExecutor) Run(_ context.Context, name string, args ...string) (executor.Result, error) {
	time.Sleep(previewDelay)

	line := name + " " + strings.Join(args, " ")
	answers := []struct {
		tool   string
		output string
	}{
		{"duf", previewDisk},
		{"journalctl", previewLog},
		{"is-active", "active"},
		{"uptime", " 10:45:31 up 12 days,  4:38,  2 users,  load average: 0.23, 0.31, 0.27"},
		{"who", "admin    pts/0        2026-11-09 09:12 (192.0.2.10)"},
		{"free", "               total        used        free      shared  buff/cache   available\nMem:           7.8Gi       3.1Gi       1.2Gi       112Mi       3.4Gi       4.3Gi\nSwap:          2.0Gi       140Mi       1.9Gi"},
	}

	if strings.Contains(line, "broken") {
		return executor.Result{Output: "Job for broken.service failed because the control process exited with error code.", ExitCode: 1}, nil
	}

	for _, answer := range answers {
		if strings.Contains(line, answer.tool) {
			return executor.Result{Output: answer.output}, nil
		}
	}

	return executor.Result{Output: "ok"}, nil
}

// previewConfig is a small homelab: two machines and a handful of services,
// one of them made to fail.
func previewConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.WebPassword = previewPassword
	cfg.Docker.AllowedContainers = []string{"immich", "paperless"}
	cfg.Services = map[string]config.Service{
		"nginx":     {Description: "Web server", Machine: "vps", Port: 443, Systemd: "nginx"},
		"gotify":    {Description: "Notifications", Machine: "vps", Port: 2006, Systemd: "gotify"},
		"gitea":     {Description: "Git forge", Machine: "mac", Port: 3000, Launchd: "com.example.gitea"},
		"vikunja":   {Description: "Tasks", Machine: "vps", Port: 3456, Systemd: "vikunja"},
		"matomo":    {Description: "Analytics", Machine: "vps", Port: 8080, Systemd: "matomo"},
		"broken":    {Description: "Always fails", Machine: "vps", Port: 9999, Systemd: "broken"},
		"immich":    {Description: "Photos", Machine: "mac", Port: 2283, Launchd: "com.example.immich"},
		"paperless": {Description: "Documents", Machine: "mac", Port: 8000, Launchd: "com.example.paperless"},
		"pulse":     {Description: "Fleet dashboard", Machine: "vps", Port: 8090, Systemd: "pulse"},
	}

	return cfg
}

// TestPreviewPanel serves the control panel until the test is interrupted.
// Run it with "make preview".
func TestPreviewPanel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	panel := newTestPanel(t, previewConfig(), &memoryStorage{})

	// The commander Enable built runs real commands; this one does not.
	fake := commander.New(panel.plugin.cfg, previewExecutor{}, panel.plugin)
	fake.SetSSHExecutor(previewExecutor{})
	panel.plugin.mu.Lock()
	panel.plugin.cmdr = fake
	panel.plugin.mu.Unlock()

	// Ten commands a minute protect a real server; a preview is clicked
	// through much faster than that.
	panel.plugin.executeLimiter = ratelimit.New(previewRateLimit, time.Minute)

	address := os.Getenv("PREVIEW_ADDR")
	if address == "" {
		address = previewAddress
	}

	t.Logf("control panel: http://%s%s  (password: %s)", address, panelBase, previewPassword)

	if err := http.ListenAndServe(address, panel.engine); err != nil {
		t.Fatalf("preview server: %v", err)
	}
}
