package executor

import (
	"context"
	"time"
)

// Result holds the outcome of running a command.
type Result struct {
	Output   string
	ExitCode int
	Duration time.Duration
}

// Executor is the interface for running external commands.
type Executor interface {
	Run(ctx context.Context, name string, args ...string) (Result, error)
}
