package command

import (
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

// vmStatSample is the head of vm_stat on an Apple silicon Mac, whose pages
// are 16 KiB. The rows after "Pages wired down" are there to be ignored.
const vmStatSample = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    98806.
Pages active:                                 903336.
Pages inactive:                               885174.
Pages speculative:                             17297.
Pages throttled:                                   0.
Pages wired down:                             229728.
Pages purgeable:                               33951.
"Translation faults":                     1071861186.
Pages stored in compressor:                   396897.
`

// launchctlPrintRunning is "launchctl print gui/501/com.txeo.ttyd" for a
// running LaunchAgent, trimmed. The coalition blocks carry their own
// "state = active" one tab deeper, which is not the service's state.
const launchctlPrintRunning = "gui/501/com.txeo.ttyd = {\n" +
	"\tactive count = 1\n" +
	"\tpath = /Users/txeo/Library/LaunchAgents/com.txeo.ttyd.plist\n" +
	"\ttype = LaunchAgent\n" +
	"\tstate = running\n" +
	"\n" +
	"\tprogram = /bin/zsh\n" +
	"\tenvironment = {\n" +
	"\t\tSECRET_TOKEN => do-not-show\n" +
	"\t}\n" +
	"\n" +
	"\tdomain = gui/501 [100002]\n" +
	"\truns = 1\n" +
	"\tpid = 700\n" +
	"\tlast exit code = (never exited)\n" +
	"\n" +
	"\tresource coalition = {\n" +
	"\t\tID = 885\n" +
	"\t\ttype = resource\n" +
	"\t\tstate = active\n" +
	"\t}\n" +
	"}\n"

// launchctlPrintStopped is a loaded LaunchAgent that is not running: no pid
// line, and a nested block whose "state = active" must not count.
const launchctlPrintStopped = "gui/501/com.example.backup = {\n" +
	"\tactive count = 0\n" +
	"\ttype = LaunchAgent\n" +
	"\tstate = not running\n" +
	"\truns = 3\n" +
	"\tlast exit code = 0\n" +
	"\tjetsam coalition = {\n" +
	"\t\tstate = active\n" +
	"\t\tpid = 12\n" +
	"\t}\n" +
	"}\n"

// netstatSample is "netstat -an -p tcp" with established, listening and
// closing sockets, IPv4 and IPv6.
const netstatSample = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)
tcp6       0      0  ::1.1980               ::1.50722              ESTABLISHED
tcp4       0      0  192.168.8.10.50123     17.57.146.20.443       ESTABLISHED
tcp4       0      0  192.168.8.10.50124     17.57.146.21.443       TIME_WAIT
tcp4       0      0  *.22                   *.*                    LISTEN
tcp46      0      0  *.8080                 *.*                    LISTEN
tcp4       0      0  192.168.8.10.50125     17.57.146.22.443       ESTABLISHED
`

// ifconfigSample is the part of ifconfig that matters: loopback, a LAN
// address and a Tailscale point-to-point address.
const ifconfigSample = "lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384\n" +
	"\tinet 127.0.0.1 netmask 0xff000000\n" +
	"\tinet6 ::1 prefixlen 128\n" +
	"en1: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500\n" +
	"\tinet 192.168.8.10 netmask 0xffffff00 broadcast 192.168.8.255\n" +
	"utun4: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1280\n" +
	"\tinet 100.123.156.9 --> 100.123.156.9 netmask 0xffffffff\n"

// testDarwinUID is the user ID the fake Mac answers to "id -u".
const testDarwinUID = "501"

// macTargetConfig returns the test config with the "mac" SSH target set to
// operatingSystem and one more Mac service, "backup", that has no port, so
// its status comes from launchd instead of an HTTP check.
func macTargetConfig(operatingSystem string) *config.Config {
	cfg := testConfig()

	cfg.SSHTargets = map[string]config.SSHTarget{
		"mac": {Host: "mac.example", OS: operatingSystem},
	}
	cfg.Services["backup"] = config.Service{
		Machine: "mac",
		Launchd: "com.example.backup",
	}
	return cfg
}

// answerLikeAMac answers the commands a Mac is asked for with sample output,
// and everything else with nothing.
func answerLikeAMac(argv []string) (executor.Result, error) {
	switch {
	case argv[0] == "id":
		return executor.Result{Output: testDarwinUID + "\n"}, nil
	case argv[0] == "ipconfig":
		return executor.Result{Output: "192.168.8.10\n"}, nil
	case argv[0] == "vm_stat":
		return executor.Result{Output: vmStatSample}, nil
	case argv[0] == "netstat":
		return executor.Result{Output: netstatSample}, nil
	case argv[0] == "launchctl" && argv[1] == "print":
		return executor.Result{Output: launchctlPrintStopped}, nil
	}
	return executor.Result{}, nil
}

// newMacBuiltins returns Builtins whose "mac" target runs operatingSystem,
// with one recorder for the local machine and one for the Mac over SSH.
func newMacBuiltins(operatingSystem string, remoteRespond func(argv []string) (executor.Result, error)) (*Builtins, *recordingExecutor, *recordingExecutor) {
	local := &recordingExecutor{}
	remote := &recordingExecutor{respond: remoteRespond}

	builtins := NewBuiltins(macTargetConfig(operatingSystem), local)
	builtins.SetSSHExecutor(remote)

	return builtins, local, remote
}

// TestCommandTableArgv checks, for every builtin that runs a machine or
// service command and for both operating systems, the exact argv run on
// each machine. The linux rows are what the builtins ran before the table
// existed, so the VPS does not change.
func TestCommandTableArgv(t *testing.T) {
	gui := "gui/" + testDarwinUID + "/"

	tests := []struct {
		name            string
		operatingSystem string
		run             func(builtins *Builtins) Response
		wantLocal       [][]string
		wantRemote      [][]string
	}{
		// The VPS is always linux, whatever the Mac runs.
		{
			name:            "free vps",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Free(Command{Target: "vps"}) },
			wantLocal:       [][]string{{"free", "-h"}},
		},
		{
			name:            "top vps",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Top(Command{Target: "vps"}) },
			wantLocal:       [][]string{{"ps", "aux", "--sort=-%cpu"}},
		},
		{
			name:            "ip vps",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Ip(Command{Target: "vps"}) },
			wantLocal:       [][]string{{"hostname", "-I"}},
		},
		{
			name:            "ports vps",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Ports(Command{Target: "vps"}) },
			wantLocal:       [][]string{{"ss", "-tlnp"}},
		},
		{
			name:            "connections vps",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Connections(Command{Target: "vps"}) },
			wantLocal:       [][]string{{"ss", "-s"}},
		},
		{
			name:            "updates without target",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Updates(Command{}) },
			wantLocal:       [][]string{{"apt", "list", "--upgradable"}},
		},
		{
			name:            "restart vps service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Restart(Command{Target: "nginx"}) },
			wantLocal:       [][]string{{"sudo", "systemctl", "restart", "nginx"}},
		},
		{
			name:            "stop vps service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Stop(Command{Target: "nginx"}) },
			wantLocal:       [][]string{{"sudo", "systemctl", "stop", "nginx"}},
		},
		{
			name:            "start vps service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Start(Command{Target: "nginx"}) },
			wantLocal:       [][]string{{"sudo", "systemctl", "start", "nginx"}},
		},
		{
			name:            "status vps service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Status(Command{Target: "nginx"}) },
			wantLocal:       [][]string{{"systemctl", "is-active", "nginx"}},
		},
		{
			name:            "logs vps service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Logs(Command{Target: "nginx"}) },
			wantLocal:       [][]string{{"sudo", "journalctl", "-u", "nginx", "-n", "20", "--no-pager"}},
		},

		// A "mac" target left as linux, the default, keeps the linux argv.
		{
			name:            "free mac on linux",
			operatingSystem: "",
			run:             func(builtins *Builtins) Response { return builtins.Free(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"free", "-h"}},
		},
		{
			name:            "top mac on linux",
			operatingSystem: "linux",
			run:             func(builtins *Builtins) Response { return builtins.Top(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"ps", "aux", "--sort=-%cpu"}},
		},
		{
			name:            "ip mac on linux",
			operatingSystem: "linux",
			run:             func(builtins *Builtins) Response { return builtins.Ip(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"hostname", "-I"}},
		},
		{
			name:            "ports mac on linux",
			operatingSystem: "linux",
			run:             func(builtins *Builtins) Response { return builtins.Ports(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"ss", "-tlnp"}},
		},
		{
			name:            "connections mac on linux",
			operatingSystem: "linux",
			run:             func(builtins *Builtins) Response { return builtins.Connections(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"ss", "-s"}},
		},
		{
			name:            "updates mac on linux",
			operatingSystem: "linux",
			run:             func(builtins *Builtins) Response { return builtins.Updates(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"apt", "list", "--upgradable"}},
		},

		// The darwin rows.
		{
			name:            "free mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Free(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"vm_stat"}},
		},
		{
			name:            "top mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Top(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"ps", "-r", "-A", "-o", "pid,pcpu,pmem,comm"}},
		},
		{
			name:            "ip mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Ip(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"ipconfig", "getifaddr", "en0"}},
		},
		{
			name:            "ports mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Ports(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"lsof", "-iTCP", "-sTCP:LISTEN", "-P", "-n"}},
		},
		{
			name:            "connections mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Connections(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"netstat", "-an", "-p", "tcp"}},
		},
		{
			name:            "updates mac on darwin",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Updates(Command{Target: "mac"}) },
			wantRemote:      [][]string{{"softwareupdate", "-l"}},
		},
		{
			name:            "restart mac service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Restart(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"id", "-u"},
				{"launchctl", "kickstart", "-k", gui + "com.example.backup"},
			},
		},
		{
			name:            "stop mac service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Stop(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"id", "-u"},
				{"launchctl", "bootout", gui + "com.example.backup"},
			},
		},
		{
			name:            "start mac service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Start(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"id", "-u"},
				{"launchctl", "enable", gui + "com.example.backup"},
				{"launchctl", "kickstart", "-k", gui + "com.example.backup"},
			},
		},
		{
			name:            "status mac service without port",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Status(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"id", "-u"},
				{"launchctl", "print", gui + "com.example.backup"},
			},
		},
		{
			name:            "status mac service with port",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Status(Command{Target: "tube"}) },
			wantRemote: [][]string{
				{"curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "5", "http://127.0.0.1:8080/"},
			},
		},
		{
			name:            "logs mac service",
			operatingSystem: "darwin",
			run:             func(builtins *Builtins) Response { return builtins.Logs(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"/usr/bin/log", "show", "--last", "10m", "--predicate", `process == "backup"`, "--style", "compact"},
			},
		},

		// launchd exists only on macOS, so a Mac service uses launchctl's
		// own commands even when the target's os was left out.
		{
			name:            "restart mac service with os left out",
			operatingSystem: "",
			run:             func(builtins *Builtins) Response { return builtins.Restart(Command{Target: "backup"}) },
			wantRemote: [][]string{
				{"id", "-u"},
				{"launchctl", "kickstart", "-k", gui + "com.example.backup"},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builtins, local, remote := newMacBuiltins(test.operatingSystem, answerLikeAMac)

			response := test.run(builtins)

			if got := local.recorded(); !reflect.DeepEqual(got, test.wantLocal) {
				t.Errorf("local argv = %q, want %q (response %q)", got, test.wantLocal, response.Message)
			}
			if got := remote.recorded(); !reflect.DeepEqual(got, test.wantRemote) {
				t.Errorf("remote argv = %q, want %q (response %q)", got, test.wantRemote, response.Message)
			}
		})
	}
}

