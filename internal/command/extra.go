package command

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

// ─── Operations: failed, health, docker, digest ─────────────────────────────

// healthWarnPercent is the disk and memory usage that turns a health line
// into a warning. At 85 % there is still room to act before a full disk
// stops the databases.
const healthWarnPercent = 85

// certWarnWindow is how close to its expiry a certificate is reported.
// certbot renews 30 days ahead, so a certificate inside 14 days means the
// renewal has been failing for two weeks.
const certWarnWindow = 14 * 24 * time.Hour

// healthOKPriority and healthWarnPriority are the priorities of a health
// summary without and with a warning, so a phone can be set to ring only
// for the second.
const (
	healthOKPriority   = 3
	healthWarnPriority = 5
)

// healthTitle and digestTitle head the health summary when it is asked for
// and when it is the daily digest.
const (
	healthTitle = "🩺 Health"
	digestTitle = "📰 Digest"
)

// systemctlFailedArgs lists the failed units without the header, the footer
// and the bullet, one unit per line.
var systemctlFailedArgs = []string{"--failed", "--no-legend", "--plain"}

// dockerLookupScript asks the shell whether docker is on PATH. command is a
// shell builtin, not a binary, so it needs sh; the script is a constant and
// no user text ever reaches it.
const dockerLookupScript = "command -v docker"

// dockerPsFormat prints one container per line as "name status". A
// container name has no spaces, so the first space splits the two.
const dockerPsFormat = "{{.Names}} {{.Status}}"

// operations holds the state the operations commands need on top of
// Builtins: the docker lookup cache and what tests replace.
type operations struct {
	builtins *Builtins

	// cpus is what the load average is compared with, and now is the clock
	// certificate expiry is measured against.
	cpus int
	now  func() time.Time

	// dockerChecked and dockerFound cache the PATH lookup: docker does not
	// come and go between two commands, and the lookup costs a process.
	dockerMu      sync.Mutex
	dockerChecked bool
	dockerFound   bool
}

// newOperations returns operations over b with the host's CPU count and the
// real clock.
func newOperations(b *Builtins) *operations {
	return &operations{
		builtins: b,
		cpus:     runtime.NumCPU(),
		now:      time.Now,
	}
}

// RegisterExtras registers the operations commands (failed, health, docker,
// digest) in r. Registering runs nothing, so CommandNames can call it with a
// nil executor.
func (b *Builtins) RegisterExtras(r *Registry) {
	ops := newOperations(b)

	r.Register(ActionFailed, ops.Failed)
	r.Register(ActionHealth, ops.Health)
	r.Register(ActionDigest, ops.DigestNow)
	r.Register(ActionDocker, ops.DockerUsage)
	r.Register(ActionDockerPs, ops.DockerPs)
	r.Register(ActionDockerRestart, ops.DockerRestart)
	r.Register(ActionDockerLogs, ops.DockerLogs)
}

// DangerousActions returns the operations actions that must share the
// commander's danger slot and ask for confirmation: restarting a container
// cuts what it serves, while reading its state or its logs does not.
func DangerousActions() []Action {
	return []Action{ActionDockerRestart}
}

// ExtraHelpLines returns the rows the help table gets for the operations
// commands, in the same "| `command` | description |" form.
func ExtraHelpLines() []string {
	return []string{
		"| `failed [vps/mac]` | 🧯 Failed systemd units |",
		"| `health` | 🩺 Disk, load, memory, units, certs |",
		"| `docker ps/restart/logs <name>` | 🐳 Allowed containers |",
		"| `digest` | 📰 Health digest now |",
	}
}

// parseExtra parses the operations verbs. It returns false for any other
// verb, so Parse goes on with the built-in ones. args are already lower
// case.
func parseExtra(verb string, args []string, raw string) (Command, bool) {
	first := ""
	if len(args) > 0 {
		first = args[0]
	}

	switch verb {
	case "failed":
		target := first
		if target == "" {
			target = "vps"
		}
		return Command{Action: ActionFailed, Target: target, Raw: raw}, true

	case "health", "digest":
		return Command{Action: Action(verb), Raw: raw}, true

	case "docker-ps", "docker-restart", "docker-logs":
		return Command{Action: Action(verb), Target: first, Raw: raw}, true

	case "docker":
		return parseDocker(args, raw), true
	}

	return Command{}, false
}

