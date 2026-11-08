package command

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

// ─── Fixtures ───────────────────────────────────────────────────────────────

// dfFixture is "df -P" on a VPS with a snap loop device and a tmpfs, both of
// which are always full or meaningless and must not raise a warning.
const dfFixture = `Filesystem     1024-blocks     Used Available Capacity Mounted on
udev               1976408        0   1976408       0% /dev
tmpfs               401796     1100    400696       1% /run
/dev/vda1         51538400 46384560   5153840      91% /
/dev/vda15          106858     6186    100672       6% /boot/efi
/dev/loop0           65536    65536         0     100% /snap/core20/2318
/dev/sdb1        103081248 20616249  82464999      20% /mnt/data volume
`

// freeFixture is "free -m" from procps 3.3.10 or later, with the available
// column.
const freeFixture = `               total        used        free      shared  buff/cache   available
Mem:            3916        1203         312          45        2400        2420
Swap:           1023           0        1023
`

// freeOldFixture is "free -m" from an older procps without the available
// column.
const freeOldFixture = `             total       used       free     shared    buffers     cached
Mem:          3916       3604        312         45        100       2000
Swap:         1023          0       1023
`

// loadavgFixture is /proc/loadavg.
const loadavgFixture = "0.52 2.58 0.59 1/389 12345\n"

// failedFixture is "systemctl --failed --no-legend --plain" with two units.
const failedFixture = `nginx.service   loaded failed failed A high performance web server
backup.timer    loaded failed failed Nightly backup
`

// fixedNow is the clock of the health tests: 2026-11-08 09:00 UTC.
var fixedNow = time.Date(2026, time.November, 8, 9, 0, 0, 0, time.UTC)

// operationsConfig is the test configuration plus two allowed containers.
func operationsConfig() *config.Config {
	cfg := testConfig()
	cfg.Docker.AllowedContainers = []string{"web", "Immich_Server"}
	return cfg
}

// newTestOperations returns operations over a recording local executor, a
// recording SSH executor, a fixed clock and two CPUs.
func newTestOperations(local, remote func(argv []string) (executor.Result, error)) (*operations, *recordingExecutor, *recordingExecutor) {
	localRecorder := &recordingExecutor{respond: local}
	remoteRecorder := &recordingExecutor{respond: remote}

	builtins := NewBuiltins(operationsConfig(), localRecorder)
	builtins.SetSSHExecutor(remoteRecorder)

	ops := newOperations(builtins)
	ops.now = func() time.Time { return fixedNow }
	ops.cpus = 2

	return ops, localRecorder, remoteRecorder
}

// joined returns the argv lists as space-joined strings, easier to compare.
func joined(calls [][]string) []string {
	var lines []string
	for _, argv := range calls {
		lines = append(lines, strings.Join(argv, " "))
	}
	return lines
}

// ─── Parsing ────────────────────────────────────────────────────────────────

// TestParseOperationsVerbs checks the targets and the docker subcommands.
func TestParseOperationsVerbs(t *testing.T) {
	cases := []struct {
		input  string
		action Action
		target string
	}{
		{"failed", ActionFailed, "vps"},
		{"failed mac", ActionFailed, "mac"},
		{"health", ActionHealth, ""},
		{"health mac", ActionHealth, ""},
		{"digest", ActionDigest, ""},
		{"docker", ActionDocker, ""},
		{"docker ps", ActionDockerPs, ""},
		{"docker ps web", ActionDockerPs, "web"},
		{"Docker Restart Immich_Server", ActionDockerRestart, "immich_server"},
		{"docker logs web", ActionDockerLogs, "web"},
		{"docker log web", ActionDockerLogs, "web"},
		{"docker-restart web", ActionDockerRestart, "web"},
		{"docker rm web", ActionDocker, "rm"},
	}

	for _, testCase := range cases {
		cmd, err := Parse(testCase.input, testAliases, testServices)
		if err != nil {
			t.Errorf("Parse(%q) error = %v", testCase.input, err)
			continue
		}
		if cmd.Action != testCase.action || cmd.Target != testCase.target {
			t.Errorf("Parse(%q) = %q %q, want %q %q", testCase.input, cmd.Action, cmd.Target, testCase.action, testCase.target)
		}
	}
}

