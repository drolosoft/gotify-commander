package command

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/security"
)

// Version is the plugin version sent in the User-Agent of outgoing HTTP
// requests. The root package sets it at startup; "dev" marks a build that did
// not.
var Version = "dev"

// nominatimReverseURL is OpenStreetMap's reverse geocoding endpoint.
const nominatimReverseURL = "https://nominatim.openstreetmap.org/reverse"

// geocodeTimeout bounds a whole reverse geocoding request, body included.
const geocodeTimeout = 15 * time.Second

// maxGeocodeBody is the most of a Nominatim answer that is read. A reverse
// lookup is a few kilobytes; anything bigger is not an answer to parse.
const maxGeocodeBody = 1 << 20

// Builtins holds dependencies for all built-in command handlers.
type Builtins struct {
	cfg     *config.Config
	exec    executor.Executor
	sshExec executor.Executor // nil until SSH wired in Task 12

	// httpClient and nominatimURL serve the locate command; tests point the
	// URL at a local server.
	httpClient   *http.Client
	nominatimURL string
}

// NewBuiltins creates a Builtins instance with the given config and executor.
func NewBuiltins(cfg *config.Config, exec executor.Executor) *Builtins {
	return &Builtins{
		cfg:          cfg,
		exec:         exec,
		httpClient:   &http.Client{Timeout: geocodeTimeout},
		nominatimURL: nominatimReverseURL,
	}
}

// SetSSHExecutor sets the SSH executor for Mac targets.
func (b *Builtins) SetSSHExecutor(exec executor.Executor) {
	b.sshExec = exec
}

// timeout returns a context with the configured timeout (default 30s).
func (b *Builtins) timeout() (context.Context, context.CancelFunc) {
	d := b.cfg.Defaults.Timeout
	if d == 0 {
		d = 30 * time.Second
	}
	return context.WithTimeout(context.Background(), d)
}

// groupServices returns sorted service names split by machine type.
func (b *Builtins) groupServices() (vps, mac []string) {
	for name, svc := range b.cfg.Services {
		switch svc.Machine {
		case "vps":
			vps = append(vps, name)
		case "mac":
			mac = append(mac, name)
		}
	}
	sort.Strings(vps)
	sort.Strings(mac)
	return
}

// formatServiceList renders services grouped by VPS/Mac as a markdown list.
func (b *Builtins) formatServiceList() string {
	vps, mac := b.groupServices()
	total := len(vps) + len(mac)
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("### 📡 Services (%d)\n\n", total))

	writeGroup := func(label string, count int, names []string) {
		if len(names) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("**── %s (%d) ──**\n", label, count))
		for _, name := range names {
			svc := b.cfg.Services[name]
			line := fmt.Sprintf("- `%s`", name)
			if svc.Port > 0 {
				line += fmt.Sprintf(" :%d", svc.Port)
			}
			if svc.Description != "" {
				line += " — " + svc.Description
			}
			if len(svc.Aliases) > 0 {
				line += fmt.Sprintf(" *[%s]*", strings.Join(svc.Aliases, ", "))
			}
			sb.WriteString(line + "\n")
		}
	}

	writeGroup("VPS", len(vps), vps)
	if len(mac) > 0 {
		sb.WriteString("\n")
	}
	writeGroup("Mac Mini", len(mac), mac)

	return strings.TrimRight(sb.String(), "\n")
}

// execForTarget returns sshExec for "mac" if available, else the local executor.
func (b *Builtins) execForTarget(target string) executor.Executor {
	if strings.EqualFold(target, "mac") && b.sshExec != nil {
		return b.sshExec
	}
	return b.exec
}

// title capitalises the first letter of s.
func (b *Builtins) title(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = []rune(strings.ToUpper(string(r[0])))[0]
	return string(r)
}

// errorResponse builds a high-priority error response.
func errorResponse(titleText, message string) Response {
	return Response{
		Title:    "❌ " + titleText,
		Message:  message,
		Priority: 7,
	}
}

// ─── 1. Help ─────────────────────────────────────────────────────────────────

