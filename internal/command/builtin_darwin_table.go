package command

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/drolosoft/gotify-commander/internal/config"
)

// machineCommand is one command a builtin runs on a machine and the function
// that turns its output into the text of the reply. A nil format sends the
// output as it came.
type machineCommand struct {
	argv   []string
	format func(output string) string
}

// text returns the reply text for output.
func (command machineCommand) text(output string) string {
	if command.format == nil {
		return output
	}
	return command.format(output)
}

// osCommands is what the builtins run on one operating system. The handlers
// take their argv from here instead of writing it out, so a Mac reached over
// SSH gets commands that exist on macOS and the VPS keeps the ones it always
// had.
type osCommands struct {
	memory      machineCommand
	processes   machineCommand
	ports       machineCommand
	connections machineCommand
	updates     machineCommand

	// addresses are tried in order. The first that exits 0 and prints
	// something answers; the last one answers whatever it prints.
	addresses []machineCommand
}

// linuxCommands are the commands every target ran before the table existed,
// with the same formatting, so the VPS does not change.
var linuxCommands = osCommands{
	memory:      machineCommand{argv: []string{"free", "-h"}},
	processes:   machineCommand{argv: []string{"ps", "aux", "--sort=-%cpu"}, format: firstLines(6)},
	ports:       machineCommand{argv: []string{"ss", "-tlnp"}},
	connections: machineCommand{argv: []string{"ss", "-s"}},
	updates:     machineCommand{argv: []string{"apt", "list", "--upgradable"}, format: formatAptUpgradable},
	addresses: []machineCommand{
		{argv: []string{"hostname", "-I"}, format: strings.TrimSpace},
	},
}

// darwinCommands are the macOS equivalents. free, ss, apt and journalctl do
// not exist there, and ps does not take GNU's --sort.
var darwinCommands = osCommands{
	memory: machineCommand{argv: []string{"vm_stat"}, format: formatVMStat},

	// -r sorts by CPU, highest first, which is what --sort=-%cpu does on
	// linux. The header and the top nine processes fit a phone screen.
	processes: machineCommand{argv: []string{"ps", "-r", "-A", "-o", "pid,pcpu,pmem,comm"}, format: firstLines(10)},

	// -iTCP and not -i: with a bare -i, lsof keeps every UDP socket next to
	// the listening TCP ones, because -sTCP:LISTEN only filters TCP.
	ports: machineCommand{argv: []string{"lsof", "-iTCP", "-sTCP:LISTEN", "-P", "-n"}},

	// netstat prints one line per socket; the count is done here, so no
	// "| wc -l" and no shell are needed.
	connections: machineCommand{argv: []string{"netstat", "-an", "-p", "tcp"}, format: formatNetstatCounts},

	updates: machineCommand{argv: []string{"softwareupdate", "-l"}, format: formatSoftwareUpdate},

	// en0 is the built-in Ethernet of a Mac mini, or the Wi-Fi of a
	// laptop. When it has no address, ifconfig lists the ones the other
	// interfaces have, Tailscale's included.
	addresses: []machineCommand{
		{argv: []string{"ipconfig", "getifaddr", "en0"}, format: strings.TrimSpace},
		{argv: []string{"ifconfig"}, format: formatIfconfigAddresses},
	},
}

// commandsFor returns the command table of the operating system named in an
// ssh_targets entry. Anything but darwin is linux: config.Validate refuses
// other values and fills in linux when os is left out, and a config built
// in a test without Validate leaves it empty.
func commandsFor(operatingSystem string) osCommands {
	if operatingSystem == config.OSDarwin {
		return darwinCommands
	}
	return linuxCommands
}

// firstLines returns a format that keeps the first count lines of an
// output, trailing newlines dropped.
func firstLines(count int) func(output string) string {
	return func(output string) string {
		lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
		if len(lines) > count {
			lines = lines[:count]
		}
		return strings.Join(lines, "\n")
	}
}

// formatAptUpgradable strips apt's "Listing..." header and blank lines, and
// says so when nothing is left.
func formatAptUpgradable(output string) string {
	var kept []string
	for _, line := range strings.Split(output, "\n") {
		if line != "" && !strings.HasPrefix(line, "Listing...") {
			kept = append(kept, line)
		}
	}

	text := strings.Join(kept, "\n")
	if strings.TrimSpace(text) == "" {
		return "System is up to date"
	}
	return text
}

// formatSoftwareUpdate strips the two progress lines softwareupdate prints
// before its answer, and turns "No new software available." into the same
// words the linux reply uses.
func formatSoftwareUpdate(output string) string {
	if strings.Contains(output, "No new software available") {
		return "System is up to date"
	}

	var kept []string
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || trimmed == "Software Update Tool" || trimmed == "Finding available software" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// vmStatPageSize finds the page size in vm_stat's first line, "Mach Virtual
// Memory Statistics: (page size of 16384 bytes)".
var vmStatPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// vmStatRows are the vm_stat rows of the summary and the label each one
// gets, in the order they are shown.
var vmStatRows = []struct {
	key   string
	label string
}{
	{"Pages free", "Free"},
	{"Pages active", "Active"},
	{"Pages inactive", "Inactive"},
	{"Pages wired down", "Wired"},
}

// parseVMStat turns vm_stat's page counts into "Free: 1543 MiB" lines. It
// reports false when the page size header or any of the rows is missing:
// pages are 4 KiB on Intel Macs and 16 KiB on Apple silicon, so a guessed
// size would be wrong by four on one of them.
func parseVMStat(output string) (string, bool) {
	match := vmStatPageSize.FindStringSubmatch(output)
	if match == nil {
		return "", false
	}
	pageSize, err := strconv.ParseUint(match[1], 10, 64)
	if err != nil {
		return "", false
	}

	// Each row is "Pages free:     98806." with the count ending in a dot.
	pages := make(map[string]uint64)
	for _, line := range strings.Split(output, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}

		count, err := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(value), "."), 10, 64)
		if err != nil {
			continue
		}
		pages[strings.TrimSpace(key)] = count
	}

	var lines []string
	for _, row := range vmStatRows {
		count, found := pages[row.key]
		if !found {
			return "", false
		}

		mebibytes := count * pageSize / (1 << 20)
		lines = append(lines, fmt.Sprintf("%s: %d MiB", row.label, mebibytes))
	}
	return strings.Join(lines, "\n"), true
}

