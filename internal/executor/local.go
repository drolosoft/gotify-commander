package executor

import (
	"bytes"
	"context"
	"os/exec"
	"time"
)

// LocalExecutor runs commands on the local machine.
type LocalExecutor struct{}

// NewLocalExecutor returns a new LocalExecutor.
func NewLocalExecutor() *LocalExecutor {
	return &LocalExecutor{}
}

// Run executes a command with the given context and returns the result.
//
// Behavior:
//   - Context timeout/cancel → returns error (command was killed).
//   - Non-zero exit code → returns Result with exit code, no error (command ran but failed).
//   - Exec failure (e.g. binary not found) → returns error.
func (l *LocalExecutor) Run(ctx context.Context, name string, args ...string) (Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, name, args...)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

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