// Help returns the full command reference plus service listing.
func (b *Builtins) Help(cmd Command) Response {
	ref := `## 🤖 Gotify Commander

### 📋 Commands

| Command | Description |
|---------|-------------|
| ` + "`restart <svc>`" + ` | 🔄 Restart a service |
| ` + "`stop <svc>`" + ` | 🛑 Stop a service |
| ` + "`start <svc>`" + ` | ▶️ Start a service |
| ` + "`status [svc]`" + ` | 📊 Status (all if omitted) |
| ` + "`logs <svc>`" + ` | 📜 Tail service logs |
| ` + "`free [vps/mac]`" + ` | 💾 Memory usage |
| ` + "`df [vps/mac]`" + ` | 💿 Disk usage |
| ` + "`uptime [vps/mac]`" + ` | ⏱️ Machine uptime |
| ` + "`top [vps/mac]`" + ` | 📊 Top processes |
| ` + "`who [vps/mac]`" + ` | 👤 Logged-in users |
| ` + "`ping [vps/mac]`" + ` | 🏓 Connectivity check |
| ` + "`ip [vps/mac]`" + ` | 🌐 IP addresses |
| ` + "`ports [vps/mac]`" + ` | 🔌 Listening ports |
| ` + "`connections [vps/mac]`" + ` | 🔗 Socket stats |
| ` + "`traffic [svc]`" + ` | 📈 Nginx traffic (rhit) |
| ` + "`analytics`" + ` | 📊 Web analytics (goaccess) |
| ` + "`certs`" + ` | 🔒 SSL cert expiry |
| ` + "`updates`" + ` | 📦 Pending apt updates |
| ` + "`locate <lat> <lon>`" + ` | 📍 GPS reverse geocode |
| ` + "`reboot vps/mac`" + ` | 🔁 Reboot a machine |
| ` + "`shutdown vps/mac`" + ` | ⚠️ Shutdown a machine |
| ` + "`services`" + ` | 📋 List all services |
| ` + "`help`" + ` | ❓ This message |

### ⚡ Quick Actions

Try: ` + "`status`" + ` · ` + "`free`" + ` · ` + "`ping`" + ` · ` + "`services`" + `
`

	msg := ref + "\n" + b.formatServiceList()
	if b.cfg.WebURL != "" {
		msg += "\n\n---\n🖥️ **Control Panel:** [Open](" + b.cfg.WebURL + ")\n" + b.cfg.WebURL
	}
	return Response{Title: "🤖 Help", Message: msg, Priority: 1, Markdown: true}
}

// ─── 2. Ping ─────────────────────────────────────────────────────────────────

// Ping returns a simple pong response.
func (b *Builtins) Ping(cmd Command) Response {
	return Response{Title: "🏓 Ping", Message: "pong", Priority: 1}
}

// ─── 3. Services ─────────────────────────────────────────────────────────────

// Services lists all configured services grouped by machine.
func (b *Builtins) Services(cmd Command) Response {
	return Response{Title: "📋 Services", Message: b.formatServiceList(), Priority: 1, Markdown: true}
}

// ─── service action helper ───────────────────────────────────────────────────

// serviceAction runs start/stop/restart for a service based on its machine type.
func (b *Builtins) serviceAction(action, name string, svc config.Service) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	switch svc.Machine {
	case "vps":
		result, err := b.exec.Run(ctx, "sudo", "systemctl", action, svc.Systemd)
		if err != nil || result.ExitCode != 0 {
			detail := ""
			if result.Output != "" {
				detail = "\n" + result.Output
			}
			if err != nil {
				detail = "\n" + err.Error()
			}
			return errorResponse(b.title(action)+" Failed", fmt.Sprintf("Failed to %s %s%s", action, name, detail))
		}
		portStr := ""
		if svc.Port > 0 {
			portStr = fmt.Sprintf(" (:%d)", svc.Port)
		}
		var emoji string
		switch action {
		case "restart":
			emoji = "✅"
		case "stop":
			emoji = "🛑"
		case "start":
			emoji = "▶️"
		default:
			emoji = "✅"
		}
		return b.serviceResponse(action, name, "VPS", portStr, emoji, svc)

	case "mac":
		if b.sshExec == nil {
			return errorResponse("SSH Not Configured", "SSH executor not configured for Mac targets")
		}
		var cmds [][]string
		switch action {
		case "restart":
			cmds = [][]string{
				{"launchctl", "unload", svc.Launchd},
				{"launchctl", "load", svc.Launchd},
			}
		case "stop":
			cmds = [][]string{{"launchctl", "unload", svc.Launchd}}
		case "start":
			cmds = [][]string{{"launchctl", "load", svc.Launchd}}
		}
		for _, c := range cmds {
			result, err := b.sshExec.Run(ctx, c[0], c[1:]...)
			if err != nil || result.ExitCode != 0 {
				detail := result.Output
				if err != nil {
					detail = err.Error()
				}
				return errorResponse(b.title(action)+" Failed", fmt.Sprintf("Failed to %s %s: %s", action, name, detail))
			}
		}
		portStr := ""
		if svc.Port > 0 {
			portStr = fmt.Sprintf(" (:%d)", svc.Port)
		}
		var emoji string
		switch action {
		case "restart":
			emoji = "✅"
		case "stop":
			emoji = "🛑"
		case "start":
			emoji = "▶️"
		default:
			emoji = "✅"
		}
		return b.serviceResponse(action, name, "Mac", portStr, emoji, svc)
	}

	return errorResponse("Unknown Machine", fmt.Sprintf("unknown machine type for service %q", name))
}

