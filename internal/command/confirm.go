package command

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Confirmations holds the one command that waits for "<keyword> <code>". A
// dangerous command sent as a message is not run at once: it is stored here
// with a random code, and only a reply with that code runs it. The store
// lives in memory, so a restart of the plugin forgets the pending command.
// It is safe for concurrent use.
type Confirmations struct {
	mu      sync.Mutex
	pending *pendingConfirmation

	// window is how long a code works after its request.
	window time.Duration

	// now is the clock the expiry is measured on, replaceable so tests
	// can move it instead of waiting a minute.
	now func() time.Time
}

// pendingConfirmation is a command waiting for its code.
type pendingConfirmation struct {
	command Command
	code    string
	expires time.Time
}

// NewConfirmations returns an empty store whose codes work for window and
// that reads the time from now.
func NewConfirmations(window time.Duration, now func() time.Time) *Confirmations {
	return &Confirmations{window: window, now: now}
}

// Request stores command as the pending one and returns its new code. Only
// one command waits at a time, so a pending one that has not expired yet is
// dropped and returned as replaced, for the reply to say so; replaced is nil
// when nothing was waiting.
func (c *Confirmations) Request(command Command) (code string, replaced *Command) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	previousCode := ""
	if c.live(now) {
		previous := c.pending.command
		replaced = &previous
		previousCode = c.pending.code
	}

	// The reply says the old code no longer works, so the new one must not
	// happen to be the same four digits.
	code = newCode()
	for code == previousCode {
		code = newCode()
	}

	c.pending = &pendingConfirmation{
		command: command,
		code:    code,
		expires: now.Add(c.window),
	}

	return code, replaced
}

// Take returns the pending command when code is its code and the window has
// not ended, and forgets it, so each code runs a command once. A wrong code
// leaves the pending command where it is: the confirmation guards against a
// command sent by mistake, not against someone who holds a client token and
// can read the reply anyway, and a typo should not cost the user the request.
func (c *Confirmations) Take(code string) (Command, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.live(c.now()) {
		c.pending = nil
		return Command{}, false
	}

	if code != c.pending.code {
		return Command{}, false
	}

	command := c.pending.command
	c.pending = nil

	return command, true
}

// live reports whether a command is pending and its window is still open at
// now. The caller must hold c.mu.
func (c *Confirmations) live(now time.Time) bool {
	if c.pending == nil {
		return false
	}

	return !now.After(c.pending.expires)
}

// codeRange is how many codes there are: 1000 to 9999. A leading zero is
// left out because the parser would keep "0042" and the user might type
// "42".
var codeRange = big.NewInt(9000)

// newCode returns a random code from 1000 to 9999, read from crypto/rand so
// the next code cannot be guessed from the previous ones.
func newCode() string {
	number, err := rand.Int(rand.Reader, codeRange)

	// Since Go 1.24 the system random source does not fail; if it ever did,
	// running a reboot on a predictable code would be worse than the panic,
	// which the commander recovers and answers as an internal error.
	if err != nil {
		panic(fmt.Sprintf("confirmation code: %v", err))
	}

	return fmt.Sprintf("%d", number.Int64()+1000)
}