// parseDocker maps "docker <subcommand> [name]" to one action per
// subcommand. An unknown or missing subcommand becomes ActionDocker, which
// answers the usage.
func parseDocker(args []string, raw string) Command {
	if len(args) == 0 {
		return Command{Action: ActionDocker, Raw: raw}
	}

	name := ""
	if len(args) > 1 {
		name = args[1]
	}

	switch args[0] {
	case "ps":
		return Command{Action: ActionDockerPs, Target: name, Raw: raw}
	case "restart":
		return Command{Action: ActionDockerRestart, Target: name, Raw: raw}
	case "logs", "log":
		return Command{Action: ActionDockerLogs, Target: name, Raw: raw}
	}

	return Command{Action: ActionDocker, Target: args[0], Raw: raw}
}

// machine returns the executor and the display name for a failed target.
// "mac" without an SSH target is an error: execForTarget would quietly run
// on the VPS and report its units as the Mac's.
func (o *operations) machine(target string) (executor.Executor, string, error) {
	switch strings.ToLower(target) {
	case "", "vps":
		return o.builtins.exec, "VPS", nil
	case "mac":
		if o.builtins.sshExec == nil {
			return nil, "", errors.New("the mac target is not configured (ssh_targets.mac)")
		}
		return o.builtins.sshExec, "Mac", nil
	}

	return nil, "", fmt.Errorf("unknown target %q. Use 'vps' or 'mac'.", target)
}

// ─── failed ─────────────────────────────────────────────────────────────────

// failedUnit is one line of "systemctl --failed".
type failedUnit struct {
	Name        string
	Description string
}

// parseFailedUnits reads "systemctl --failed --no-legend --plain": UNIT LOAD
// ACTIVE SUB DESCRIPTION. Lines with fewer than four columns are messages,
// not units.
func parseFailedUnits(output string) []failedUnit {
	var units []failedUnit

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)

		// systemd before 248 printed the bullet even with --plain.
		if len(fields) > 0 && (fields[0] == "●" || fields[0] == "*") {
			fields = fields[1:]
		}
		if len(fields) < 4 {
			continue
		}

		units = append(units, failedUnit{
			Name:        fields[0],
			Description: strings.Join(fields[4:], " "),
		})
	}

	return units
}

// failedUnits runs systemctl on runner and parses its answer.
func failedUnits(ctx context.Context, runner executor.Executor) ([]failedUnit, error) {
	result, err := runner.Run(ctx, "systemctl", systemctlFailedArgs...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("systemctl exited with %d: %s", result.ExitCode, strings.TrimSpace(result.Output))
	}

	return parseFailedUnits(result.Output), nil
}

// isDarwin reports whether runner is a macOS machine. A target that cannot
// answer uname counts as not macOS, and systemctl then reports the error.
func isDarwin(ctx context.Context, runner executor.Executor) bool {
	result, err := runner.Run(ctx, "uname", "-s")
	if err != nil {
		return false
	}
	return strings.TrimSpace(result.Output) == "Darwin"
}

// Failed lists the failed systemd units of the VPS, or of the mac SSH target
// when that machine runs Linux.
func (o *operations) Failed(cmd Command) Response {
	runner, label, err := o.machine(cmd.Target)
	if err != nil {
		return errorResponse("Failed Units", err.Error())
	}

	ctx, cancel := o.builtins.timeout()
	defer cancel()

	// The SSH target is named mac but may be any machine; only a real macOS
	// lacks systemd. The VPS is the Linux host Gotify runs on.
	if label == "Mac" && isDarwin(ctx, runner) {
		return Response{Title: "🧯 Failed Units", Message: "🧯 Mac: not available on macOS", Priority: 3}
	}

	units, err := failedUnits(ctx, runner)
	if err != nil {
		return errorResponse("Failed Units", err.Error())
	}

	if len(units) == 0 {
		return Response{Title: "🧯 Failed Units", Message: "✅ No failed units", Priority: 3}
	}

	lines := []string{fmt.Sprintf("🧯 %s Failed Units (%d)", label, len(units))}
	for _, unit := range units {
		line := "- " + unit.Name
		if unit.Description != "" {
			line += ": " + unit.Description
		}
		lines = append(lines, line)
	}

	return Response{Title: "🧯 Failed Units", Message: strings.Join(lines, "\n"), Priority: 5}
}

