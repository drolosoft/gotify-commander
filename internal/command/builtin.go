package command

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
	"github.com/drolosoft/gotify-commander/internal/security"
)

// Builtins holds dependencies for all built-in command handlers.
type Builtins struct {
	cfg     *config.Config
	exec    executor.Executor
	sshExec executor.Executor // nil until SSH wired in Task 12
}

// NewBuiltins creates a Builtins instance with the given config and executor.
func NewBuiltins(cfg *config.Config, exec executor.Executor) *Builtins {
	return &Builtins{cfg: cfg, exec: exec}
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
		return Response{
			Title:    b.title(action) + " " + b.title(name),
			Message:  fmt.Sprintf("%s %s %sed on VPS%s", emoji, name, action, portStr),
			Priority: 3,
		}

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
		return Response{
			Title:    b.title(action) + " " + b.title(name),
			Message:  fmt.Sprintf("%s %s %sed on Mac%s", emoji, name, action, portStr),
			Priority: 3,
		}
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

func (b *Builtins) Certs(cmd Command) Response {
	ctx, cancel := b.timeout()
	defer cancel()

	script := `sudo find /etc/letsencrypt/live -name cert.pem 2>/dev/null | sort | while read d; do domain=$(basename $(dirname "$d")); expiry=$(sudo openssl x509 -enddate -noout -in "$d" 2>/dev/null | cut -d= -f2); echo "$domain: $expiry"; done`
	result, err := b.exec.Run(ctx, "bash", "-c", script)
	if err != nil {
		return errorResponse("Certs Failed", err.Error())
	}

	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "No certificates found in /etc/letsencrypt/live/"
	}

	return Response{
		Title:    "🔒 SSL Certificates",
		Message:  fmt.Sprintf("🔒 SSL Certificates\n%s", output),
		Priority: 3,
	}
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
		// Per-site traffic: resolve service name to domain
		domain := cmd.Target
		if svc, ok := b.cfg.Services[cmd.Target]; ok && svc.Domain != "" {
			domain = svc.Domain
		}
		// Before using domain in the shell script, validate it
		if err := security.ValidateInput(domain); err != nil {
			return errorResponse("Traffic Failed", "invalid service name: "+err.Error())
		}
		// Filter logs for this domain into temp file, then run rhit on it
		script := fmt.Sprintf(
			"tmpf=$(mktemp /tmp/rhit-XXXXXX.log) && sudo zcat -f /var/log/nginx/access.log* 2>/dev/null | grep -i '%s' > \"$tmpf\" && sudo rhit \"$tmpf\" --color no -d '%s' --length 10 2>&1 | sed 's/\\x1b\\[[0-9;]*m//g' | grep -v '^$' | grep -v '\\[2K'; rm -f \"$tmpf\"",
			domain, dateRange)
		result, err := b.exec.Run(ctx, "bash", "-c", script)
		if err != nil {
			return errorResponse("Traffic Failed", err.Error())
		}
		output := result.Output
		if output == "" || strings.Contains(output, "0 hits") {
			output = "No traffic found for " + domain
		}
		return Response{
			Title:   fmt.Sprintf("📈 %s Traffic", domain),
			Message: fmt.Sprintf("📈 %s (last 3 months)\n%s", domain, output),
			Priority: 3,
		}
	}

	// All traffic, last 3 months
	result, err := b.exec.Run(ctx, "bash", "-c", "sudo rhit /var/log/nginx/ --color no -d '"+dateRange+"' --length 15 2>&1 | sed 's/\\x1b\\[[0-9;]*m//g'")
	if err != nil {
		return errorResponse("Traffic Failed", err.Error())
	}
	return Response{
		Title:    "📈 Traffic",
		Message:  fmt.Sprintf("📈 All Traffic (last 3 months)\n%s", result.Output),
		Priority: 3,
	}
}

// ─── 22. Analytics ──────────────────────────────────────────────────────────

