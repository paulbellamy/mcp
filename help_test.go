package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestHelpFlags verifies that every subcommand responds to --help/-h with a
// usage string and no error, so agents can look up docs without side effects.
func TestHelpFlags(t *testing.T) {
	setupTestConfigDir(t)

	cases := []struct {
		name string
		run  func() error
		want string
	}{
		{"servers", func() error { return cmdServers([]string{"--help"}) }, "Usage: mcp servers"},
		{"add", func() error { return cmdAdd([]string{"--help"}) }, "Usage: mcp add"},
		{"remove", func() error { return cmdRemove([]string{"--help"}) }, "Usage: mcp remove"},
		{"enable", func() error { return cmdSetEnabled([]string{"--help"}, true) }, "Usage: mcp enable"},
		{"disable", func() error { return cmdSetEnabled([]string{"--help"}, false) }, "Usage: mcp disable"},
		{"tools", func() error { return cmdTools([]string{"--help"}) }, "Usage: mcp tools"},
		{"auth", func() error { return cmdAuth([]string{"--help"}) }, "Usage: mcp auth"},
		{"ping", func() error { return cmdPing([]string{"--help"}) }, "Usage: mcp ping"},
		{"daemon", func() error { return cmdDaemon([]string{"--help"}) }, "Usage: mcp daemon"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			stderr := captureStderr(t, func() { err = tc.run() })
			if err != nil {
				t.Fatalf("--help returned error: %v", err)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("expected %q in --help output, got %q", tc.want, stderr)
			}
		})
	}
}

// TestAuthHelpNoSideEffects verifies that `mcp auth --help` is a pure docs
// lookup: it must not run the expired-pending-auth cleanup, which would delete
// files from disk.
func TestAuthHelpNoSideEffects(t *testing.T) {
	setupTestConfigDir(t)

	// Write a pending-auth file and backdate it past the TTL so the cleanup
	// would remove it if it ran.
	path := pendingAuthPath("somesrv")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * pendingAuthTTL)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	captureStderr(t, func() {
		if err := cmdAuth([]string{"--help"}); err != nil {
			t.Fatal(err)
		}
	})

	if _, err := os.Stat(path); err != nil {
		t.Errorf("auth --help deleted the expired pending file (side effect): %v", err)
	}
}

// TestHelpShortFlag verifies the -h alias works the same as --help.
func TestHelpShortFlag(t *testing.T) {
	setupTestConfigDir(t)

	var err error
	stderr := captureStderr(t, func() { err = cmdAuth([]string{"-h"}) })
	if err != nil {
		t.Fatalf("-h returned error: %v", err)
	}
	if !strings.Contains(stderr, "Usage: mcp auth") {
		t.Errorf("expected usage in -h output, got %q", stderr)
	}
}