// ─── health ─────────────────────────────────────────────────────────────────

// healthLine is one line of the health summary.
type healthLine struct {
	ok   bool
	text string
}

// String returns the line with its marker.
func (h healthLine) String() string {
	if h.ok {
		return "✅ " + h.text
	}
	return "⚠️ " + h.text
}

// diskUsage is the capacity of one mounted filesystem.
type diskUsage struct {
	Mount   string
	Percent int
}

// pseudoFilesystems are df sources that are memory or containers, not disks
// that can fill up.
var pseudoFilesystems = map[string]bool{
	"tmpfs":    true,
	"devtmpfs": true,
	"udev":     true,
	"overlay":  true,
	"shm":      true,
	"none":     true,
	"efivarfs": true,
}

// parseDf reads "df -P": Filesystem, 1024-blocks, Used, Available, Capacity,
// Mounted on. The mount point is the rest of the line, so it may contain
// spaces. Pseudo filesystems and snap loop devices are left out: a snap is a
// read-only image that is always 100 % full.
func parseDf(output string) []diskUsage {
	var mounts []diskUsage

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}

		source := fields[0]
		if pseudoFilesystems[source] || strings.HasPrefix(source, "/dev/loop") {
			continue
		}

		// The header has "Capacity" here, so it fails the number check.
		percent, err := strconv.Atoi(strings.TrimSuffix(fields[4], "%"))
		if err != nil {
			continue
		}

		mount := strings.Join(fields[5:], " ")
		if strings.HasPrefix(mount, "/snap/") {
			continue
		}

		mounts = append(mounts, diskUsage{Mount: mount, Percent: percent})
	}

	return mounts
}

// parseFree reads "free -m" and returns the total and used memory in MB.
// Used is total minus available, since buffers and cache are given back on
// demand. An older free without the available column falls back to its used
// column.
func parseFree(output string) (total, used int, err error) {
	lines := strings.Split(output, "\n")
	header := strings.Fields(lines[0])

	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] != "Mem:" {
			continue
		}

		total, err = strconv.Atoi(fields[1])
		if err != nil {
			return 0, 0, fmt.Errorf("free total %q: %w", fields[1], err)
		}

		// The header has no label column, so column i of the header is
		// field i+1 of the Mem: line.
		available := slices.Index(header, "available")
		if available >= 0 && available+1 < len(fields) {
			free, err := strconv.Atoi(fields[available+1])
			if err != nil {
				return 0, 0, fmt.Errorf("free available %q: %w", fields[available+1], err)
			}
			return total, total - free, nil
		}

		used, err = strconv.Atoi(fields[2])
		if err != nil {
			return 0, 0, fmt.Errorf("free used %q: %w", fields[2], err)
		}
		return total, used, nil
	}

	return 0, 0, errors.New("no Mem: line in free output")
}

// parseLoadavg reads the 1, 5 and 15 minute load averages from /proc/loadavg.
func parseLoadavg(output string) ([3]float64, error) {
	var averages [3]float64

	fields := strings.Fields(output)
	if len(fields) < 3 {
		return averages, fmt.Errorf("loadavg %q: want three averages", strings.TrimSpace(output))
	}

	for position := range averages {
		value, err := strconv.ParseFloat(fields[position], 64)
		if err != nil {
			return averages, fmt.Errorf("loadavg %q: %w", fields[position], err)
		}
		averages[position] = value
	}

	return averages, nil
}

// certDateLayout is how "openssl x509 -enddate" prints notAfter, with the day
// padded by a space: "Jan  2 03:04:05 2027 GMT".
const certDateLayout = "Jan _2 15:04:05 2006 MST"

// parseCertDate parses openssl's notAfter date.
func parseCertDate(text string) (time.Time, error) {
	return time.Parse(certDateLayout, strings.TrimSpace(text))
}