// TestParseFailedOutput checks unit names and descriptions.
func TestParseFailedOutput(t *testing.T) {
	units := parseFailedUnits(failedFixture + "\n")

	want := []failedUnit{
		{Name: "nginx.service", Description: "A high performance web server"},
		{Name: "backup.timer", Description: "Nightly backup"},
	}
	if !slices.Equal(units, want) {
		t.Errorf("units = %+v, want %+v", units, want)
	}

	// An older systemctl may still print the bullet in front of the unit.
	bullet := parseFailedUnits("● cron.service loaded failed failed Cron\n")
	if len(bullet) != 1 || bullet[0].Name != "cron.service" {
		t.Errorf("bullet line = %+v, want cron.service", bullet)
	}

	if empty := parseFailedUnits(""); len(empty) != 0 {
		t.Errorf("empty output = %+v, want none", empty)
	}
}

// TestParseDf checks that pseudo filesystems and snaps are skipped and that a
// mount point with a space keeps it.
func TestParseDf(t *testing.T) {
	mounts := parseDf(dfFixture)

	want := []diskUsage{
		{Mount: "/", Percent: 91},
		{Mount: "/boot/efi", Percent: 6},
		{Mount: "/mnt/data volume", Percent: 20},
	}
	if !slices.Equal(mounts, want) {
		t.Errorf("mounts = %+v, want %+v", mounts, want)
	}
}

// TestParseFree checks both procps layouts.
func TestParseFree(t *testing.T) {
	total, used, err := parseFree(freeFixture)
	if err != nil || total != 3916 || used != 1496 {
		t.Errorf("parseFree = %d %d %v, want 3916 1496 nil", total, used, err)
	}

	total, used, err = parseFree(freeOldFixture)
	if err != nil || total != 3916 || used != 3604 {
		t.Errorf("parseFree old = %d %d %v, want 3916 3604 nil", total, used, err)
	}

	if _, _, err := parseFree("garbage"); err == nil {
		t.Error("parseFree(garbage) error = nil, want an error")
	}
}

// TestParseLoadavg checks the three averages and a broken input.
func TestParseLoadavg(t *testing.T) {
	averages, err := parseLoadavg(loadavgFixture)
	if err != nil || averages != [3]float64{0.52, 2.58, 0.59} {
		t.Errorf("parseLoadavg = %v %v", averages, err)
	}

	if _, err := parseLoadavg("x y"); err == nil {
		t.Error("parseLoadavg(x y) error = nil, want an error")
	}
}

// TestParseCertDate checks openssl's notAfter format, with the padded day.
func TestParseCertDate(t *testing.T) {
	expiry, err := parseCertDate("Jan  2 03:04:05 2027 GMT")
	if err != nil {
		t.Fatalf("parseCertDate error = %v", err)
	}

	want := time.Date(2027, time.January, 2, 3, 4, 5, 0, time.UTC)
	if !expiry.Equal(want) {
		t.Errorf("expiry = %s, want %s", expiry, want)
	}

	if _, err := parseCertDate("soon"); err == nil {
		t.Error("parseCertDate(soon) error = nil, want an error")
	}
}

// ─── failed ─────────────────────────────────────────────────────────────────

// TestFailedListsLocalUnits checks the argv and the list.
func TestFailedListsLocalUnits(t *testing.T) {
	ops, local, remote := newTestOperations(func(argv []string) (executor.Result, error) {
		return executor.Result{Output: failedFixture}, nil
	}, nil)

	response := ops.Failed(Command{Action: ActionFailed, Target: "vps"})

	wantCalls := []string{"systemctl --failed --no-legend --plain"}
	if got := joined(local.recorded()); !slices.Equal(got, wantCalls) {
		t.Errorf("calls = %q, want %q", got, wantCalls)
	}
	if len(remote.recorded()) != 0 {
		t.Errorf("remote calls = %q, want none", remote.recorded())
	}

	want := "🧯 VPS Failed Units (2)\n- nginx.service: A high performance web server\n- backup.timer: Nightly backup"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}
}

