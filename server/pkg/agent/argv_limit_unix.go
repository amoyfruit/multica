//go:build unix

package agent

import (
	"errors"
	"syscall"
)

// isArgvLimitErr matches the E2BIG that fork/exec returns for an oversized
// argv/env. Go wraps it in a *fs.PathError; errors.Is sees through.
func isArgvLimitErr(err error) bool {
	return errors.Is(err, syscall.E2BIG)
}
