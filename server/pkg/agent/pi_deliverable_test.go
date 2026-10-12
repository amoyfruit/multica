package agent

import (
	"context"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPiExecuteFailsWhenFinalTurnTruncatedWithNoOutput pins MULT-22 modes 1
// and 3: the model's final turn dies at the output-token limit
// (stop_reason "length") and no text character survives it — the deliverable
// the turn was generating was cut off in flight. The process still exits 0,
// so without the guard the run would report status=completed with empty
// output and the task would complete with nothing delivered.
func TestPiExecuteFailsWhenFinalTurnTruncatedWithNoOutput(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	events := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"turn_end","message":{"role":"assistant","content":[],"model":"test","usage":{"input":1,"output":1},"stopReason":"length"}}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
	}
	fakePath := filepath.Join(t.TempDir(), "pi")
	writeTestExecutable(t, fakePath, []byte(piEventStreamScript(events)))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	select {
	case result := <-session.Result:
		if result.Status != "failed" {
			t.Fatalf("expected status=failed, got %q (error=%q)", result.Status, result.Error)
		}
		if !strings.Contains(result.Error, "final turn truncated: stop_reason length with no output") {
			t.Fatalf("Error: got %q, want the classifier marker", result.Error)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// TestPiExecuteCompletesWhenTruncatedTurnEmittedText pins the guard's gate:
// a truncated final turn that DID emit text is delivering that text, so the
// partial result stays visible (completion publishes it) instead of being
// burned into a retry.
func TestPiExecuteCompletesWhenTruncatedTurnEmittedText(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}

	events := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"partial but real answer"}}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":9},"stopReason":"length"}}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
	}
	fakePath := filepath.Join(t.TempDir(), "pi")
	writeTestExecutable(t, fakePath, []byte(piEventStreamScript(events)))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("expected status=completed, got %q (error=%q)", result.Status, result.Error)
		}
		if result.Output != "partial but real answer" {
			t.Fatalf("Output: got %q, want the partial text preserved", result.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}

// TestPiExecuteCompletesWhenEarlierTruncatedTurnIsSuperseded pins that only
// the FINAL turn's stop reason matters: an earlier turn cut at the output
// limit, followed by a healthy terminal turn, is a normal successful run.
func TestPiExecuteCompletesWhenEarlierTruncatedTurnIsSuperseded(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	events := []string{
		`{"type":"agent_start"}`,
		`{"type":"turn_start"}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":1,"output":1},"stopReason":"length"}}`,
		`{"type":"turn_start"}`,
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"done"}}`,
		`{"type":"turn_end","message":{"role":"assistant","model":"test","usage":{"input":2,"output":1}}}`,
		`{"type":"agent_end","messages":[],"willRetry":false}`,
	}
	fakePath := filepath.Join(t.TempDir(), "pi")
	writeTestExecutable(t, fakePath, []byte(piEventStreamScript(events)))

	backend, err := New("pi", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("new pi backend: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "prompt-ignored", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	select {
	case result := <-session.Result:
		if result.Status != "completed" {
			t.Fatalf("expected status=completed, got %q (error=%q)", result.Status, result.Error)
		}
		if result.Output != "done" {
			t.Fatalf("Output: got %q, want %q", result.Output, "done")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
}