// TestFailedNone checks the empty answer.
func TestFailedNone(t *testing.T) {
	ops, _, _ := newTestOperations(nil, nil)

	response := ops.Failed(Command{Action: ActionFailed, Target: "vps"})

	if response.Message != "✅ No failed units" {
		t.Errorf("message = %q, want the no failed units line", response.Message)
	}
}

// TestFailedMacOnDarwin checks that a macOS target is asked its system and
// not sent a systemctl it does not have.
func TestFailedMacOnDarwin(t *testing.T) {
	ops, local, remote := newTestOperations(nil, func(argv []string) (executor.Result, error) {
		return executor.Result{Output: "Darwin\n"}, nil
	})

	response := ops.Failed(Command{Action: ActionFailed, Target: "mac"})

	if !strings.Contains(response.Message, "not available on macOS") {
		t.Errorf("message = %q, want not available on macOS", response.Message)
	}
	if got := joined(remote.recorded()); !slices.Equal(got, []string{"uname -s"}) {
		t.Errorf("remote calls = %q, want only uname -s", got)
	}
	if len(local.recorded()) != 0 {
		t.Errorf("local calls = %q, want none", local.recorded())
	}
}

// TestFailedMacOnLinux checks that a Linux SSH target runs systemctl there.
func TestFailedMacOnLinux(t *testing.T) {
	ops, local, remote := newTestOperations(nil, func(argv []string) (executor.Result, error) {
		if argv[0] == "uname" {
			return executor.Result{Output: "Linux\n"}, nil
		}
		return executor.Result{Output: "cron.service loaded failed failed Cron\n"}, nil
	})

	response := ops.Failed(Command{Action: ActionFailed, Target: "mac"})

	wantCalls := []string{"uname -s", "systemctl --failed --no-legend --plain"}
	if got := joined(remote.recorded()); !slices.Equal(got, wantCalls) {
		t.Errorf("remote calls = %q, want %q", got, wantCalls)
	}
	if len(local.recorded()) != 0 {
		t.Errorf("local calls = %q, want none", local.recorded())
	}
	if !strings.HasPrefix(response.Message, "🧯 Mac Failed Units (1)") {
		t.Errorf("message = %q, want the Mac list", response.Message)
	}
}

// TestFailedMacWithoutSSH checks that a missing SSH target is reported
// instead of answering with the local machine's units.
func TestFailedMacWithoutSSH(t *testing.T) {
	recorder := &recordingExecutor{}
	ops := newOperations(NewBuiltins(operationsConfig(), recorder))

	response := ops.Failed(Command{Action: ActionFailed, Target: "mac"})

	if !strings.HasPrefix(response.Title, "❌") {
		t.Errorf("title = %q, want an error", response.Title)
	}
	if len(recorder.recorded()) != 0 {
		t.Errorf("calls = %q, want none", recorder.recorded())
	}
}

// ─── health ─────────────────────────────────────────────────────────────────

// healthResponder answers every command health runs with the fixtures; the
// certificate alpha expires in five days and zeta in a year.
func healthResponder(argv []string) (executor.Result, error) {
	switch {
	case argv[0] == "df":
		return executor.Result{Output: dfFixture}, nil
	case argv[0] == "cat":
		return executor.Result{Output: loadavgFixture}, nil
	case argv[0] == "free":
		return executor.Result{Output: freeFixture}, nil
	case argv[0] == "systemctl":
		return executor.Result{Output: failedFixture}, nil
	case argvContains(argv, "find"):
		output := "/etc/letsencrypt/live/zeta.example/cert.pem\n/etc/letsencrypt/live/alpha.example/cert.pem\n"
		return executor.Result{Output: output}, nil
	case strings.Contains(argv[len(argv)-1], "alpha"):
		return executor.Result{Output: "notAfter=Nov 13 09:00:00 2026 GMT\n"}, nil
	}
	return executor.Result{Output: "notAfter=Nov  8 09:00:00 2027 GMT\n"}, nil
}

