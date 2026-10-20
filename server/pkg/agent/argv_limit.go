package agent

import (
	"errors"
	"fmt"
)

// ErrArgvLimitExceeded names the failure class where an agent process cannot
// start because the OS refuses its command line. The classic trigger is the
// task prompt inlined as a single argv string: on Linux that hits
// MAX_ARG_STRLEN (32 * PAGE_SIZE = 131072 bytes), and the task dies ~140 ms
// after spawn with zero output and an opaque E2BIG "argument list too long"
// (GH #9354-class incident: a 145 KB log pasted into a thread produced a
// 147 KB prompt and a dead run).
//
// Two layers name it:
//
//   - Backends that still hand the prompt to their CLI as one argv element
//     call inlinePromptArgvLimitError before spawning (antigravity today;
//     copilot was moved to stdin and keeps a regression test against
//     regressing).
//   - Every process this package starts runs its Start error through
//     wrapArgvLimitError, so an E2BIG from ANY source — a bloated
//     custom_args entry included — becomes this named error instead of a
//     raw errno string.
//
// Callers can errors.Is against it; the text carries the operator guidance.
var ErrArgvLimitExceeded = errors.New(
	"argument list too long: the agent process cannot be spawned because a command-line argument exceeds the OS limit; large inline prompts are the usual cause — attach big logs or files to the issue instead of pasting them inline")

// wrapArgvLimitError renames the OS's oversized-command-line refusal at the
// single point where the package starts processes. A non-argv-limit error
// (or nil) passes through unchanged.
func wrapArgvLimitError(err error) error {
	if err == nil || !isArgvLimitErr(err) {
		return err
	}
	// Both errors stay on the chain: errors.Is finds the named class AND the
	// raw E2BIG a diagnostics-minded caller may look for.
	return fmt.Errorf("%w (underlying error: %w)", ErrArgvLimitExceeded, err)
}

// inlinePromptArgvLimitError is the pre-spawn guard for backends that deliver
// the prompt as a single argv element. maxInlinePromptArgBytes is the
// platform's per-argument ceiling, or 0 where the platform documents no
// per-argument cap (then only the post-spawn E2BIG wrap applies).
//
// It exists so an over-limit prompt fails BEFORE a doomed fork/exec, with the
// byte counts and the remedy in the same error the member sees on the issue.
func inlinePromptArgvLimitError(backend, prompt string) error {
	if maxInlinePromptArgBytes <= 0 || len(prompt) <= maxInlinePromptArgBytes {
		return nil
	}
	return fmt.Errorf("%s prompt is %d bytes, over this OS's %d-byte command-line argument limit: %w",
		backend, len(prompt), maxInlinePromptArgBytes, ErrArgvLimitExceeded)
}
