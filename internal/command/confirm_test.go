package command

import (
	"strconv"
	"testing"
	"time"
)

// testClock is a clock the tests move by hand.
type testClock struct {
	now time.Time
}

// read returns the current time of the clock, for NewConfirmations.
func (clock *testClock) read() time.Time {
	return clock.now
}

// newTestConfirmations returns an empty store on a clock that starts at a
// fixed time.
func newTestConfirmations() (*Confirmations, *testClock) {
	clock := &testClock{now: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)}
	return NewConfirmations(clock.read), clock
}

// rebootVPS is the dangerous command most tests ask to confirm.
var rebootVPS = Command{Action: ActionReboot, Target: "vps", Raw: "reboot vps"}

func TestConfirmationCodeHasFourDigits(t *testing.T) {
	confirmations, _ := newTestConfirmations()

	for range 200 {
		code, _ := confirmations.Request(rebootVPS)

		number, err := strconv.Atoi(code)
		if err != nil || len(code) != 4 || number < 1000 {
			t.Fatalf("code %q, want four digits without a leading zero", code)
		}
	}
}

func TestTakeReturnsThePendingCommandOnce(t *testing.T) {
	confirmations, clock := newTestConfirmations()
	code, replaced := confirmations.Request(rebootVPS)
	if replaced != nil {
		t.Errorf("first request replaced %+v, want nothing", *replaced)
	}

	clock.now = clock.now.Add(59 * time.Second)
	command, ok := confirmations.Take(code)
	if !ok || command != rebootVPS {
		t.Fatalf("Take(%q) = %+v, %v; want the reboot", code, command, ok)
	}

	// The code is single use.
	if _, ok := confirmations.Take(code); ok {
		t.Error("the same code worked twice")
	}
}

func TestTakeWorksUntilTheWindowEnds(t *testing.T) {
	confirmations, clock := newTestConfirmations()
	code, _ := confirmations.Request(rebootVPS)

	clock.now = clock.now.Add(ConfirmationWindow)
	if _, ok := confirmations.Take(code); !ok {
		t.Errorf("code refused exactly %s after the request, want it still valid", ConfirmationWindow)
	}
}

func TestTakeRefusesAnExpiredCode(t *testing.T) {
	confirmations, clock := newTestConfirmations()
	code, _ := confirmations.Request(rebootVPS)

	clock.now = clock.now.Add(61 * time.Second)
	if _, ok := confirmations.Take(code); ok {
		t.Error("code accepted 61 s after the request")
	}
}

func TestTakeKeepsThePendingCommandAfterAWrongCode(t *testing.T) {
	confirmations, _ := newTestConfirmations()
	code, _ := confirmations.Request(rebootVPS)

	if _, ok := confirmations.Take(wrongCode(code)); ok {
		t.Fatal("a wrong code was accepted")
	}
	if _, ok := confirmations.Take(""); ok {
		t.Fatal("an empty code was accepted")
	}

	// A typo must not cost the user the pending command.
	if _, ok := confirmations.Take(code); !ok {
		t.Error("the right code was refused after a wrong one")
	}
}

func TestTakeWithNothingPending(t *testing.T) {
	confirmations, _ := newTestConfirmations()

	if _, ok := confirmations.Take("1234"); ok {
		t.Error("Take accepted a code with nothing pending")
	}
}

func TestRequestReplacesThePendingCommand(t *testing.T) {
	confirmations, _ := newTestConfirmations()
	shutdownVPS := Command{Action: ActionShutdown, Target: "vps", Raw: "shutdown vps"}

	firstCode, _ := confirmations.Request(rebootVPS)
	secondCode, replaced := confirmations.Request(shutdownVPS)

	if replaced == nil || *replaced != rebootVPS {
		t.Fatalf("second request replaced %v, want the reboot", replaced)
	}
	if secondCode == firstCode {
		t.Errorf("the new code %q is the replaced one", secondCode)
	}
	if _, ok := confirmations.Take(firstCode); ok {
		t.Error("the replaced command's code still works")
	}

	command, ok := confirmations.Take(secondCode)
	if !ok || command != shutdownVPS {
		t.Errorf("Take(%q) = %+v, %v; want the shutdown", secondCode, command, ok)
	}
}

func TestRequestAfterExpiryReplacesNothing(t *testing.T) {
	confirmations, clock := newTestConfirmations()
	confirmations.Request(rebootVPS)

	// An expired command is gone already; the reply must not mention it.
	clock.now = clock.now.Add(61 * time.Second)
	if _, replaced := confirmations.Request(rebootVPS); replaced != nil {
		t.Errorf("request after expiry replaced %+v, want nothing", *replaced)
	}
}

// wrongCode returns a four-digit code that differs from code.
func wrongCode(code string) string {
	if code == "1000" {
		return "1001"
	}
	return "1000"
}