// TestHealthSummary checks every line, its marker and the argv of each call.
func TestHealthSummary(t *testing.T) {
	ops, local, _ := newTestOperations(healthResponder, nil)

	response := ops.Health(Command{Action: ActionHealth})

	if response.Title != "🩺 Health" {
		t.Errorf("title = %q, want 🩺 Health", response.Title)
	}

	want := strings.Join([]string{
		"🩺 Health",
		"⚠️ Disk /: 91 % used",
		"⚠️ Load: 0.52 2.58 0.59 (2 CPUs)",
		"✅ Memory: 1496 of 3916 MB used (38 %)",
		"⚠️ Failed units: 2 (nginx.service, backup.timer)",
		"⚠️ Certificate alpha.example expires in 5 days",
	}, "\n")
	if response.Message != want {
		t.Errorf("message =\n%s\nwant\n%s", response.Message, want)
	}

	wantCalls := []string{
		"df -P",
		"cat /proc/loadavg",
		"free -m",
		"systemctl --failed --no-legend --plain",
		"sudo find /etc/letsencrypt/live -name cert.pem",
		"sudo openssl x509 -enddate -noout -in /etc/letsencrypt/live/alpha.example/cert.pem",
		"sudo openssl x509 -enddate -noout -in /etc/letsencrypt/live/zeta.example/cert.pem",
	}
	if got := joined(local.recorded()); !slices.Equal(got, wantCalls) {
		t.Errorf("calls =\n%q\nwant\n%q", got, wantCalls)
	}
	assertNoInterpolatedShell(t, local.recorded(), "")
}

