//go:build unix

package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestWrapArgvLimitErrorNamesE2BIG(t *testing.T) {
	t.Parallel()

	// This is the exact shape Go produces for the incident: fork/exec of the
	// CLI with a 147 KB inline prompt, error text "argument list too long".
	raw := &fs.PathError{Op: "fork/exec", Path: "/usr/local/bin/copilot", Err: syscall.E2BIG}

	err := wrapArgvLimitError(raw)
	if !errors.Is(err, ErrArgvLimitExceeded) {
		t.Fatalf("E2BIG must surface as ErrArgvLimitExceeded, got %#v", err)
	}
	if !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("the underlying E2BIG must stay on the error chain, got %v", err)
	}
	for _, want := range []string{"argument list too long", "attach big logs or files to the issue"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error text must contain %q (operator guidance), got %q", want, err.Error())
		}
	}
}

func TestWrapArgvLimitErrorPassesThrough(t *testing.T) {
	t.Parallel()

	for _, in := range []error{nil, exec.ErrNotFound, errors.New("permission denied")} {
		if got := wrapArgvLimitError(in); !errors.Is(got, in) {
			t.Fatalf("wrapArgvLimitError(%v) = %v, want it unchanged", in, got)
		}
	}
}

func TestInlinePromptArgvLimitError(t *testing.T) {
	t.Parallel()

	if maxInlinePromptArgBytes <= 0 {
		t.Skip("platform documents no per-argument cap; guard is compile-time disabled")
	}

	if err := inlinePromptArgvLimitError("antigravity", strings.Repeat("x", maxInlinePromptArgBytes)); err != nil {
		t.Fatalf("a prompt at exactly the %d-byte limit must pass the guard, got %v", maxInlinePromptArgBytes, err)
	}

	err := inlinePromptArgvLimitError("antigravity", strings.Repeat("x", maxInlinePromptArgBytes+1))
	if !errors.Is(err, ErrArgvLimitExceeded) {
		t.Fatalf("a prompt over the limit must fail with ErrArgvLimitExceeded, got %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes", maxInlinePromptArgBytes+1)) {
		t.Errorf("error must name the prompt size, got %q", err.Error())
	}
}

// TestAntigravityExecuteRejectsOversizedPromptBeforeSpawn pins that the
// guard that antigravity keeps (agy has no verified stdin prompt channel)
// fires BEFORE any process is started — the task must fail loudly with the
// named error, not via a doomed fork/exec.
func TestAntigravityExecuteRejectsOversizedPromptBeforeSpawn(t *testing.T) {
	t.Parallel()

	if maxInlinePromptArgBytes <= 0 {
		t.Skip("platform documents no per-argument cap; guard is compile-time disabled")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "spawned")
	fakePath := filepath.Join(dir, "agy")
	writeTestExecutable(t, fakePath, []byte("#!/bin/sh\n: > "+marker+"\n"))

	backend, err := New("antigravity", Config{ExecutablePath: fakePath, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(antigravity): %v", err)
	}

	_, err = backend.Execute(t.Context(), strings.Repeat("x", maxInlinePromptArgBytes+1), ExecOptions{})
	if !errors.Is(err, ErrArgvLimitExceeded) {
		t.Fatalf("oversized prompt must fail with ErrArgvLimitExceeded, got %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("agy was spawned despite the pre-spawn guard")
	}
}