// lookupService resolves a target (name or alias) to its canonical name and Service.
func (b *Builtins) lookupService(target string) (string, config.Service, bool) {
	// Direct match.
	if svc, ok := b.cfg.Services[target]; ok {
		return target, svc, true
	}
	// Alias match.
	for name, svc := range b.cfg.Services {
		for _, alias := range svc.Aliases {
			if alias == target {
				return name, svc, true
			}
		}
	}
	return "", config.Service{}, false
}

// serviceResponse builds a response for service actions.
func (b *Builtins) serviceResponse(action, name, machine, portStr, emoji string, svc config.Service) Response {
	return Response{
		Title:    b.title(action) + " " + b.title(name),
		Message:  fmt.Sprintf("%s %s %sed on %s%s", emoji, name, action, machine, portStr),
		Priority: 3,
	}
}

// ─── 4. Restart ──────────────────────────────────────────────────────────────

func (b *Builtins) Restart(cmd Command) Response {
	name, svc, ok := b.lookupService(cmd.Target)
	if !ok {
		return errorResponse("Unknown Service", fmt.Sprintf("service %q not found. Use 'services' to list available services.", cmd.Target))
	}
	return b.serviceAction("restart", name, svc)
}

// ─── 5. Stop ─────────────────────────────────────────────────────────────────

func (b *Builtins) Stop(cmd Command) Response {
	name, svc, ok := b.lookupService(cmd.Target)
	if !ok {
		return errorResponse("Unknown Service", fmt.Sprintf("service %q not found. Use 'services' to list available services.", cmd.Target))
	}
	return b.serviceAction("stop", name, svc)
}

// ─── 6. Start ────────────────────────────────────────────────────────────────

func (b *Builtins) Start(cmd Command) Response {
	name, svc, ok := b.lookupService(cmd.Target)
	if !ok {
		return errorResponse("Unknown Service", fmt.Sprintf("service %q not found. Use 'services' to list available services.", cmd.Target))
	}
	return b.serviceAction("start", name, svc)
}

// ─── 7. Status ───────────────────────────────────────────────────────────────

func (b *Builtins) statusSingle(name string, svc config.Service) string {
	ctx, cancel := b.timeout()
	defer cancel()

	var status string
	var ok bool

	switch svc.Machine {
	case "vps":
		result, err := b.exec.Run(ctx, "systemctl", "is-active", svc.Systemd)
		status = strings.TrimSpace(result.Output)
		ok = err == nil && result.ExitCode == 0 && status == "active"
	case "mac":
		if b.sshExec == nil {
			status = "no SSH"
			ok = false
		} else if svc.Port > 0 {
			// HTTP health check — curl the port on localhost
			result, err := b.sshExec.Run(ctx, "curl", "-s", "-o", "/dev/null", "-w", "%{http_code}",
				"--max-time", "5", fmt.Sprintf("http://127.0.0.1:%d/", svc.Port))
			code := strings.TrimSpace(result.Output)
			status = "HTTP " + code
			ok = err == nil && code != "" && code != "000"
		} else {
			// No port — check if launchd service is loaded
			result, _ := b.sshExec.Run(ctx, "launchctl", "list")
			if strings.Contains(result.Output, svc.Launchd) {
				status = "loaded"
				ok = true
			} else {
				status = "not loaded"
				ok = false
			}
		}
	}

	icon := "❌"
	if ok {
		icon = "✅"
	}
	portStr := ""
	if svc.Port > 0 {
		portStr = fmt.Sprintf(" (:%d)", svc.Port)
	}
	return fmt.Sprintf("%s %s%s — %s", icon, name, portStr, status)
}

func (b *Builtins) statusAll() string {
	vps, mac := b.groupServices()
	var sb strings.Builder

	writeGroup := func(label string, count int, names []string) {
		if len(names) == 0 {
			return
		}
		sb.WriteString(fmt.Sprintf("**── %s (%d) ──**\n", label, count))
		for _, name := range names {
			svc := b.cfg.Services[name]
			sb.WriteString("- " + b.statusSingle(name, svc) + "\n")
		}
	}

	writeGroup("VPS", len(vps), vps)
	if len(mac) > 0 {
		sb.WriteString("\n")
	}
	writeGroup("Mac Mini", len(mac), mac)

	return strings.TrimRight(sb.String(), "\n")
}

func (b *Builtins) Status(cmd Command) Response {
	if cmd.Target == "" {
		return Response{Title: "📊 Status", Message: b.statusAll(), Priority: 3, Markdown: true}
	}
	name, svc, ok := b.lookupService(cmd.Target)
	if !ok {
		return errorResponse("Unknown Service", fmt.Sprintf("service %q not found.", cmd.Target))
	}
	line := b.statusSingle(name, svc)
	return Response{Title: "📊 Status — " + name, Message: line, Priority: 3}
}

// ─── 8. Free ─────────────────────────────────────────────────────────────────