// TestHealthAllGood checks the green lines and the low priority.
func TestHealthAllGood(t *testing.T) {
	ops, _, _ := newTestOperations(func(argv []string) (executor.Result, error) {
		switch argv[0] {
		case "df":
			return executor.Result{Output: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/vda1 100 10 90 10% /\n"}, nil
		case "cat":
			return executor.Result{Output: "0.10 0.20 0.30 1/100 1\n"}, nil
		case "free":
			return executor.Result{Output: freeFixture}, nil
		}
		return executor.Result{}, nil
	}, nil)

	response := ops.Health(Command{Action: ActionHealth})

	want := strings.Join([]string{
		"🩺 Health",
		"✅ Disk: every mount under 85 %",
		"✅ Load: 0.10 0.20 0.30 (2 CPUs)",
		"✅ Memory: 1496 of 3916 MB used (38 %)",
		"✅ Failed units: none",
		"✅ Certificates: none found",
	}, "\n")
	if response.Message != want {
		t.Errorf("message =\n%s\nwant\n%s", response.Message, want)
	}
	if response.Priority != healthOKPriority {
		t.Errorf("priority = %d, want %d", response.Priority, healthOKPriority)
	}
}

// TestHealthReportsBrokenChecks checks that one failing command marks its own
// line and does not hide the others.
func TestHealthReportsBrokenChecks(t *testing.T) {
	ops, _, _ := newTestOperations(func(argv []string) (executor.Result, error) {
		if argv[0] == "free" {
			return executor.Result{}, errors.New("exec: free not found")
		}
		return healthResponder(argv)
	}, nil)

	response := ops.Health(Command{Action: ActionHealth})

	if !strings.Contains(response.Message, "⚠️ Memory: unavailable") {
		t.Errorf("message = %q, want the memory line marked unavailable", response.Message)
	}
	if !strings.Contains(response.Message, "⚠️ Disk /: 91 % used") {
		t.Errorf("message = %q, want the disk line still there", response.Message)
	}
	if response.Priority != healthWarnPriority {
		t.Errorf("priority = %d, want %d", response.Priority, healthWarnPriority)
	}
}

// TestDigestCommandSendsHealthNow checks that digest is the health summary
// under its own title.
func TestDigestCommandSendsHealthNow(t *testing.T) {
	ops, _, _ := newTestOperations(healthResponder, nil)

	response := ops.DigestNow(Command{Action: ActionDigest})

	if response.Title != digestTitle {
		t.Errorf("title = %q, want %q", response.Title, digestTitle)
	}
	if !strings.Contains(response.Message, "⚠️ Disk /: 91 % used") {
		t.Errorf("message = %q, want the health lines", response.Message)
	}
}

// ─── docker ─────────────────────────────────────────────────────────────────

// dockerResponder answers the lookup as present and records nothing special.
func dockerResponder(argv []string) (executor.Result, error) {
	if argv[0] == "sh" {
		return executor.Result{Output: "/usr/bin/docker\n"}, nil
	}
	if argvContains(argv, "ps") {
		return executor.Result{Output: "web Up 3 hours\nsecret_db Up 2 days\nImmich_Server Exited (0) 1 day ago\n"}, nil
	}
	return executor.Result{Output: "ok\n"}, nil
}

// TestDockerRefusesContainersOutsideTheList checks that nothing runs for a
// name that is not allowed.
func TestDockerRefusesContainersOutsideTheList(t *testing.T) {
	ops, local, _ := newTestOperations(dockerResponder, nil)

	for _, response := range []Response{
		ops.DockerRestart(Command{Action: ActionDockerRestart, Target: "secret_db"}),
		ops.DockerLogs(Command{Action: ActionDockerLogs, Target: "secret_db"}),
		ops.DockerPs(Command{Action: ActionDockerPs, Target: "secret_db"}),
	} {
		if !strings.HasPrefix(response.Message, "🚫 Container not allowed") {
			t.Errorf("message = %q, want 🚫 Container not allowed", response.Message)
		}
	}

	if len(local.recorded()) != 0 {
		t.Errorf("calls = %q, want none", local.recorded())
	}
}

// TestDockerNotAvailable checks the answer when docker is not on PATH.
func TestDockerNotAvailable(t *testing.T) {
	ops, _, _ := newTestOperations(func(argv []string) (executor.Result, error) {
		return executor.Result{ExitCode: 1}, nil
	}, nil)

	response := ops.DockerRestart(Command{Action: ActionDockerRestart, Target: "web"})

	if response.Message != "docker not available" {
		t.Errorf("message = %q, want docker not available", response.Message)
	}
}

// TestDockerLookupIsCached checks that the PATH lookup runs once for several
// commands, with the fixed script.
func TestDockerLookupIsCached(t *testing.T) {
	ops, local, _ := newTestOperations(dockerResponder, nil)

	ops.DockerRestart(Command{Action: ActionDockerRestart, Target: "web"})
	ops.DockerLogs(Command{Action: ActionDockerLogs, Target: "immich_server"})

	wantCalls := []string{
		"sh -c " + dockerLookupScript,
		"docker restart web",
		"docker logs --tail 20 Immich_Server",
	}
	if got := joined(local.recorded()); !slices.Equal(got, wantCalls) {
		t.Errorf("calls = %q, want %q", got, wantCalls)
	}
}

// TestDockerLookupErrorIsNotCached checks that a lookup that timed out is
// tried again on the next command.
func TestDockerLookupErrorIsNotCached(t *testing.T) {
	attempts := 0
	ops, _, _ := newTestOperations(func(argv []string) (executor.Result, error) {
		if argv[0] == "sh" {
			attempts++
			if attempts == 1 {
				return executor.Result{}, context.DeadlineExceeded
			}
		}
		return dockerResponder(argv)
	}, nil)

	first := ops.DockerRestart(Command{Action: ActionDockerRestart, Target: "web"})
	second := ops.DockerRestart(Command{Action: ActionDockerRestart, Target: "web"})

	if first.Message != "docker not available" {
		t.Errorf("first message = %q, want docker not available", first.Message)
	}
	if strings.Contains(second.Message, "not available") {
		t.Errorf("second message = %q, want the restart", second.Message)
	}
	if attempts != 2 {
		t.Errorf("lookups = %d, want 2", attempts)
	}
}

// TestDockerPsShowsOnlyAllowedContainers checks the filter and the order of
// the allow-list, with a container that is not there.
func TestDockerPsShowsOnlyAllowedContainers(t *testing.T) {
	ops, local, _ := newTestOperations(func(argv []string) (executor.Result, error) {
		if argv[0] == "sh" {
			return executor.Result{}, nil
		}
		return executor.Result{Output: "secret_db Up 2 days\nImmich_Server Exited (0) 1 day ago\n"}, nil
	}, nil)

	response := ops.DockerPs(Command{Action: ActionDockerPs})

	want := "🐳 Containers\n- web: not found\n- Immich_Server: Exited (0) 1 day ago"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}

	calls := local.recorded()
	wantPs := "docker ps --all --format {{.Names}} {{.Status}}"
	if got := strings.Join(calls[len(calls)-1], " "); got != wantPs {
		t.Errorf("ps call = %q, want %q", got, wantPs)
	}
}

