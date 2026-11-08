package command

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/drolosoft/gotify-commander/internal/config"
	"github.com/drolosoft/gotify-commander/internal/executor"
)

// recordingExecutor records the argv of every Run and answers through
// respond, so a test can check exactly what would have been executed.
type recordingExecutor struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(argv []string) (executor.Result, error)
}

// Run records name and args and returns what respond decides.
func (r *recordingExecutor) Run(ctx context.Context, name string, args ...string) (executor.Result, error) {
	argv := append([]string{name}, args...)

	r.mu.Lock()
	r.calls = append(r.calls, argv)
	r.mu.Unlock()

	if r.respond == nil {
		return executor.Result{}, nil
	}
	return r.respond(argv)
}

// recorded returns a copy of the argv lists seen so far.
func (r *recordingExecutor) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([][]string(nil), r.calls...)
}

// newRecordingBuiltins returns Builtins over the test config and a recorder.
func newRecordingBuiltins(respond func(argv []string) (executor.Result, error)) (*Builtins, *recordingExecutor) {
	recorder := &recordingExecutor{respond: respond}
	return NewBuiltins(testConfig(), recorder), recorder
}

// argvContains reports whether any word of argv equals word.
func argvContains(argv []string, word string) bool {
	for _, candidate := range argv {
		if candidate == word {
			return true
		}
	}
	return false
}

// assertNoInterpolatedShell fails when a call runs a shell whose script is
// not one of the fixed scripts, or when text appears inside a script.
func assertNoInterpolatedShell(t *testing.T, calls [][]string, text string) {
	t.Helper()

	for _, argv := range calls {
		isShell := argv[0] == "bash" || argv[0] == "sh"
		if !isShell {
			continue
		}

		if len(argv) < 3 || argv[1] != "-c" || argv[2] != trafficFilterScript {
			t.Errorf("shell run with a script that is not a fixed constant: %q", argv)
			continue
		}
		if text != "" && strings.Contains(argv[2], text) {
			t.Errorf("%q was interpolated into the shell script: %q", text, argv)
		}
	}
}

// ─── Certs ──────────────────────────────────────────────────────────────────

// TestCertsRunsFindAndOpensslDirectly checks that certs lists the
// certificates sorted by path without any shell in between.
func TestCertsRunsFindAndOpensslDirectly(t *testing.T) {
	builtins, recorder := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		if argvContains(argv, "find") {
			output := "/etc/letsencrypt/live/zeta.example/cert.pem\n" +
				"find: '/etc/letsencrypt/live/broken': Permission denied\n" +
				"/etc/letsencrypt/live/alpha.example/cert.pem\n"
			return executor.Result{Output: output}, nil
		}
		if strings.Contains(argv[len(argv)-1], "alpha") {
			return executor.Result{Output: "notAfter=Jan  2 03:04:05 2027 GMT\n"}, nil
		}
		return executor.Result{Output: "notAfter=Dec 31 23:59:59 2026 GMT\n"}, nil
	})

	response := builtins.Certs(Command{Action: ActionCerts})

	want := "🔒 SSL Certificates\nalpha.example: Jan  2 03:04:05 2027 GMT\nzeta.example: Dec 31 23:59:59 2026 GMT"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}

	calls := recorder.recorded()
	assertNoInterpolatedShell(t, calls, "")

	wantFind := []string{"sudo", "find", "/etc/letsencrypt/live", "-name", "cert.pem"}
	if strings.Join(calls[0], " ") != strings.Join(wantFind, " ") {
		t.Errorf("first call = %q, want %q", calls[0], wantFind)
	}
	wantOpenssl := []string{"sudo", "openssl", "x509", "-enddate", "-noout", "-in", "/etc/letsencrypt/live/alpha.example/cert.pem"}
	if strings.Join(calls[1], " ") != strings.Join(wantOpenssl, " ") {
		t.Errorf("second call = %q, want %q", calls[1], wantOpenssl)
	}
}

// TestCertsNoCertificates keeps the message shown when nothing is found.
func TestCertsNoCertificates(t *testing.T) {
	builtins, _ := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		return executor.Result{Output: "find: '/etc/letsencrypt/live': No such file or directory\n", ExitCode: 1}, nil
	})

	response := builtins.Certs(Command{Action: ActionCerts})

	if !strings.Contains(response.Message, "No certificates found in /etc/letsencrypt/live/") {
		t.Errorf("unexpected message %q", response.Message)
	}
}