// TestDarwinUserIDIsAskedOnce checks that "id -u" runs on the first launchd
// command only and that later ones reuse the cached domain.
func TestDarwinUserIDIsAskedOnce(t *testing.T) {
	builtins, _, remote := newMacBuiltins("darwin", answerLikeAMac)

	builtins.Restart(Command{Target: "backup"})
	builtins.Restart(Command{Target: "backup"})

	asked := 0
	for _, argv := range remote.recorded() {
		if argv[0] == "id" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("id -u ran %d times, want 1", asked)
	}
}

// TestDarwinUserIDIsAskedOnceUnderLoad checks the cache with commands that
// arrive together, as the panel and the Gotify stream can send them: the
// first one asks, the others wait for its answer.
func TestDarwinUserIDIsAskedOnceUnderLoad(t *testing.T) {
	builtins, _, remote := newMacBuiltins("darwin", answerLikeAMac)

	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			builtins.Status(Command{Target: "backup"})
		}()
	}
	group.Wait()

	asked := 0
	for _, argv := range remote.recorded() {
		if argv[0] == "id" {
			asked++
		}
	}
	if asked != 1 {
		t.Errorf("id -u ran %d times for 8 concurrent commands, want 1", asked)
	}
}

// TestDarwinUserIDFailureIsReported checks that a Mac that does not answer
// "id -u" with a number gets no launchctl command and an error reply.
func TestDarwinUserIDFailureIsReported(t *testing.T) {
	builtins, _, remote := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		return executor.Result{Output: "id: command not found\n", ExitCode: 127}, nil
	})

	response := builtins.Restart(Command{Target: "backup"})

	if response.Priority < 7 {
		t.Errorf("priority = %d, want an error reply; message %q", response.Priority, response.Message)
	}
	if calls := remote.recorded(); len(calls) != 1 {
		t.Errorf("calls = %q, want only id -u", calls)
	}
}