// TestDockerPsWithoutAllowList checks the hint when nothing is allowed.
func TestDockerPsWithoutAllowList(t *testing.T) {
	recorder := &recordingExecutor{respond: dockerResponder}
	ops := newOperations(NewBuiltins(testConfig(), recorder))

	response := ops.DockerPs(Command{Action: ActionDockerPs})

	if !strings.Contains(response.Message, "docker.allowed_containers") {
		t.Errorf("message = %q, want the setting named", response.Message)
	}
	if len(recorder.recorded()) != 0 {
		t.Errorf("calls = %q, want none", recorder.recorded())
	}
}

// TestDockerUsage checks the answer to a bare or unknown subcommand.
func TestDockerUsage(t *testing.T) {
	ops, local, _ := newTestOperations(dockerResponder, nil)

	response := ops.DockerUsage(Command{Action: ActionDocker, Target: "rm"})

	if !strings.Contains(response.Message, "docker ps|restart|logs <name>") {
		t.Errorf("message = %q, want the usage", response.Message)
	}
	if len(local.recorded()) != 0 {
		t.Errorf("calls = %q, want none", local.recorded())
	}
}

// TestDangerousActionsHasDockerRestartOnly checks that reading containers is
// not slowed down by the danger slot.
func TestDangerousActionsHasDockerRestartOnly(t *testing.T) {
	if got := DangerousActions(); !slices.Equal(got, []Action{ActionDockerRestart}) {
		t.Errorf("DangerousActions = %q, want docker-restart only", got)
	}
}

// ─── registration and help ──────────────────────────────────────────────────

// TestRegisterExtras checks that every operations action has a handler and
// that registering runs nothing.
func TestRegisterExtras(t *testing.T) {
	recorder := &recordingExecutor{}
	registry := NewRegistry()

	NewBuiltins(operationsConfig(), recorder).RegisterExtras(registry)

	want := []string{"digest", "docker", "docker-logs", "docker-ps", "docker-restart", "failed", "health"}
	if got := registry.Names(); !slices.Equal(got, want) {
		t.Errorf("names = %q, want %q", got, want)
	}
	if len(recorder.recorded()) != 0 {
		t.Errorf("calls = %q, want none at registration", recorder.recorded())
	}
}

// TestExtraHelpLines checks one help row per command, in the table format of
// help.
func TestExtraHelpLines(t *testing.T) {
	lines := ExtraHelpLines()

	for _, verb := range []string{"failed", "health", "docker", "digest"} {
		found := false
		for _, line := range lines {
			if strings.HasPrefix(line, "| `"+verb) && strings.HasSuffix(line, " |") {
				found = true
			}
		}
		if !found {
			t.Errorf("no help row for %q in %q", verb, lines)
		}
	}
}

// ─── Digest ─────────────────────────────────────────────────────────────────

// fakeWait is one timer the digest asked for, fired by the test.
type fakeWait struct {
	duration time.Duration
	fire     chan time.Time
}

