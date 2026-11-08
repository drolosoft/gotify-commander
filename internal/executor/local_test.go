package executor

import (
	"context"
	"errors"
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

// TestRunTimeoutKillsPipeline checks that a timeout kills the whole process
// group. Without it, bash dies but "sleep" keeps the output pipe open and Run
// waits the full six seconds.
func TestRunTimeoutKillsPipeline(t *testing.T) {
	ex := NewLocalExecutor()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	start := time.Now()
	_, err := ex.Run(ctx, "bash", "-c", "sleep 6 | cat")
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected a deadline error, got %v", err)
	}
	if elapsed > 2500*time.Millisecond {
		t.Fatalf("Run took %s, the pipeline was not killed", elapsed)
	}
}

// TestRunCapsOutput checks that a command printing more than the cap is cut
// at maxOutputBytes and carries the truncation marker.
func TestRunCapsOutput(t *testing.T) {
	ex := NewLocalExecutor()

	result, err := ex.Run(context.Background(), "bash", "-c", `head -c 200000 /dev/zero | tr "\0" a`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(result.Output) > maxOutputBytes+len(truncatedMarker) {
		t.Fatalf("output is %d bytes, more than the %d byte cap plus marker", len(result.Output), maxOutputBytes)
	}
	if !strings.HasSuffix(result.Output, truncatedMarker) {
		t.Fatalf("output does not end with the truncation marker: %q", result.Output[len(result.Output)-40:])
	}
	if result.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", result.ExitCode)
	}
}

// TestLimitedBufferKeepsSmallOutput checks that output under the cap passes
// through untouched and without a marker.
func TestLimitedBufferKeepsSmallOutput(t *testing.T) {
	buffer := &limitedBuffer{limit: 8}

	written, err := buffer.Write([]byte("hello"))
	if err != nil || written != 5 {
		t.Fatalf("Write returned (%d, %v), want (5, nil)", written, err)
	}

	if buffer.String() != "hello" {
		t.Errorf("got %q, want %q", buffer.String(), "hello")
	}
}

// TestLimitedBufferDropsOverflow checks that writes past the cap report full
// success, so the child never sees a short write, while the excess is dropped.
func TestLimitedBufferDropsOverflow(t *testing.T) {
	buffer := &limitedBuffer{limit: 8}

	buffer.Write([]byte("hello"))
	written, err := buffer.Write([]byte(" world"))
	if err != nil || written != 6 {
		t.Fatalf("Write returned (%d, %v), want (6, nil)", written, err)
	}

	if !buffer.truncated {
		t.Error("expected the truncated flag to be set")
	}
	if buffer.String() != "hello wo"+truncatedMarker {
		t.Errorf("got %q", buffer.String())
	}
}