func (b *Builtins) Free(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "VPS"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "free", "-h")
	if err != nil {
		return errorResponse("Free Failed", err.Error())
	}
	return Response{
		Title:    "💾 Memory",
		Message:  fmt.Sprintf("💾 %s Memory\n%s", b.title(target), result.Output),
		Priority: 3,
	}
}

// ─── 9. Df ───────────────────────────────────────────────────────────────────

func (b *Builtins) Df(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "VPS"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "duf", "-hide", "special,loops,binds")
	if err != nil {
		return errorResponse("Df Failed", err.Error())
	}
	return Response{
		Title:    "💿 Disk",
		Message:  fmt.Sprintf("💿 %s Disk\n%s", b.title(target), result.Output),
		Priority: 3,
	}
}

// ─── 10. Uptime ──────────────────────────────────────────────────────────────

func (b *Builtins) Uptime(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "VPS"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "uptime")
	if err != nil {
		return errorResponse("Uptime Failed", err.Error())
	}
	return Response{
		Title:    "⏱️ Uptime",
		Message:  fmt.Sprintf("⏱️ %s Uptime\n%s", b.title(target), result.Output),
		Priority: 3,
	}
}

// ─── 11. Who ─────────────────────────────────────────────────────────────────

func (b *Builtins) Who(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "VPS"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "who")
	if err != nil {
		return errorResponse("Who Failed", err.Error())
	}

	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "No users logged in"
	}

	return Response{
		Title:    "👤 Who",
		Message:  fmt.Sprintf("👤 %s Users\n%s", b.title(target), output),
		Priority: 3,
	}
}

// ─── 12. Logs ────────────────────────────────────────────────────────────────

func (b *Builtins) Logs(cmd Command) Response {
	name, svc, ok := b.lookupService(cmd.Target)
	if !ok {
		return errorResponse("Unknown Service", fmt.Sprintf("service %q not found.", cmd.Target))
	}

	if svc.Machine == "mac" {
		return Response{
			Title:    "📜 Logs",
			Message:  fmt.Sprintf("Logs for Mac services not yet supported (service: %s)", name),
			Priority: 3,
		}
	}

	lines := b.cfg.Defaults.LogLines
	if lines == 0 {
		lines = 30
	}

	ctx, cancel := b.timeout()
	defer cancel()

	result, err := b.exec.Run(ctx, "sudo", "journalctl", "-u", svc.Systemd, "-n", fmt.Sprintf("%d", lines), "--no-pager")
	if err != nil {
		return errorResponse("Logs Failed", fmt.Sprintf("failed to fetch logs for %s: %s", name, err.Error()))
	}

	return Response{
		Title:    fmt.Sprintf("📜 %s Logs", name),
		Message:  fmt.Sprintf("📜 %s Logs\n%s", name, result.Output),
		Priority: 3,
	}
}

// ─── 13. Reboot ──────────────────────────────────────────────────────────────

func (b *Builtins) Reboot(cmd Command) Response {
	if cmd.Target == "" {
		return Response{
			Title:    "❌ Reboot",
			Message:  "Target required. Usage: reboot <vps|mac>",
			Priority: 7,
		}
	}

	switch strings.ToLower(cmd.Target) {
	case "vps":
		ctx, cancel := b.timeout()
		defer cancel()

		_, err := b.exec.Run(ctx, "sudo", "reboot")
		if err != nil {
			// Reboot disconnects, so connection errors are expected — treat as success.
		}
		return Response{
			Title:    "⚠️ Reboot",
			Message:  "⚠️ VPS reboot command sent",
			Priority: 8,
		}

	case "mac":
		return Response{
			Title:    "⚠️ Reboot",
			Message:  "Mac reboot not yet supported",
			Priority: 5,
		}

	default:
		return errorResponse("Unknown Target", fmt.Sprintf("unknown reboot target %q. Use 'vps' or 'mac'.", cmd.Target))
	}
}

// ─── 14. Shutdown ────────────────────────────────────────────────────────────

func (b *Builtins) Shutdown(cmd Command) Response {
	if cmd.Target == "" {
		return Response{
			Title:    "❌ Shutdown",
			Message:  "Target required. Usage: shutdown <vps|mac>",
			Priority: 7,
		}
	}

	switch strings.ToLower(cmd.Target) {
	case "vps":
		ctx, cancel := b.timeout()
		defer cancel()

		_, err := b.exec.Run(ctx, "sudo", "shutdown", "-h", "now")
		if err != nil {
			// Shutdown disconnects, so connection errors are expected — treat as success.
		}
		return Response{
			Title:    "⚠️ Shutdown",
			Message:  "⚠️ VPS shutdown command sent",
			Priority: 8,
		}

	case "mac":
		return Response{
			Title:    "⚠️ Shutdown",
			Message:  "Mac shutdown not yet supported",
			Priority: 5,
		}

	default:
		return errorResponse("Unknown Target", fmt.Sprintf("unknown shutdown target %q. Use 'vps' or 'mac'.", cmd.Target))
	}
}

