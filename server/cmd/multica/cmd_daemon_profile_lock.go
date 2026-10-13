package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A fallback port no longer acts as the profile's single-instance guard.
// Keep this OS lock for the daemon's entire lifetime. A successor spawned by
// self-update may wait briefly for its predecessor to release it.
func acquireDaemonProfileLock(profile string, handoff bool) (*os.File, error) {
	dir := daemonDirForProfile(profile)
	if dir == "" {
		return nil, fmt.Errorf("resolve daemon profile directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "daemon.lock")
	deadline := time.Now().Add(5 * time.Second)
	if handoff {
		deadline = deadline.Add(30 * time.Second)
	}
	for {
		f, acquired, err := tryLockDaemonProfile(path)
		if err != nil {
			return nil, fmt.Errorf("lock daemon profile %q: %w", profile, err)
		}
		if acquired {
			return f, nil
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("daemon profile %q is already running or starting", profile)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