// TestDarwinIPFallsBackToIfconfig checks that a Mac without an address on
// en0 lists the addresses ifconfig shows, loopback left out.
func TestDarwinIPFallsBackToIfconfig(t *testing.T) {
	builtins, _, remote := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		if argv[0] == "ipconfig" {
			return executor.Result{ExitCode: 1}, nil
		}
		return executor.Result{Output: ifconfigSample}, nil
	})

	response := builtins.Ip(Command{Target: "mac"})

	want := [][]string{{"ipconfig", "getifaddr", "en0"}, {"ifconfig"}}
	if got := remote.recorded(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv = %q, want %q", got, want)
	}
	if !strings.HasSuffix(response.Message, "\n192.168.8.10 100.123.156.9") {
		t.Errorf("message = %q, want the two addresses", response.Message)
	}
}

// TestDarwinMessages checks what the Mac commands that need work in Go
// answer, from the sample outputs.
func TestDarwinMessages(t *testing.T) {
	builtins, _, _ := newMacBuiltins("darwin", answerLikeAMac)

	memory := builtins.Free(Command{Target: "mac"})
	wantMemory := "💾 Mac Memory\nFree: 1543 MiB\nActive: 14114 MiB\nInactive: 13830 MiB\nWired: 3589 MiB"
	if memory.Message != wantMemory {
		t.Errorf("free = %q, want %q", memory.Message, wantMemory)
	}

	connections := builtins.Connections(Command{Target: "mac"})
	wantConnections := "🔗 Mac Connections\nTCP established: 3\nTCP listening: 2"
	if connections.Message != wantConnections {
		t.Errorf("connections = %q, want %q", connections.Message, wantConnections)
	}

	status := builtins.Status(Command{Target: "backup"})
	if status.Message != "❌ backup — not running" {
		t.Errorf("status = %q", status.Message)
	}
}