// ─── 15. Top ─────────────────────────────────────────────────────────────────

func (b *Builtins) Top(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "vps"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "ps", "aux", "--sort=-%cpu")
	if err != nil {
		return errorResponse("Top Failed", err.Error())
	}

	// Show header + top 5 CPU processes
	lines := strings.Split(strings.TrimRight(result.Output, "\n"), "\n")
	limit := 6
	if len(lines) < limit {
		limit = len(lines)
	}
	output := strings.Join(lines[:limit], "\n")

	return Response{
		Title:    "📊 Top Processes",
		Message:  fmt.Sprintf("📊 %s Top Processes\n%s", b.title(target), output),
		Priority: 3,
	}
}

// ─── 16. Ports ───────────────────────────────────────────────────────────────

func (b *Builtins) Ports(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "vps"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "ss", "-tlnp")
	if err != nil {
		return errorResponse("Ports Failed", err.Error())
	}
	return Response{
		Title:    "🔌 Listening Ports",
		Message:  fmt.Sprintf("🔌 %s Listening Ports\n%s", b.title(target), result.Output),
		Priority: 3,
	}
}

// ─── 17. Ip ──────────────────────────────────────────────────────────────────

func (b *Builtins) Ip(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "vps"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "hostname", "-I")
	if err != nil {
		return errorResponse("IP Failed", err.Error())
	}
	return Response{
		Title:    "🌐 IP Addresses",
		Message:  fmt.Sprintf("🌐 %s IP Addresses\n%s", b.title(target), strings.TrimSpace(result.Output)),
		Priority: 3,
	}
}

// ─── 18. Updates ─────────────────────────────────────────────────────────────

func (b *Builtins) Updates(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	result, err := b.exec.Run(ctx, "apt", "list", "--upgradable")
	if err != nil {
		return errorResponse("Updates Failed", err.Error())
	}

	// Strip the "Listing..." header line (tail -n +2 equivalent)
	lines := strings.Split(result.Output, "\n")
	var filtered []string
	for _, line := range lines {
		if line != "" && !strings.HasPrefix(line, "Listing...") {
			filtered = append(filtered, line)
		}
	}

	msg := strings.Join(filtered, "\n")
	if strings.TrimSpace(msg) == "" {
		msg = "System is up to date"
	}

	return Response{
		Title:    "📦 Available Updates",
		Message:  fmt.Sprintf("📦 Available Updates\n%s", msg),
		Priority: 3,
	}
}

// ─── 19. Certs ───────────────────────────────────────────────────────────────

// letsencryptLive is where certbot keeps one directory per certificate.
const letsencryptLive = "/etc/letsencrypt/live"

// Certs lists every Let's Encrypt certificate with its expiry date. find and
// openssl run directly, one openssl per certificate, so no path or domain
// name ever passes through a shell.
func (b *Builtins) Certs(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	result, err := b.exec.Run(ctx, "sudo", "find", letsencryptLive, "-name", "cert.pem")
	if err != nil {
		return errorResponse("Certs Failed", err.Error())
	}

	// stdout and stderr arrive together, so only lines that are certificate
	// paths count; find's complaints about unreadable directories do not.
	var paths []string
	for _, line := range strings.Split(result.Output, "\n") {
		if strings.HasPrefix(line, letsencryptLive+"/") && strings.HasSuffix(line, "/cert.pem") {
			paths = append(paths, line)
		}
	}
	sort.Strings(paths)

	var lines []string
	for _, path := range paths {
		expiry, err := b.certExpiry(ctx, path)
		if err != nil {
			return errorResponse("Certs Failed", err.Error())
		}

		domain := filepath.Base(filepath.Dir(path))
		lines = append(lines, domain+": "+expiry)
	}

	output := strings.Join(lines, "\n")
	if output == "" {
		output = "No certificates found in /etc/letsencrypt/live/"
	}

	return Response{
		Title:    "🔒 SSL Certificates",
		Message:  fmt.Sprintf("🔒 SSL Certificates\n%s", output),
		Priority: 3,
	}
}

// certExpiry returns the notAfter date openssl prints for the certificate at
// path, or "" when openssl cannot read it.
func (b *Builtins) certExpiry(ctx context.Context, path string) (string, error) {
	result, err := b.exec.Run(ctx, "sudo", "openssl", "x509", "-enddate", "-noout", "-in", path)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(result.Output, "\n") {
		if expiry, found := strings.CutPrefix(line, "notAfter="); found {
			return strings.TrimSpace(expiry), nil
		}
	}
	return "", nil
}

// ─── 20. Connections ─────────────────────────────────────────────────────────