// diskHealth warns about every mount at healthWarnPercent or more.
func (o *operations) diskHealth(ctx context.Context, runner executor.Executor) []healthLine {
	result, err := runner.Run(ctx, "df", "-P")
	if err != nil {
		return []healthLine{{text: "Disk: unavailable (" + err.Error() + ")"}}
	}

	mounts := parseDf(result.Output)
	if len(mounts) == 0 {
		return []healthLine{{text: "Disk: unavailable (no mounts in df output)"}}
	}

	var lines []healthLine
	for _, mount := range mounts {
		if mount.Percent >= healthWarnPercent {
			lines = append(lines, healthLine{text: fmt.Sprintf("Disk %s: %d %% used", mount.Mount, mount.Percent)})
		}
	}

	if len(lines) == 0 {
		return []healthLine{{ok: true, text: fmt.Sprintf("Disk: every mount under %d %%", healthWarnPercent)}}
	}
	return lines
}

// loadHealth warns when the 5 minute load is above the CPU count: more
// processes want to run than there are CPUs, for minutes, not for a spike.
func (o *operations) loadHealth(ctx context.Context, runner executor.Executor) []healthLine {
	result, err := runner.Run(ctx, "cat", "/proc/loadavg")
	if err != nil {
		return []healthLine{{text: "Load: unavailable (" + err.Error() + ")"}}
	}

	averages, err := parseLoadavg(result.Output)
	if err != nil {
		return []healthLine{{text: "Load: unavailable (" + err.Error() + ")"}}
	}

	text := fmt.Sprintf("Load: %.2f %.2f %.2f (%d CPUs)", averages[0], averages[1], averages[2], o.cpus)
	return []healthLine{{ok: averages[1] <= float64(o.cpus), text: text}}
}

// memoryHealth warns when the memory in use reaches healthWarnPercent.
func (o *operations) memoryHealth(ctx context.Context, runner executor.Executor) []healthLine {
	result, err := runner.Run(ctx, "free", "-m")
	if err != nil {
		return []healthLine{{text: "Memory: unavailable (" + err.Error() + ")"}}
	}

	total, used, err := parseFree(result.Output)
	if err != nil || total == 0 {
		return []healthLine{{text: "Memory: unavailable (no total in free output)"}}
	}

	percent := used * 100 / total
	text := fmt.Sprintf("Memory: %d of %d MB used (%d %%)", used, total, percent)
	return []healthLine{{ok: percent < healthWarnPercent, text: text}}
}

// unitsHealth warns about any failed unit and names them.
func (o *operations) unitsHealth(ctx context.Context, runner executor.Executor) []healthLine {
	units, err := failedUnits(ctx, runner)
	if err != nil {
		return []healthLine{{text: "Failed units: unavailable (" + err.Error() + ")"}}
	}

	if len(units) == 0 {
		return []healthLine{{ok: true, text: "Failed units: none"}}
	}

	names := make([]string, 0, len(units))
	for _, unit := range units {
		names = append(names, unit.Name)
	}

	text := fmt.Sprintf("Failed units: %d (%s)", len(units), strings.Join(names, ", "))
	return []healthLine{{text: text}}
}