// ─── Traffic ────────────────────────────────────────────────────────────────

// rhitSample is rhit output with colour codes, a carriage-return progress
// line and blank lines, all of which the command strips.
const rhitSample = "\x1b[1m42 hits\x1b[0m\r12 / 300\r\n\n  date       hits\n   \n2026/11/01   42\n"

// TestTrafficSitePassesDomainAsArgument checks that the domain only reaches
// the fixed filter script as a positional argument, and that rhit runs
// directly on the filtered temp file.
func TestTrafficSitePassesDomainAsArgument(t *testing.T) {
	builtins, recorder := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		if argvContains(argv, "rhit") {
			return executor.Result{Output: rhitSample}, nil
		}
		return executor.Result{}, nil
	})
	builtins.cfg.Services["nginx"] = withDomain(builtins.cfg.Services["nginx"], "example.com")

	response := builtins.Traffic(Command{Action: ActionTraffic, Target: "nginx"})

	calls := recorder.recorded()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %q", calls)
	}
	assertNoInterpolatedShell(t, calls, "example.com")

	filter := calls[0]
	if filter[len(filter)-2] != "example.com" {
		t.Errorf("domain is not a positional argument: %q", filter)
	}
	tempPath := filter[len(filter)-1]

	rhit := calls[1]
	if rhit[0] != "sudo" || rhit[1] != "rhit" || rhit[2] != tempPath {
		t.Errorf("rhit did not run directly on the temp file: %q", rhit)
	}

	want := "📈 example.com (last 3 months)\n42 hits\n  date       hits\n2026/11/01   42\n"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}

	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("temp file %s still exists after traffic returned", tempPath)
	}
}

// TestTrafficRemovesTempFileOnTimeout checks that the temp file goes away
// when rhit is killed by the deadline.
func TestTrafficRemovesTempFileOnTimeout(t *testing.T) {
	var tempPath string
	builtins, _ := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		if argvContains(argv, "rhit") {
			return executor.Result{}, context.DeadlineExceeded
		}

		tempPath = argv[len(argv)-1]
		if _, err := os.Stat(tempPath); err != nil {
			t.Errorf("temp file does not exist while filtering: %v", err)
		}
		return executor.Result{}, nil
	})

	response := builtins.Traffic(Command{Action: ActionTraffic, Target: "laporra"})

	if !strings.Contains(response.Title, "Traffic Failed") {
		t.Errorf("expected a failure response, got %q", response.Title)
	}
	if tempPath == "" {
		t.Fatal("the filter step never ran")
	}
	if _, err := os.Stat(tempPath); !os.IsNotExist(err) {
		t.Errorf("temp file %s survived the timeout", tempPath)
	}
}

// TestTrafficSiteWithoutMatches keeps the message for a domain with no hits:
// grep exits 1 and rhit never runs.
func TestTrafficSiteWithoutMatches(t *testing.T) {
	builtins, recorder := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		return executor.Result{ExitCode: 1}, nil
	})

	response := builtins.Traffic(Command{Action: ActionTraffic, Target: "laporra"})

	if response.Message != "📈 laporra (last 3 months)\nNo traffic found for laporra" {
		t.Errorf("unexpected message %q", response.Message)
	}
	if calls := recorder.recorded(); len(calls) != 1 {
		t.Errorf("rhit ran without matches: %q", calls)
	}
}

// TestTrafficRejectsShellSyntax checks that a service name carrying shell
// syntax is refused before anything runs.
func TestTrafficRejectsShellSyntax(t *testing.T) {
	for _, target := range []string{"nginx;id", "$(id)", "`id`", "a b"} {
		builtins, recorder := newRecordingBuiltins(nil)

		response := builtins.Traffic(Command{Action: ActionTraffic, Target: target})

		if !strings.Contains(response.Title, "Traffic Failed") {
			t.Errorf("target %q was not refused: %q", target, response.Title)
		}
		if calls := recorder.recorded(); len(calls) != 0 {
			t.Errorf("target %q reached the executor: %q", target, calls)
		}
	}
}

