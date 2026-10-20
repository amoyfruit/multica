//go:build windows

package agent

import (
	"errors"
	"strings"
	"syscall"
)

// maxInlinePromptArgBytes is deliberately the cmd.exe ceiling, not the
// CreateProcess one: CreateProcessW caps lpCommandLine at 32,767 characters,
// but a .cmd/.bat shim routes the call through cmd.exe, which caps at 8,191
// (#6538 hit exactly that with the Chocolatey opencode shim). A wrapper that
// can pass the full 32,767 is the rarer case, and an early, actionable
// refusal beats discovering which ceiling this host's shim obeys through a
// dead run.
const maxInlinePromptArgBytes = 8191

// isArgvLimitErr matches the two shapes Go surfaces for an over-long Windows
// command line: ERROR_FILENAME_EXCED_RANGE (206) wrapped in a *os.PathError,
// reported as "The filename or extension is too long" — the misleading text
// #6538 documented.
func isArgvLimitErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.Errno(206)) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "filename or extension is too long")
}