func (b *Builtins) Connections(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	target := cmd.Target
	if target == "" {
		target = "vps"
	}

	exec := b.execForTarget(target)
	result, err := exec.Run(ctx, "ss", "-s")
	if err != nil {
		return errorResponse("Connections Failed", err.Error())
	}
	return Response{
		Title:    "🔗 Connections",
		Message:  fmt.Sprintf("🔗 %s Connections\n%s", b.title(target), result.Output),
		Priority: 3,
	}
}

// ─── 21. Traffic ────────────────────────────────────────────────────────────

// trafficFilterScript copies the nginx access log lines that mention a domain
// into a file. It is the one shell left in the builtins: the logs are far
// larger than the executor's output cap, so they must go from zcat to the file
// without passing through Go. The script is a constant; the domain ($1) and the
// file ($2) arrive as positional arguments and are never parsed as code.
const trafficFilterScript = `sudo zcat -f /var/log/nginx/access.log* 2>/dev/null | grep -i -F -e "$1" > "$2"`

// ansiEscape matches the terminal colour and cursor codes rhit prints.
var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// rhitProgress matches rhit's "123 / 456" progress lines.
var rhitProgress = regexp.MustCompile(`[0-9] / [0-9]`)

// rhitArgs returns the rhit arguments shared by both traffic reports, for the
// given log path and date range.
func rhitArgs(logPath, dateRange string) []string {
	return []string{"rhit", logPath, "--color", "no", "-s", "2xx,3xx", "-d", dateRange, "--length", "10", "-f", "date,status,ref"}
}

// cleanRhitOutput removes colour codes, progress lines and blank lines from
// rhit's output, keeping one trailing newline like the grep it replaces.
func cleanRhitOutput(output string) string {
	output = ansiEscape.ReplaceAllString(output, "")
	output = strings.ReplaceAll(output, "\r", "\n")

	var kept []string
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == "" || rhitProgress.MatchString(line) {
			continue
		}
		kept = append(kept, line)
	}

	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}

// Traffic reports nginx traffic of the last three months with rhit, for one
// site when a service is given and for all sites otherwise.
func (b *Builtins) Traffic(cmd Command) Response {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Calculate 3 months ago for default date range
	now := time.Now()
	threeMonthsAgo := now.AddDate(0, -3, 0)
	dateRange := fmt.Sprintf("%s-%s",
		threeMonthsAgo.Format("2006/01/02"),
		now.Format("2006/01/02"))

	if cmd.Target != "" {
		return b.siteTraffic(ctx, cmd.Target, dateRange)
	}

	// All traffic, last 3 months
	result, err := b.exec.Run(ctx, "sudo", rhitArgs("/var/log/nginx/", dateRange)...)
	if err != nil {
		return errorResponse("Traffic Failed", err.Error())
	}
	return Response{
		Title:    "📈 Traffic",
		Message:  fmt.Sprintf("📈 All Traffic (last 3 months)\n%s", cleanRhitOutput(result.Output)),
		Priority: 3,
	}
}

// siteTraffic filters the access logs for one service's domain into a temp
// file and runs rhit on it. The file is removed when the report returns,
// timeout included.
func (b *Builtins) siteTraffic(ctx context.Context, target, dateRange string) Response {
	// Per-site traffic: resolve service name to domain
	domain := target
	if svc, ok := b.cfg.Services[target]; ok && svc.Domain != "" {
		domain = svc.Domain
	}

	// The domain is only ever an argument, but a name with shell or grep
	// syntax in it is a mistake worth refusing before touching the logs.
	if err := security.ValidateInput(domain); err != nil {
		return errorResponse("Traffic Failed", "invalid service name: "+err.Error())
	}

	tempFile, err := os.CreateTemp("", "rhit-*.log")
	if err != nil {
		return errorResponse("Traffic Failed", err.Error())
	}
	tempPath := tempFile.Name()
	tempFile.Close()
	defer os.Remove(tempPath)

	// grep exits 1 when no line matches; like the && chain this replaces,
	// that means there is nothing for rhit to read.
	filter, err := b.exec.Run(ctx, "bash", "-c", trafficFilterScript, "gotify-traffic", domain, tempPath)
	if err != nil {
		return errorResponse("Traffic Failed", err.Error())
	}

	output := ""
	if filter.ExitCode == 0 {
		result, err := b.exec.Run(ctx, "sudo", rhitArgs(tempPath, dateRange)...)
		if err != nil {
			return errorResponse("Traffic Failed", err.Error())
		}
		output = cleanRhitOutput(result.Output)
	}

	if output == "" || strings.Contains(output, "0 hits") {
		output = "No traffic found for " + domain
	}
	return Response{
		Title:    fmt.Sprintf("📈 %s Traffic", domain),
		Message:  fmt.Sprintf("📈 %s (last 3 months)\n%s", domain, output),
		Priority: 3,
	}
}

// ─── 22. Analytics ──────────────────────────────────────────────────────────