// formatVMStat is the memory reply of a Mac: the vm_stat summary, or what
// vm_stat printed when it cannot be read, so an error still reaches the
// user.
func formatVMStat(output string) string {
	if summary, ok := parseVMStat(output); ok {
		return summary
	}
	return output
}

// formatNetstatCounts counts the established and listening TCP sockets in
// "netstat -an -p tcp" output. The state is the last column of every
// socket line, IPv4 (tcp4) and IPv6 (tcp6, tcp46) alike.
func formatNetstatCounts(output string) string {
	established := 0
	listening := 0

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(fields[0], "tcp") {
			continue
		}

		switch fields[len(fields)-1] {
		case "ESTABLISHED":
			established++
		case "LISTEN":
			listening++
		}
	}

	return fmt.Sprintf("TCP established: %d\nTCP listening: %d", established, listening)
}

// formatIfconfigAddresses lists the IPv4 addresses in ifconfig output on
// one line, like hostname -I does on linux. Loopback is left out, as
// hostname -I leaves it out.
func formatIfconfigAddresses(output string) string {
	var addresses []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "inet" || strings.HasPrefix(fields[1], "127.") {
			continue
		}
		addresses = append(addresses, fields[1])
	}
	return strings.Join(addresses, " ")
}

// launchdDomainPrefix is the launchd domain of the LaunchAgents of one
// logged-in user, followed by the user ID and the label.
const launchdDomainPrefix = "gui/"

// launchdService returns the service target launchctl takes,
// "gui/<uid>/<label>".
func launchdService(uid, label string) string {
	return launchdDomainPrefix + uid + "/" + label
}

// launchdSteps returns the launchctl commands of a start, stop or restart of
// the LaunchAgent label, in order.
//
// restart is "kickstart -k", which kills the running instance and starts a
// new one. stop is "bootout", which unloads the service: launchd would
// start a KeepAlive agent again after a plain kill. start enables the
// service and kickstarts it, which works for a loaded service. After a
// bootout the service is no longer loaded and only "launchctl bootstrap"
// with the plist path can load it again; the configuration holds the
// label, not the path, so start then fails and the reply carries
// launchctl's "Could not find service" error.
func launchdSteps(action, uid, label string) [][]string {
	service := launchdService(uid, label)

	switch action {
	case "restart":
		return [][]string{{"launchctl", "kickstart", "-k", service}}
	case "stop":
		return [][]string{{"launchctl", "bootout", service}}
	case "start":
		return [][]string{
			{"launchctl", "enable", service},
			{"launchctl", "kickstart", "-k", service},
		}
	}
	return nil
}

// launchctlNotFound is the exit status of "launchctl print" for a label the
// domain does not have.
const launchctlNotFound = 113

// parseLaunchctlPrint reads the state and pid of a service from "launchctl
// print" output. Only the service's own properties count: they sit one tab
// in, and the nested blocks (coalitions, sockets) carry a "state" of their
// own one tab deeper. The rest of the output, the environment included, is
// never returned, since it may hold secrets.
func parseLaunchctlPrint(output string) (string, bool) {
	state := ""
	pid := ""

	for _, line := range strings.Split(output, "\n") {
		property, isTopLevel := strings.CutPrefix(line, "\t")
		if !isTopLevel || strings.HasPrefix(property, "\t") {
			continue
		}

		if value, found := strings.CutPrefix(property, "state = "); found {
			state = strings.TrimSpace(value)
		}
		if value, found := strings.CutPrefix(property, "pid = "); found {
			pid = strings.TrimSpace(value)
		}
	}

	if state == "" {
		return "unknown", false
	}
	if state == "running" && pid != "" {
		return "running (pid " + pid + ")", true
	}
	return state, state == "running"
}

// logShowWindow is how far back the logs of a Mac service go. log show reads
// the unified log, which has no "last n lines" option, so a time window
// bounds what it prints and log_lines trims it afterwards.
const logShowWindow = "10m"

// logShowArgv returns the argv that reads the unified log of the process
// named process. The full path matters: over SSH the command goes through
// the user's shell, and zsh, the macOS default, has a builtin "log" that
// would answer instead.
func logShowArgv(process string) []string {
	predicate := `process == "` + process + `"`
	return []string{"/usr/bin/log", "show", "--last", logShowWindow, "--predicate", predicate, "--style", "compact"}
}

// launchdProcessName is the process name the logs of a LaunchAgent are
// searched by: the last part of its label, "web" for
// "com.tubearchivist.web".
func launchdProcessName(label string) string {
	return label[strings.LastIndex(label, ".")+1:]
}

// lastLogLines keeps the last count lines of log show output, without its
// "Timestamp ..." header. It returns "" when there are no lines.
func lastLogLines(output string, count int) string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "Timestamp ") {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, "\n")
}
