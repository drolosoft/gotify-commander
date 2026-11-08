package executor

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

// waitDelay is how long Run waits for the output pipes to close after the
// command was killed. A grandchild that escaped the kill would otherwise keep
// them open and hold Run past its deadline.
const waitDelay = 2 * time.Second

// LocalExecutor runs commands on the local machine.
type LocalExecutor struct{}

// NewLocalExecutor returns a new LocalExecutor.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

// Run executes a command with the given context and returns the result.
//
// Behavior:
//   - Context timeout/cancel → returns error (the whole process group is killed).
//   - Non-zero exit code → returns Result with exit code, no error (command ran but failed).
//   - Exec failure (e.g. binary not found) → returns error.
//   - Output beyond maxOutputBytes is dropped and marked as truncated.
func (l *LocalExecutor) Run(ctx context.Context, name string, args ...string) (Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)

	// The command leads its own process group, and cancelling kills the
	// group. Killing only the direct child leaves the rest of a pipeline
	// ("sleep 6 | cat") running and holding the output pipe.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = waitDelay

	// Stdout and Stderr share one pointer, so os/exec never calls Write from
	// two goroutines at once.
	buf := &limitedBuffer{limit: maxOutputBytes}
	cmd.Stdout = buf
	cmd.Stderr = buf

	err := cmd.Run()
	duration := time.Since(start)

	if err != nil {
		// Check if the context was cancelled or timed out — propagate as error.
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}

		// Check if it's an exit error (command ran but returned non-zero).
		if exitErr, ok := err.(*exec.ExitError); ok {
			return Result{
				Output:   buf.String(),
				ExitCode: exitErr.ExitCode(),
				Duration: duration,
			}, nil
		}

		// Exec failure: binary not found, permission denied, etc.
		return Result{}, err
	}

	return Result{
		Output:   buf.String(),
		ExitCode: 0,
		Duration: duration,
	}, nil
}