// TestTrafficAllRunsRhitDirectly checks the all-sites report.
func TestTrafficAllRunsRhitDirectly(t *testing.T) {
	builtins, recorder := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		return executor.Result{Output: rhitSample}, nil
	})

	response := builtins.Traffic(Command{Action: ActionTraffic})

	calls := recorder.recorded()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %q", calls)
	}
	argv := calls[0]
	if argv[0] != "sudo" || argv[1] != "rhit" || argv[2] != "/var/log/nginx/" {
		t.Errorf("unexpected argv %q", argv)
	}
	if !argvContains(argv, "--color") || !argvContains(argv, "date,status,ref") {
		t.Errorf("rhit flags missing: %q", argv)
	}

	want := "📈 All Traffic (last 3 months)\n42 hits\n  date       hits\n2026/11/01   42\n"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}
}

// ─── Analytics ──────────────────────────────────────────────────────────────

// goaccessSample is the start of goaccess CSV output: the general summary
// rows, then a panel row the command ignores.
const goaccessSample = "\"0\",,\"general\",,,,,,,,\"2026-11-08 10:00:00 +0000\",\"date_time\"\r\n" +
	"\"1\",,\"general\",,,,,,,,\"1234\",\"total_requests\"\r\n" +
	"\"2\",,\"general\",,,,,,,,\"56\",\"unique_visitors\"\r\n" +
	"\"3\",,\"general\",,,,,,,,\"78\",\"unique_files\"\r\n" +
	"\"4\",,\"general\",,,,,,,,\"9999\",\"bandwidth\"\r\n" +
	"\"5\",,\"general\",,,,,,,,\"3\",\"unique_not_found\"\r\n" +
	"\"6\",,\"general\",,,,,,,,\"x\",\"log_path\"\r\n" +
	"\"0\",,\"visitors\",\"10\",\"0.81\",\"5\",\"0.9\",\"100\",\"0.5\",\"0\",\"20261108\"\r\n"

// TestAnalyticsParsesGoaccessInGo checks that goaccess runs directly and its
// CSV becomes the same summary lines the python script printed.
func TestAnalyticsParsesGoaccessInGo(t *testing.T) {
	builtins, recorder := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		return executor.Result{Output: goaccessSample}, nil
	})

	response := builtins.Analytics(Command{Action: ActionAnalytics})

	want := "📊 Web Analytics (today)\n\n" +
		"Generated: 2026-11-08 10:00:00 +0000\n" +
		"Total requests: 1234\n" +
		"Unique visitors: 56\n" +
		"Unique pages: 78\n" +
		"Bandwidth: 9999 bytes\n" +
		"404 errors: 3"
	if response.Message != want {
		t.Errorf("message = %q, want %q", response.Message, want)
	}

	calls := recorder.recorded()
	assertNoInterpolatedShell(t, calls, "")
	if calls[0][0] != "sudo" || calls[0][1] != "goaccess" || !argvContains(calls[0], "-") {
		t.Errorf("unexpected argv %q", calls[0])
	}
}

// TestAnalyticsWithoutData keeps the hint shown when goaccess prints nothing
// useful, such as an error message.
func TestAnalyticsWithoutData(t *testing.T) {
	builtins, _ := newRecordingBuiltins(func(argv []string) (executor.Result, error) {
		return executor.Result{Output: "sudo: goaccess: command not found\n", ExitCode: 1}, nil
	})

	response := builtins.Analytics(Command{Action: ActionAnalytics})

	if !strings.Contains(response.Message, "No analytics data available. Is goaccess installed?") {
		t.Errorf("unexpected message %q", response.Message)
	}
}

// ─── Locate ─────────────────────────────────────────────────────────────────

// TestLocateRejectsInvalidCoordinates checks that out-of-range or non-finite
// coordinates are refused before any request is made.
func TestLocateRejectsInvalidCoordinates(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
	}))
	defer server.Close()

	inputs := []string{
		"locate 91 0",
		"locate -90.5 0",
		"locate 0 180.1",
		"locate 0 -181",
		"locate NaN 0",
		"locate 0 Inf",
		"locate 1e400 0",
		"locate abc 0",
	}
	for _, raw := range inputs {
		builtins, _ := newRecordingBuiltins(nil)
		builtins.nominatimURL = server.URL

		response := builtins.Locate(Command{Action: ActionLocate, Raw: raw})

		if response.Priority < 5 || !strings.Contains(response.Message, "invalid") {
			t.Errorf("%q was not refused: %+v", raw, response)
		}
	}

	if requests != 0 {
		t.Errorf("%d requests reached the geocoder", requests)
	}
}