// certsHealth warns about every certificate that expires within
// certWarnWindow. It runs find and openssl with the same arguments as the
// certs command, one openssl per certificate and no shell in between.
func (o *operations) certsHealth(ctx context.Context, runner executor.Executor) []healthLine {
	result, err := runner.Run(ctx, "sudo", "find", letsencryptLive, "-name", "cert.pem")
	if err != nil {
		return []healthLine{{text: "Certificates: unavailable (" + err.Error() + ")"}}
	}

	// Same filter as certs: find's complaints arrive mixed with the paths.
	var paths []string
	for _, line := range strings.Split(result.Output, "\n") {
		if strings.HasPrefix(line, letsencryptLive+"/") && strings.HasSuffix(line, "/cert.pem") {
			paths = append(paths, line)
		}
	}
	sort.Strings(paths)

	if len(paths) == 0 {
		return []healthLine{{ok: true, text: "Certificates: none found"}}
	}

	now := o.now()
	var lines []healthLine

	for _, path := range paths {
		domain := filepath.Base(filepath.Dir(path))

		expiryText, err := o.builtins.certExpiry(ctx, path)
		if err != nil {
			lines = append(lines, healthLine{text: "Certificate " + domain + ": unreadable (" + err.Error() + ")"})
			continue
		}

		expiry, err := parseCertDate(expiryText)
		if err != nil {
			lines = append(lines, healthLine{text: "Certificate " + domain + ": unreadable expiry"})
			continue
		}

		remaining := expiry.Sub(now)
		switch {
		case remaining < 0:
			lines = append(lines, healthLine{text: "Certificate " + domain + " expired on " + expiryText})
		case remaining < certWarnWindow:
			days := int(remaining.Hours() / 24)
			lines = append(lines, healthLine{text: fmt.Sprintf("Certificate %s expires in %d days", domain, days)})
		}
	}

	if len(lines) == 0 {
		text := fmt.Sprintf("Certificates: %d, none expiring within 14 days", len(paths))
		return []healthLine{{ok: true, text: text}}
	}
	return lines
}

// healthReport runs every check on the local machine, one after another
// under one timeout, and puts their lines under title. A check that fails
// marks its own line and the others still run.
func (o *operations) healthReport(title string) Response {
	ctx, cancel := o.builtins.timeout()
	defer cancel()

	runner := o.builtins.exec
	checks := []func(context.Context, executor.Executor) []healthLine{
		o.diskHealth,
		o.loadHealth,
		o.memoryHealth,
		o.unitsHealth,
		o.certsHealth,
	}

	lines := []string{healthTitle}
	priority := healthOKPriority

	for _, check := range checks {
		for _, line := range check(ctx, runner) {
			if !line.ok {
				priority = healthWarnPriority
			}
			lines = append(lines, line.String())
		}
	}

	return Response{Title: title, Message: strings.Join(lines, "\n"), Priority: priority}
}

// Health sends the one-message summary of the VPS: disk, load, memory,
// failed units and certificates.
func (o *operations) Health(cmd Command) Response {
	return o.healthReport(healthTitle)
}

// DigestNow sends the health summary right now, under the digest title.
func (o *operations) DigestNow(cmd Command) Response {
	return o.healthReport(digestTitle)
}

// ─── docker ─────────────────────────────────────────────────────────────────

// dockerUsage is the answer to a docker command without a known subcommand
// or without the name it needs.
const dockerUsage = "Usage: docker ps|restart|logs <name>"

// allowedContainer returns the configured spelling of name when it is in
// docker.allowed_containers. Parse lower-cases what the user types, and
// docker names are case-sensitive, so the argv always takes the
// configuration's spelling, never the user's text.
func (o *operations) allowedContainer(name string) (string, bool) {
	for _, allowed := range o.builtins.cfg.Docker.AllowedContainers {
		if strings.EqualFold(allowed, name) {
			return allowed, true
		}
	}
	return "", false
}

// dockerAvailable reports whether docker is on PATH. A definite answer is
// cached; a lookup that failed (a timeout) is tried again next time.
func (o *operations) dockerAvailable(ctx context.Context) bool {
	o.dockerMu.Lock()
	defer o.dockerMu.Unlock()

	if o.dockerChecked {
		return o.dockerFound
	}

	result, err := o.builtins.exec.Run(ctx, "sh", "-c", dockerLookupScript)
	if err != nil {
		return false
	}

	o.dockerChecked = true
	o.dockerFound = result.ExitCode == 0
	return o.dockerFound
}

// notAllowed is the answer for a container outside the allow-list.
func notAllowed(name string) Response {
	return Response{Title: "🚫 Docker", Message: "🚫 Container not allowed: " + name, Priority: 5}
}

// notAvailable is the answer when the host has no docker.
func notAvailable() Response {
	return Response{Title: "🐳 Docker", Message: "docker not available", Priority: 5}
}

// DockerUsage answers a bare docker or an unknown subcommand.
func (o *operations) DockerUsage(cmd Command) Response {
	return Response{Title: "🐳 Docker", Message: dockerUsage, Priority: 3}
}