// TestParseVMStat checks the memory summary built from vm_stat.
func TestParseVMStat(t *testing.T) {
	summary, ok := parseVMStat(vmStatSample)

	want := "Free: 1543 MiB\nActive: 14114 MiB\nInactive: 13830 MiB\nWired: 3589 MiB"
	if !ok || summary != want {
		t.Errorf("parseVMStat = %q, %v; want %q, true", summary, ok, want)
	}

	// Without the header the page size is unknown, and guessing 4 KiB on
	// a 16 KiB Mac would report a quarter of the memory.
	if _, ok := parseVMStat("Pages free: 10.\n"); ok {
		t.Error("parseVMStat accepted output without the page size header")
	}
	if _, ok := parseVMStat("ssh: connect to host mac: Connection refused\n"); ok {
		t.Error("parseVMStat accepted an error message")
	}
}

// TestParseLaunchctlPrint checks that only the top level state and pid of
// the service count, never those of a nested block.
func TestParseLaunchctlPrint(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantStatus  string
		wantRunning bool
	}{
		{"running", launchctlPrintRunning, "running (pid 700)", true},
		{"stopped", launchctlPrintStopped, "not running", false},
		{"no state", "Bad request.\n", "unknown", false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status, running := parseLaunchctlPrint(test.output)

			if status != test.wantStatus || running != test.wantRunning {
				t.Errorf("parseLaunchctlPrint = %q, %v; want %q, %v", status, running, test.wantStatus, test.wantRunning)
			}
			if strings.Contains(status, "do-not-show") {
				t.Errorf("status leaks the service environment: %q", status)
			}
		})
	}
}