func (b *Builtins) Analytics(cmd Command) Response {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// GoAccess CSV → parse with python for reliable extraction
	script := `sudo goaccess /var/log/nginx/access.log --log-format=COMBINED --no-term-resolver --no-color --output-format csv -o - 2>/dev/null | python3 -c "
import sys, csv
reader = csv.reader(sys.stdin)
out = []
for row in reader:
    if len(row) >= 12 and row[2] == 'general':
        k, v = row[11], row[10]
        if k == 'date_time': out.append('Generated: ' + v)
        elif k == 'total_requests': out.append('Total requests: ' + v)
        elif k == 'unique_visitors': out.append('Unique visitors: ' + v)
        elif k == 'unique_files': out.append('Unique pages: ' + v)
        elif k == 'bandwidth': out.append('Bandwidth: ' + v + ' bytes')
        elif k == 'unique_not_found': out.append('404 errors: ' + v)
print(chr(10).join(out))
"`
	result, err := b.exec.Run(ctx, "bash", "-c", script)
	if err != nil {
		return errorResponse("Analytics Failed", err.Error())
	}

	output := strings.TrimSpace(result.Output)
	if output == "" {
		output = "No analytics data available. Is goaccess installed?"
	}

	return Response{
		Title:    "📊 Analytics",
		Message:  fmt.Sprintf("📊 Web Analytics (today)\n\n%s", output),
		Priority: 3,
	}
}

// ─── 23. Locate ─────────────────────────────────────────────────────────────

func (b *Builtins) Locate(cmd Command) Response {
	// Parse lat,lon from raw input: "locate 40.4168 -3.7038"
	parts := strings.Fields(cmd.Raw)
	if len(parts) < 3 {
		return errorResponse("Locate", "Usage: locate <lat> <lon>\nExample: locate 40.4168 -3.7038")
	}
	lat := parts[1]
	lon := parts[2]

	if _, err := strconv.ParseFloat(lat, 64); err != nil {
		return errorResponse("Locate", "invalid latitude: "+lat)
	}
	if _, err := strconv.ParseFloat(lon, 64); err != nil {
		return errorResponse("Locate", "invalid longitude: "+lon)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Use python3 to parse JSON properly — avoid fmt.Sprintf to prevent % conflicts
	apiURL := "https://nominatim.openstreetmap.org/reverse?format=json&lat=" + lat + "&lon=" + lon + "&zoom=18&addressdetails=1"
	pyScript := `import sys, json, math
d = json.load(sys.stdin)
a = d.get('address', {})
lt = str(d.get('lat',''))
ln = str(d.get('lon',''))
lines = []
if a.get('road'):
    r = a['road']
    if a.get('house_number'): r = a['house_number'] + ' ' + r
    lines.append(r)
if a.get('neighbourhood'): lines.append(a['neighbourhood'])
if a.get('town') or a.get('city'): lines.append(a.get('city', a.get('town', '')))
if a.get('province'): lines.append(a['province'])
if a.get('state'): lines.append(a['state'])
if a.get('postcode'): lines.append(a['postcode'])
if a.get('country'): lines.append(a['country'])
out = []
out.append('**Address:** ' + ', '.join(lines))
out.append('**Coordinates:** ' + lt + ', ' + ln)
if a.get('road'): out.append('**Street:** ' + (a.get('house_number','') + ' ' + a['road']).strip())
if a.get('neighbourhood'): out.append('**Neighbourhood:** ' + a['neighbourhood'])
if a.get('town') or a.get('city'): out.append('**City:** ' + a.get('city', a.get('town','')))
if a.get('province'): out.append('**Province:** ' + a['province'])
if a.get('state'): out.append('**State:** ' + a['state'])
if a.get('postcode'): out.append('**Postal code:** ' + a['postcode'])
if a.get('country'): out.append('**Country:** ' + a['country'])
if a.get('country_code'): out.append('**Country code:** ' + a['country_code'].upper())
mapurl = 'https://www.openstreetmap.org/?mlat=' + lt + '&mlon=' + ln + '&zoom=17'
out.append('')
out.append('[Open in map](' + mapurl + ')')
# Static map image via OSM embed
z = 15
lat_f = float(lt)
lon_f = float(ln)
n = 2 ** z
xtile = int((lon_f + 180.0) / 360.0 * n)
ytile = int((1.0 - math.log(math.tan(math.radians(lat_f)) + 1.0/math.cos(math.radians(lat_f))) / math.pi) / 2.0 * n)
tileurl = 'https://tile.openstreetmap.org/' + str(z) + '/' + str(xtile) + '/' + str(ytile) + '.png'
out.append('')
out.append('![map](' + tileurl + ')')
print(chr(10).join(out))
`
	script := `curl -s -H "User-Agent: gotify-commander/1.0" "` + apiURL + `" | python3 -c "` + pyScript + `"`

	result, err := b.exec.Run(ctx, "bash", "-c", script)
	if err != nil {
		return errorResponse("Locate Failed", err.Error())
	}

	output := strings.TrimSpace(result.Output)
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