// fakeClock is a clock the test moves by hand. Every after call is handed to
// the test through waits, buffered so the digest never blocks on it.
type fakeClock struct {
	mu      sync.Mutex
	current time.Time
	waits   chan fakeWait
}

// newFakeClock returns a clock stopped at start.
func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{current: start, waits: make(chan fakeWait, 16)}
}

// now returns the time the test last set.
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.current
}

// set moves the clock to moment.
func (c *fakeClock) set(moment time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.current = moment
}

// after records the wait and returns its channel.
func (c *fakeClock) after(duration time.Duration) <-chan time.Time {
	wait := fakeWait{duration: duration, fire: make(chan time.Time, 1)}
	c.waits <- wait
	return wait.fire
}

// nextWait returns the next timer the digest asked for, or fails the test.
func (c *fakeClock) nextWait(t *testing.T) fakeWait {
	t.Helper()

	select {
	case wait := <-c.waits:
		return wait
	case <-time.After(2 * time.Second):
		t.Fatal("the digest asked for no timer")
		return fakeWait{}
	}
}

// newTestDigest returns an enabled digest at 07:30 UTC over clock, and the
// channel that receives what it sends.
func newTestDigest(t *testing.T, clock *fakeClock, summary func() Response) (*Digest, chan Response) {
	t.Helper()

	sent := make(chan Response, 8)
	settings := config.DigestConfig{Enabled: true, At: "07:30", Timezone: "UTC"}

	digest, err := NewDigest(settings, summary, func(response Response) { sent <- response })
	if err != nil {
		t.Fatalf("NewDigest error = %v", err)
	}

	digest.now = clock.now
	digest.after = clock.after
	return digest, sent
}

// digestDay is the day of the digest tests.
var digestDay = time.Date(2026, time.November, 8, 0, 0, 0, 0, time.UTC)

// at returns hour:minute on digestDay.
func at(hour, minute int) time.Time {
	return digestDay.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
}