// goaccessSummary maps the keys of goaccess's "general" CSV rows to the label
// each one gets, in the order goaccess prints them.
var goaccessSummary = map[string]string{
	"date_time":        "Generated: ",
	"total_requests":   "Total requests: ",
	"unique_visitors":  "Unique visitors: ",
	"unique_files":     "Unique pages: ",
	"bandwidth":        "Bandwidth: ",
	"unique_not_found": "404 errors: ",
}

// Analytics runs goaccess on today's access log and summarises its CSV.
func (b *Builtins) Analytics(cmd Command) Response {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := b.exec.Run(ctx, "sudo", "goaccess", "/var/log/nginx/access.log",
		"--log-format=COMBINED", "--no-term-resolver", "--no-color",
		"--output-format", "csv", "-o", "-")
	if err != nil {
		return errorResponse("Analytics Failed", err.Error())
	}

	output := parseGoaccessSummary(result.Output)
	if output == "" {
		output = "No analytics data available. Is goaccess installed?"
	}

	return Response{
		Title:    "📊 Analytics",
		Message:  fmt.Sprintf("📊 Web Analytics (today)\n\n%s", output),
		Priority: 3,
	}
}

// parseGoaccessSummary picks the summary figures out of goaccess CSV output.
// Each line is parsed on its own and unreadable lines are skipped, because
// goaccess's stderr arrives mixed into the same text.
func parseGoaccessSummary(output string) string {
	var lines []string

	for _, line := range strings.Split(output, "\n") {
		reader := csv.NewReader(strings.NewReader(line))
		reader.FieldsPerRecord = -1
		reader.LazyQuotes = true

		row, err := reader.Read()
		if err != nil || len(row) < 12 || row[2] != "general" {
			continue
		}

		key := row[11]
		value := row[10]
		label, known := goaccessSummary[key]
		if !known {
			continue
		}

		if key == "bandwidth" {
			value += " bytes"
		}
		lines = append(lines, label+value)
	}

	return strings.Join(lines, "\n")
}

// ─── 23. Locate ─────────────────────────────────────────────────────────────

// nominatimPlace is the part of a Nominatim reverse answer the message uses.
// Nominatim sends coordinates as strings, and "error" instead of an address
// for a point it cannot place, such as the open sea.
type nominatimPlace struct {
	Lat     string            `json:"lat"`
	Lon     string            `json:"lon"`
	Error   string            `json:"error"`
	Address map[string]string `json:"address"`
}

// markdownEscaper backslash-escapes the characters Gotify's markdown would
// read as formatting, links, headings or HTML, and folds line breaks into
// spaces so OSM text cannot start a line of its own.
var markdownEscaper = strings.NewReplacer(
	`\`, `\\`,
	"*", `\*`,
	"_", `\_`,
	"[", `\[`,
	"]", `\]`,
	"(", `\(`,
	")", `\)`,
	"`", "\\`",
	"#", `\#`,
	"<", `\<`,
	">", `\>`,
	"\r\n", " ",
	"\r", " ",
	"\n", " ",
)

// escapeMarkdown makes OSM-provided text safe to insert into the message.
func escapeMarkdown(text string) string {
	return markdownEscaper.Replace(text)
}

// parseCoordinate parses text as a finite number within ±limit.
func parseCoordinate(text string, limit float64) (float64, bool) {
	value, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false
	}
	if value < -limit || value > limit {
		return 0, false
	}
	return value, true
}