// TestDarwinStatusNotLoaded checks the answer for a label launchd does not
// know, which launchctl print reports with exit status 113.
func TestDarwinStatusNotLoaded(t *testing.T) {
	builtins, _, _ := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		if argv[0] == "id" {
			return executor.Result{Output: testDarwinUID + "\n"}, nil
		}
		output := "Bad request.\nCould not find service \"com.example.backup\" in domain for user gui: 501\n"
		return executor.Result{Output: output, ExitCode: 113}, nil
	})

	response := builtins.Status(Command{Target: "backup"})

	if response.Message != "❌ backup — not loaded" {
		t.Errorf("status = %q", response.Message)
	}
}

// TestDarwinLogsKeepsTheLastLines checks that log show output is cut to the
// configured log_lines, newest last, and that an empty window says so.
func TestDarwinLogsKeepsTheLastLines(t *testing.T) {
	var output strings.Builder
	output.WriteString("Timestamp               Ty Process[PID:TID]\n")
	for line := 1; line <= 40; line++ {
		output.WriteString("2026-11-08 13:52:49.099 Df backup[471:e5ed6] line " + strings.Repeat("x", line) + "\n")
	}

	builtins, _, _ := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		return executor.Result{Output: output.String()}, nil
	})

	response := builtins.Logs(Command{Target: "backup"})

	lines := strings.Split(response.Message, "\n")
	// The title line plus log_lines (20) lines of log.
	if len(lines) != 21 {
		t.Fatalf("message has %d lines, want 21: %q", len(lines), response.Message)
	}
	if !strings.HasSuffix(lines[20], strings.Repeat("x", 40)) {
		t.Errorf("last line = %q, want the newest", lines[20])
	}

	empty, _, _ := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		return executor.Result{Output: "Timestamp               Ty Process[PID:TID]\n"}, nil
	})
	response = empty.Logs(Command{Target: "backup"})
	if !strings.Contains(response.Message, "No log lines in the last 10 minutes") {
		t.Errorf("empty logs = %q", response.Message)
	}
}

// TestDarwinUpdatesMessage checks the softwareupdate answers.
func TestDarwinUpdatesMessage(t *testing.T) {
	upToDate := "Software Update Tool\n\nFinding available software\nNo new software available.\n"
	builtins, _, _ := newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		return executor.Result{Output: upToDate}, nil
	})

	response := builtins.Updates(Command{Target: "mac"})
	if response.Message != "📦 Available Updates\nSystem is up to date" {
		t.Errorf("up to date = %q", response.Message)
	}

	pending := "Software Update Tool\n\nFinding available software\nSoftware Update found the following new or updated software:\n* Label: macOS Tahoe 26.2-25C56\n"
	builtins, _, _ = newMacBuiltins("darwin", func(argv []string) (executor.Result, error) {
		return executor.Result{Output: pending}, nil
	})

	response = builtins.Updates(Command{Target: "mac"})
	want := "📦 Available Updates\nSoftware Update found the following new or updated software:\n* Label: macOS Tahoe 26.2-25C56"
	if response.Message != want {
		t.Errorf("pending = %q, want %q", response.Message, want)
	}
}