// TestDigestFiresOnceAtTheConfiguredTime walks the clock past 07:30 with an
// early wake-up on the way and checks one message, then the stop on cancel.
func TestDigestFiresOnceAtTheConfiguredTime(t *testing.T) {
	clock := newFakeClock(at(7, 0))
	digest, sent := newTestDigest(t, clock, func() Response { return Response{Title: digestTitle} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	digest.Start(ctx)

	first := clock.nextWait(t)
	if first.duration != 30*time.Minute {
		t.Errorf("first wait = %s, want 30m", first.duration)
	}

	// A timer that fires a minute early must not send anything.
	clock.set(at(7, 29))
	first.fire <- clock.now()

	second := clock.nextWait(t)
	if second.duration != time.Minute {
		t.Errorf("second wait = %s, want 1m", second.duration)
	}
	if len(sent) != 0 {
		t.Fatalf("sent %d before 07:30, want 0", len(sent))
	}

	clock.set(at(7, 30))
	second.fire <- clock.now()

	// The next timer is asked for after the send, so by now it happened.
	third := clock.nextWait(t)
	if third.duration != maxDigestWait {
		t.Errorf("third wait = %s, want the %s cap", third.duration, maxDigestWait)
	}
	if len(sent) != 1 {
		t.Fatalf("sent %d at 07:30, want 1", len(sent))
	}

	clock.set(at(8, 30))
	third.fire <- clock.now()
	clock.nextWait(t)

	if len(sent) != 1 {
		t.Errorf("sent %d after 08:30, want still 1", len(sent))
	}

	cancel()

	select {
	case <-digest.done:
	case <-time.After(2 * time.Second):
		t.Fatal("the digest goroutine did not stop on ctx cancel")
	}
}

// TestDigestStop checks that Stop returns only once the goroutine is gone and
// that a second Stop is harmless.
func TestDigestStop(t *testing.T) {
	clock := newFakeClock(at(7, 0))
	digest, _ := newTestDigest(t, clock, func() Response { return Response{} })

	digest.Start(context.Background())
	clock.nextWait(t)
	done := digest.done

	digest.Stop()

	select {
	case <-done:
	default:
		t.Fatal("Stop returned before the goroutine ended")
	}

	digest.Stop()
}

// TestDigestSurvivesAPanic checks that a summary that panics does not end the
// schedule.
func TestDigestSurvivesAPanic(t *testing.T) {
	clock := newFakeClock(at(7, 29))
	digest, _ := newTestDigest(t, clock, func() Response { panic("boom") })

	digest.Start(context.Background())
	defer digest.Stop()

	wait := clock.nextWait(t)
	clock.set(at(7, 30))
	wait.fire <- clock.now()

	next := clock.nextWait(t)
	if next.duration != maxDigestWait {
		t.Errorf("wait after the panic = %s, want %s", next.duration, maxDigestWait)
	}
}

// TestDigestDisabledDoesNotStart checks that a disabled digest starts no
// goroutine.
func TestDigestDisabledDoesNotStart(t *testing.T) {
	digest, err := NewDigest(config.DigestConfig{}, func() Response { return Response{} }, func(Response) {})
	if err != nil {
		t.Fatalf("NewDigest error = %v", err)
	}

	digest.Start(context.Background())
	defer digest.Stop()

	if digest.done != nil {
		t.Error("a disabled digest started its goroutine")
	}
}

// TestDigestNextRun checks the default time, a timezone and the next day.
func TestDigestNextRun(t *testing.T) {
	madrid, err := NewDigest(config.DigestConfig{Enabled: true, Timezone: "Europe/Madrid"}, nil, nil)
	if err != nil {
		t.Fatalf("NewDigest error = %v", err)
	}

	// 07:30 in Madrid is 06:30 UTC in November.
	got := madrid.nextRun(at(6, 0))
	if want := at(6, 30); !got.Equal(want) {
		t.Errorf("nextRun(06:00 UTC) = %s, want %s", got, want)
	}

	// At the exact time, the next run is the next day.
	got = madrid.nextRun(at(6, 30))
	if want := at(6, 30).Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("nextRun(06:30 UTC) = %s, want %s", got, want)
	}
}

// TestNewDigestRejectsBadSettings checks the time and timezone errors.
func TestNewDigestRejectsBadSettings(t *testing.T) {
	for _, settings := range []config.DigestConfig{
		{Enabled: true, At: "7h30"},
		{Enabled: true, At: "25:00"},
		{Enabled: true, Timezone: "Mars/Olympus"},
	} {
		if _, err := NewDigest(settings, nil, nil); err == nil {
			t.Errorf("NewDigest(%+v) error = nil, want an error", settings)
		}
	}
}

// TestOperationsSettingsLoadFromYAML checks the docker and digest sections
// of the configuration, and that leaving them out keeps the digest off.
func TestOperationsSettingsLoadFromYAML(t *testing.T) {
	cfg, err := config.ParseYAML([]byte(`
docker:
  allowed_containers: [web, Immich_Server]
digest:
  enabled: true
  at: "06:45"
  timezone: Europe/Madrid
`))
	if err != nil {
		t.Fatalf("ParseYAML error = %v", err)
	}

	if !slices.Equal(cfg.Docker.AllowedContainers, []string{"web", "Immich_Server"}) {
		t.Errorf("allowed_containers = %q", cfg.Docker.AllowedContainers)
	}
	want := config.DigestConfig{Enabled: true, At: "06:45", Timezone: "Europe/Madrid"}
	if cfg.Digest != want {
		t.Errorf("digest = %+v, want %+v", cfg.Digest, want)
	}

	empty, err := config.ParseYAML([]byte("defaults:\n  log_lines: 10\n"))
	if err != nil {
		t.Fatalf("ParseYAML error = %v", err)
	}
	if empty.Digest.Enabled || len(empty.Docker.AllowedContainers) != 0 {
		t.Errorf("defaults = %+v %+v, want the digest off and no containers", empty.Digest, empty.Docker)
	}
}