// formatCoordinate prints a coordinate in plain decimal notation, the form
// Nominatim and the OSM links expect.
func formatCoordinate(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// Locate reverse geocodes "locate <lat> <lon>" with OpenStreetMap Nominatim
// and answers with the address, a map link and a map tile. The coordinates
// are personal data: they are sent to Nominatim and back to the user, and
// never logged.
func (b *Builtins) Locate(cmd Command) Response {
	// Parse lat,lon from raw input: "locate 40.4168 -3.7038"
	parts := strings.Fields(cmd.Raw)
	if len(parts) < 3 {
		return errorResponse("Locate", "Usage: locate <lat> <lon>\nExample: locate 40.4168 -3.7038")
	}
	lat := parts[1]
	lon := parts[2]

	latitude, ok := parseCoordinate(lat, 90)
	if !ok {
		return errorResponse("Locate", "invalid latitude: "+lat+" (must be a number from -90 to 90)")
	}
	longitude, ok := parseCoordinate(lon, 180)
	if !ok {
		return errorResponse("Locate", "invalid longitude: "+lon+" (must be a number from -180 to 180)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), geocodeTimeout)
	defer cancel()

	place, err := b.reverseGeocode(ctx, latitude, longitude)
	if err != nil {
		return errorResponse("Locate Failed", err.Error())
	}

	output := formatPlace(place)
	if output == "" {
		output = "Location not found"
	}

	return Response{
		Title:    "📍 Location",
		Message:  fmt.Sprintf("## 📍 %s, %s\n\n%s", lat, lon, output),
		Priority: 3,
		Markdown: true,
	}
}

// reverseGeocode asks Nominatim for the place at latitude, longitude. Errors
// never include the request URL, since it carries the coordinates.
func (b *Builtins) reverseGeocode(ctx context.Context, latitude, longitude float64) (nominatimPlace, error) {
	query := url.Values{}
	query.Set("format", "json")
	query.Set("lat", formatCoordinate(latitude))
	query.Set("lon", formatCoordinate(longitude))
	query.Set("zoom", "18")
	query.Set("addressdetails", "1")

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.nominatimURL+"?"+query.Encode(), nil)
	if err != nil {
		return nominatimPlace{}, errors.New("could not build the geocoding request")
	}

	// Nominatim's usage policy asks for a User-Agent naming the application.
	request.Header.Set("User-Agent", "gotify-commander/"+Version+" (+https://github.com/drolosoft/gotify-commander)")

	response, err := b.httpClient.Do(request)
	if err != nil {
		var urlError *url.Error
		if errors.As(err, &urlError) {
			err = urlError.Err
		}
		return nominatimPlace{}, fmt.Errorf("geocoding request failed: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nominatimPlace{}, fmt.Errorf("geocoding service answered %s", response.Status)
	}

	var place nominatimPlace
	if err := json.NewDecoder(io.LimitReader(response.Body, maxGeocodeBody)).Decode(&place); err != nil {
		return nominatimPlace{}, fmt.Errorf("could not read the geocoding answer: %w", err)
	}
	return place, nil
}

// formatPlace renders a Nominatim place as the markdown message, line for
// line as the former python script did. It returns "" when Nominatim found
// no place or sent coordinates that are not numbers.
func formatPlace(place nominatimPlace) string {
	if place.Error != "" {
		return ""
	}

	latitude, latitudeOK := parseCoordinate(place.Lat, 90)
	longitude, longitudeOK := parseCoordinate(place.Lon, 180)
	if !latitudeOK || !longitudeOK {
		return ""
	}
	lt := formatCoordinate(latitude)
	ln := formatCoordinate(longitude)

	// Every address part comes from OSM contributors, so each one is escaped
	// once here and used escaped below.
	address := make(map[string]string, len(place.Address))
	for key, value := range place.Address {
		address[key] = escapeMarkdown(value)
	}

	road := address["road"]
	houseNumber := address["house_number"]
	neighbourhood := address["neighbourhood"]
	province := address["province"]
	state := address["state"]
	postcode := address["postcode"]
	country := address["country"]
	countryCode := address["country_code"]
	city := address["city"]
	if city == "" {
		city = address["town"]
	}

	var addressParts []string
	if road != "" {
		street := road
		if houseNumber != "" {
			street = houseNumber + " " + road
		}
		addressParts = append(addressParts, street)
	}
	for _, part := range []string{neighbourhood, city, province, state, postcode, country} {
		if part != "" {
			addressParts = append(addressParts, part)
		}
	}

	var out []string
	out = append(out, "**Address:** "+strings.Join(addressParts, ", "))
	out = append(out, "**Coordinates:** "+lt+", "+ln)

	// Each detail line appears only when OSM has that part, in this order.
	details := []struct {
		label string
		value string
	}{
		{"Street", strings.TrimSpace(houseNumber + " " + road)},
		{"Neighbourhood", neighbourhood},
		{"City", city},
		{"Province", province},
		{"State", state},
		{"Postal code", postcode},
		{"Country", country},
		{"Country code", strings.ToUpper(countryCode)},
	}
	for _, detail := range details {
		if detail.label == "Street" && road == "" {
			continue
		}
		if detail.value != "" {
			out = append(out, "**"+detail.label+":** "+detail.value)
		}
	}

	mapURL := "https://www.openstreetmap.org/?mlat=" + lt + "&mlon=" + ln + "&zoom=17"
	out = append(out, "")
	out = append(out, "[Open in map]("+mapURL+")")

	out = append(out, "")
	out = append(out, "![map]("+tileURL(latitude, longitude)+")")

	return strings.Join(out, "\n")
}

// tileURL returns the zoom 15 OpenStreetMap tile that contains the point,
// using the standard Web Mercator tile formula.
func tileURL(latitude, longitude float64) string {
	const zoom = 15

	tiles := math.Exp2(zoom)
	radians := latitude * math.Pi / 180
	xTile := int((longitude + 180.0) / 360.0 * tiles)
	yTile := int((1.0 - math.Log(math.Tan(radians)+1.0/math.Cos(radians))/math.Pi) / 2.0 * tiles)

	return fmt.Sprintf("https://tile.openstreetmap.org/%d/%d/%d.png", zoom, xTile, yTile)
}