// DockerPs shows the state of the allowed containers, or of the one named.
// Containers outside the allow-list are not shown at all.
func (o *operations) DockerPs(cmd Command) Response {
	names := o.builtins.cfg.Docker.AllowedContainers
	if len(names) == 0 {
		return Response{Title: "🚫 Docker", Message: "🚫 No containers allowed: list them in docker.allowed_containers", Priority: 5}
	}

	if cmd.Target != "" {
		name, ok := o.allowedContainer(cmd.Target)
		if !ok {
			return notAllowed(cmd.Target)
		}
		names = []string{name}
	}

	ctx, cancel := o.builtins.timeout()
	defer cancel()

	if !o.dockerAvailable(ctx) {
		return notAvailable()
	}

	result, err := o.builtins.exec.Run(ctx, "docker", "ps", "--all", "--format", dockerPsFormat)
	if err != nil {
		return errorResponse("Docker Failed", err.Error())
	}
	if result.ExitCode != 0 {
		return errorResponse("Docker Failed", strings.TrimSpace(result.Output))
	}

	statuses := make(map[string]string)
	for _, line := range strings.Split(result.Output, "\n") {
		name, status, found := strings.Cut(strings.TrimSpace(line), " ")
		if found {
			statuses[name] = status
		}
	}

	// The allow-list order is the order the user chose; a container that
	// docker does not know is still listed, so a typo in the list shows.
	lines := []string{"🐳 Containers"}
	for _, name := range names {
		status, ok := statuses[name]
		if !ok {
			status = "not found"
		}
		lines = append(lines, "- "+name+": "+status)
	}

	return Response{Title: "🐳 Docker", Message: strings.Join(lines, "\n"), Priority: 3}
}

// DockerRestart restarts one allowed container. It is in DangerousActions,
// so it shares the commander's danger slot.
func (o *operations) DockerRestart(cmd Command) Response {
	if cmd.Target == "" {
		return errorResponse("Docker", dockerUsage)
	}

	name, ok := o.allowedContainer(cmd.Target)
	if !ok {
		return notAllowed(cmd.Target)
	}

	ctx, cancel := o.builtins.timeout()
	defer cancel()

	if !o.dockerAvailable(ctx) {
		return notAvailable()
	}

	result, err := o.builtins.exec.Run(ctx, "docker", "restart", name)
	if err != nil {
		return errorResponse("Docker Restart Failed", fmt.Sprintf("Failed to restart %s\n%s", name, err.Error()))
	}
	if result.ExitCode != 0 {
		return errorResponse("Docker Restart Failed", fmt.Sprintf("Failed to restart %s\n%s", name, strings.TrimSpace(result.Output)))
	}

	return Response{Title: "🐳 Docker", Message: "🔄 Restarted container " + name, Priority: 5}
}

// DockerLogs shows the last log lines of one allowed container, as many as
// logs <service> shows.
func (o *operations) DockerLogs(cmd Command) Response {
	if cmd.Target == "" {
		return errorResponse("Docker", dockerUsage)
	}

	name, ok := o.allowedContainer(cmd.Target)
	if !ok {
		return notAllowed(cmd.Target)
	}

	lines := o.builtins.cfg.Defaults.LogLines
	if lines == 0 {
		lines = 30
	}

	ctx, cancel := o.builtins.timeout()
	defer cancel()

	if !o.dockerAvailable(ctx) {
		return notAvailable()
	}

	result, err := o.builtins.exec.Run(ctx, "docker", "logs", "--tail", strconv.Itoa(lines), name)
	if err != nil {
		return errorResponse("Docker Logs Failed", fmt.Sprintf("failed to fetch logs for %s: %s", name, err.Error()))
	}

	return Response{
		Title:    fmt.Sprintf("🐳 %s Logs", name),
		Message:  fmt.Sprintf("🐳 %s Logs\n%s", name, result.Output),
		Priority: 3,
	}
}

// ─── Daily digest ───────────────────────────────────────────────────────────

// maxDigestWait is the longest the digest sleeps before it looks at the
// clock again. A timer counts elapsed time, not wall time, so a clock that
// was corrected or a summer time change would otherwise move the send by up
// to a day.
const maxDigestWait = time.Hour

