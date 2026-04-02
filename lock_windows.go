//go:build windows

package main

import (
	"fmt"
	"os"
)

// lockFile acquires an exclusive lock on path + ".lock".
// On Windows, os.OpenFile with O_CREATE|O_EXCL provides basic mutual exclusion.
func lockFile(path string) (unlock func(), err error) {
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	return func() {
		f.Close()
		os.Remove(lockPath)
	}, nil
}
