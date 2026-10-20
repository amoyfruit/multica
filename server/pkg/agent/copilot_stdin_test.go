//go:build unix

package agent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copilotStreamTail is the minimal successful Copilot JSONL stream the fake
// CLI below emits once it has recorded what the backend handed it.
const copilotStreamTail = `printf '%s\n' '{"type":"assistant.message","data":{"messageId":"m1","content":"ok"}}'
printf '%s\n' '{"type":"result","sessionId":"sess-stdin-test","exitCode":0}'
`

// copilotStdinProbe runs the Copilot backend against a fake CLI that records
// argv, drains stdin to EOF, and emits a successful Copilot JSONL stream.
// It mirrors opencodeStdinProbe: the two providers share the incident this
// regression guards (prompt as a single argv string dies with E2BIG above
// MAX_ARG_STRLEN), so both get the same byte-for-byte stdin assertion.
func copilotStdinProbe(t *testing.T, prompt string) ([]string, string, Result) {
	t.Helper()

	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv.txt")
	stdinPath := filepath.Join(dir, "stdin.txt")

	script := fmt.Sprintf("#!/bin/sh\n"+
		": > %[1]q\n"+
		"for a in \"$@\"; do printf '%%s\\n' \"$a\" >> %[1]q; done\n"+
		"cat > %[2]q\n", argvPath, stdinPath) + copilotStreamTail

	fakePath := filepath.Join(dir, "copilot")
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := New("copilot", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(copilot): %v", err)
	}
	session, err := backend.Execute(t.Context(), prompt, ExecOptions{
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()
	result := <-session.Result

	argvRaw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	stdinRaw, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read recorded stdin: %v", err)
	}
	argv := strings.Split(strings.TrimSuffix(string(argvRaw), "\n"), "\n")
	return argv, string(stdinRaw), result
}

// TestCopilotExecuteSendsPromptOnStdinNotArgv pins the launch shape: the
// prompt reaches the CLI byte-for-byte on stdin, argv carries only the
// daemon's own flags, and no prompt fragment appears in argv.
func TestCopilotExecuteSendsPromptOnStdinNotArgv(t *testing.T) {
	t.Parallel()

	prompt := "MULTICA_E2BIG_PROBE\n" +
		`{"task": "review the attached thread", "secret": "stdin-only-value"}`

	argv, stdinGot, result := copilotStdinProbe(t, prompt)

	if stdinGot != prompt {
		t.Errorf("prompt did not arrive on stdin intact:\n got  %q\n want %q", stdinGot, prompt)
	}
	for _, arg := range argv {
		if arg == "-p" {
			t.Errorf("-p reappeared in copilot argv: %v", argv)
		}
		if strings.Contains(arg, "stdin-only-value") || strings.Contains(arg, "MULTICA_E2BIG_PROBE") {
			t.Errorf("prompt fragment leaked into argv element %q; argv=%v", arg, argv)
		}
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{"--output-format json", "--allow-all", "--no-ask-user"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in argv, got %v", want, argv)
		}
	}
	if result.Status != "completed" || result.Output != "ok" {
		t.Fatalf("result = %+v, want completed with output ok", result)
	}
}

// e2bigProbePrompt is 1.2 MB — past the 1 MiB acceptance threshold and past
// Linux MAX_ARG_STRLEN (131071), the value that produced the original E2BIG
// task death from a 145 KB log pasted into an issue thread.
var e2bigProbePrompt = strings.Repeat("multica e2big regression thread context line 0123456789\n", 30000)

// requireE2BigProbePrompt guards the probe size against platform drift.
func requireE2BigProbePrompt(t *testing.T) {
	t.Helper()
	if len(e2bigProbePrompt) <= 1<<20 {
		t.Fatalf("probe prompt must exceed 1 MiB, got %d bytes", len(e2bigProbePrompt))
	}
	if maxInlinePromptArgBytes > 0 && len(e2bigProbePrompt) <= maxInlinePromptArgBytes {
		t.Fatalf("probe prompt must exceed the platform argv limit %d, got %d bytes", maxInlinePromptArgBytes, len(e2bigProbePrompt))
	}
}

// TestCopilotExecutePromptAboveOneMegabyte is the incident regression: a
// > 1 MB thread context must spawn successfully. On the old `-p <prompt>`
// argv path the fork/exec itself failed with E2BIG and the task died with
// zero output; on stdin the size of the prompt is no longer a spawn problem.
func TestCopilotExecutePromptAboveOneMegabyte(t *testing.T) {
	t.Parallel()
	requireE2BigProbePrompt(t)

	_, stdinGot, result := copilotStdinProbe(t, e2bigProbePrompt)

	if stdinGot != e2bigProbePrompt {
		t.Errorf("1 MB+ prompt did not arrive on stdin intact: got %d bytes, want %d", len(stdinGot), len(e2bigProbePrompt))
	}
	if result.Status != "completed" || result.Output != "ok" {
		t.Fatalf("result = %+v, want completed with output ok", result)
	}
}

// TestOpencodeExecutePromptAboveOneMegabyte is the same regression on the
// provider that already moved to stdin for #6538, so a future refactor that
// puts either provider's prompt back on argv fails here at the daemon
// boundary instead of in a user's dead task.
func TestOpencodeExecutePromptAboveOneMegabyte(t *testing.T) {
	t.Parallel()
	requireE2BigProbePrompt(t)

	_, stdinGot, result := opencodeStdinProbe(t, e2bigProbePrompt)

	if stdinGot != e2bigProbePrompt {
		t.Errorf("1 MB+ prompt did not arrive on stdin intact: got %d bytes, want %d", len(stdinGot), len(e2bigProbePrompt))
	}
	if result.Status != "completed" || result.Output != "ok" {
		t.Fatalf("result = %+v, want completed with output ok", result)
	}
}

// TestCopilotExecuteRejectsEmptyPrompt — stdin mode without a prompt would
// spawn a CLI with nothing to run; the guard refuses before the spawn.
func TestCopilotExecuteRejectsEmptyPrompt(t *testing.T) {
	t.Parallel()

	fakePath := filepath.Join(t.TempDir(), "copilot")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\nexit 0\n"))

	backend, err := New("copilot", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(copilot): %v", err)
	}
	if _, err := backend.Execute(t.Context(), "", ExecOptions{}); err == nil {
		t.Fatal("expected an error for an empty prompt, got nil")
	} else if !strings.Contains(err.Error(), "empty prompt") {
		t.Fatalf("expected an empty-prompt error, got %v", err)
	}
}