// TestLocateFormatsAndEscapesOSMText checks the request Nominatim receives
// and that OSM text cannot inject markdown into the message.
func TestLocateFormatsAndEscapesOSMText(t *testing.T) {
	var gotQuery, gotUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotUserAgent = r.Header.Get("User-Agent")

		place := map[string]any{
			"lat": "40.4167047",
			"lon": "-3.7035825",
			"address": map[string]string{
				"house_number":  "1",
				"road":          "Calle *Mayor*",
				"neighbourhood": "Sol",
				"city":          "Madrid [x](javascript:alert(1))",
				"state":         "Comunidad\nde Madrid",
				"postcode":      "28013",
				"country":       "España <b>",
				"country_code":  "es",
			},
		}
		json.NewEncoder(w).Encode(place)
	}))
	defer server.Close()

	builtins, _ := newRecordingBuiltins(nil)
	builtins.nominatimURL = server.URL

	response := builtins.Locate(Command{Action: ActionLocate, Raw: "locate 40.4168 -3.7038"})

	if gotUserAgent != "gotify-commander/"+Version+" (+https://github.com/drolosoft/gotify-commander)" {
		t.Errorf("User-Agent = %q", gotUserAgent)
	}
	for _, part := range []string{"lat=40.4168", "lon=-3.7038", "format=json", "zoom=18", "addressdetails=1"} {
		if !strings.Contains(gotQuery, part) {
			t.Errorf("query %q is missing %q", gotQuery, part)
		}
	}

	want := "## 📍 40.4168, -3.7038\n\n" +
		`**Address:** 1 Calle \*Mayor\*, Sol, Madrid \[x\]\(javascript:alert\(1\)\), Comunidad de Madrid, 28013, España \<b\>` + "\n" +
		"**Coordinates:** 40.4167047, -3.7035825\n" +
		`**Street:** 1 Calle \*Mayor\*` + "\n" +
		"**Neighbourhood:** Sol\n" +
		`**City:** Madrid \[x\]\(javascript:alert\(1\)\)` + "\n" +
		"**State:** Comunidad de Madrid\n" +
		"**Postal code:** 28013\n" +
		`**Country:** España \<b\>` + "\n" +
		"**Country code:** ES\n" +
		"\n" +
		"[Open in map](https://www.openstreetmap.org/?mlat=40.4167047&mlon=-3.7035825&zoom=17)\n" +
		"\n" +
		"![map](https://tile.openstreetmap.org/15/16046/12355.png)"
	if response.Message != want {
		t.Errorf("message mismatch\n got: %q\nwant: %q", response.Message, want)
	}
	if !response.Markdown || response.Title != "📍 Location" {
		t.Errorf("unexpected response header %+v", response)
	}
}

// TestLocateNotFound checks that Nominatim's error answer for a point with
// no address becomes "Location not found".
func TestLocateNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"Unable to geocode"}`))
	}))
	defer server.Close()

	builtins, _ := newRecordingBuiltins(nil)
	builtins.nominatimURL = server.URL

	response := builtins.Locate(Command{Action: ActionLocate, Raw: "locate 0 -30"})

	if response.Message != "## 📍 0, -30\n\nLocation not found" {
		t.Errorf("unexpected message %q", response.Message)
	}
}

// TestLocateErrorHidesCoordinates checks that a failed request is reported
// without the request URL, which carries the coordinates.
func TestLocateErrorHidesCoordinates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	builtins, _ := newRecordingBuiltins(nil)
	builtins.nominatimURL = server.URL

	response := builtins.Locate(Command{Action: ActionLocate, Raw: "locate 12.345 67.891"})

	if !strings.Contains(response.Title, "Locate Failed") {
		t.Fatalf("expected a failure, got %+v", response)
	}
	if strings.Contains(response.Message, "12.345") || strings.Contains(response.Message, "67.891") {
		t.Errorf("error message leaks the coordinates: %q", response.Message)
	}
}

// withDomain returns svc with its Domain set, since config.Service is a value
// stored in the map.
func withDomain(svc config.Service, domain string) config.Service {
	svc.Domain = domain
	return svc
}