// DigestSender delivers one digest message. The plugin passes a function
// over its Gotify message sender.
type DigestSender func(Response)

// Digest sends a summary once a day at a configured local time. Its owner
// calls Start once the plugin is enabled and Stop when it is disabled; the
// goroutine also ends when the context given to Start is cancelled.
type Digest struct {
	enabled  bool
	hour     int
	minute   int
	location *time.Location

	summary func() Response
	send    DigestSender

	// now and after are the clock; tests replace them with one they move
	// by hand.
	now   func() time.Time
	after func(time.Duration) <-chan time.Time

	// cancel and done belong to the running goroutine: cancel asks it to
	// stop and done is closed when it has.
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// NewDigest checks settings and returns a stopped Digest that, once started,
// sends summary() through send every day at settings.At. A disabled digest
// is still returned, and its Start does nothing.
func NewDigest(settings config.DigestConfig, summary func() Response, send DigestSender) (*Digest, error) {
	at := settings.At
	if at == "" {
		at = config.DefaultDigestAt
	}

	clock, err := time.Parse("15:04", at)
	if err != nil {
		return nil, fmt.Errorf("digest.at must be a time like 07:30, got %q", settings.At)
	}

	location := time.Local
	if settings.Timezone != "" {
		location, err = time.LoadLocation(settings.Timezone)
		if err != nil {
			return nil, fmt.Errorf("digest.timezone %q: %w", settings.Timezone, err)
		}
	}

	digest := &Digest{
		enabled:  settings.Enabled,
		hour:     clock.Hour(),
		minute:   clock.Minute(),
		location: location,
		summary:  summary,
		send:     send,
		now:      time.Now,
		after:    time.After,
	}
	return digest, nil
}

// Start runs the schedule in a goroutine and returns at once. It does
// nothing when the digest is disabled or already running.
func (d *Digest) Start(ctx context.Context) {
	if !d.enabled {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// A goroutine that ended because its context was cancelled may be
	// replaced; one that still runs may not.
	if d.done != nil {
		select {
		case <-d.done:
		default:
			return
		}
	}

	runContext, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	d.cancel = cancel
	d.done = done

	go d.run(runContext, done)
}

// Stop ends the goroutine and waits for it, including a send in progress.
// It is safe to call when the digest never started or already stopped.
func (d *Digest) Stop() {
	d.mu.Lock()
	cancel := d.cancel
	done := d.done
	d.cancel = nil
	d.mu.Unlock()

	if cancel == nil {
		return
	}

	cancel()
	<-done
}

// run sleeps until the next run, sends, and starts over, until ctx ends.
// It wakes at least every maxDigestWait and sends only when the wall clock
// has reached the planned time, so an early timer never sends twice.
func (d *Digest) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	next := d.nextRun(d.now())

	for {
		wait := min(next.Sub(d.now()), maxDigestWait)

		select {
		case <-ctx.Done():
			return
		case <-d.after(wait):
		}

		now := d.now()
		if now.Before(next) {
			continue
		}

		// The context may have ended while the timer fired.
		if ctx.Err() != nil {
			return
		}

		d.fire()

		// Planning from now, not from next, sends one digest after a
		// machine that slept through several days, not one per missed day.
		next = d.nextRun(now)
	}
}

// fire builds and sends one digest. A panic in either is logged and the
// schedule goes on: one bad morning must not stop the next ones.
func (d *Digest) fire() {
	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("[digest] panic sending the digest: %v\n%s", recovered, debug.Stack())
		}
	}()

	d.send(d.summary())
}

// nextRun returns the first moment after now, strictly, at the digest's hour
// and minute in its timezone. time.Date normalises the day after the last
// of the month and the summer time changes.
func (d *Digest) nextRun(now time.Time) time.Time {
	local := now.In(d.location)
	today := time.Date(local.Year(), local.Month(), local.Day(), d.hour, d.minute, 0, 0, d.location)

	if today.After(now) {
		return today
	}
	return time.Date(local.Year(), local.Month(), local.Day()+1, d.hour, d.minute, 0, 0, d.location)
}
