//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// lockFile acquires an exclusive advisory lock on path + ".lock".
// Returns an unlock function that releases the lock and removes the lock file.
func lockFile(path string) (unlock func(), err error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		_ = os.Remove(lockPath)
	}, nil
}
