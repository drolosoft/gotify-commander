package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLocalExecutorEcho(t *testing.T) {
	ex := NewLocalExecutor()
	ctx := context.Background()

	result, err := ex.Run(ctx, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
	output := strings.TrimSpace(result.Output)
	if output != "hello" {
		t.Errorf("expected output 'hello', got %q", output)
	}
}

func TestLocalExecutorTimeout(t *testing.T) {
	ex := NewLocalExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, err := ex.Run(ctx, "sleep", "10")
	if err == nil {
		t.Fatal("expected timeout error, got none")
	}
}

func TestLocalExecutorFailure(t *testing.T) {
	ex := NewLocalExecutor()
	ctx := context.Background()

	result, err := ex.Run(ctx, "false")
	if err != nil {
		t.Fatalf("unexpected error for non-zero exit: %v", err)
	}
	if result.ExitCode == 0 {
		t.Error("expected non-zero exit code")
	}
}
