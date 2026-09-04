// Package lock provides the flock-based single-instance guard shared by
// `comms sync` and `comms run`: only one process may touch the state DB and
// archive tree at a time.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"golang.org/x/sys/unix"
)

// Acquire takes an exclusive non-blocking flock on <stateDir>/lock and
// returns an idempotent release function. flock associates the lock with
// the open file description, so a second Acquire — from another process or
// this one — fails immediately with a clear "another comms instance is
// running" error instead of blocking. The lock also dies with the process,
// so a crash can never leave it stuck.
func Acquire(stateDir string) (func(), error) {
	path := filepath.Join(stateDir, "lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("another comms instance is running (lock %s is held) — stop it or wait for it to finish", path)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	// Best-effort PID note so a human inspecting the lock file can find
	// the holder; the flock itself is the real mutex.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)

	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
			_ = f.Close()
		})
	}
	return release, nil
}
