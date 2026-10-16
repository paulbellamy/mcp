package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestCmdListOverview verifies `mcp list` prints one line per configured
// server, in config order: cached tool names, a failing server's error, and
// disabled servers — without failing the command.
func TestCmdListOverview(t *testing.T) {
	setupTestConfigDir(t)

	disabled := false
	if err := saveServers([]ServerConfig{
		{Name: "beta", Transport: "stdio", Command: "beta-bin"},
		{Name: "broken", Transport: "stdio", Command: "/nonexistent/mcp-list-test-server"},
		{Name: "alpha", Transport: "stdio", Command: "alpha-bin"},
		{Name: "off", Transport: "stdio", Command: "off-bin", Enabled: &disabled},
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedTools("beta", []toolOutput{
		{Server: "beta", Name: "zeta", Description: "z"},
		{Server: "beta", Name: "eta", Description: "e"},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedTools("alpha", []toolOutput{{Server: "alpha", Name: "only"}}, nil); err != nil {
		t.Fatal(err)
	}

	var err error
	out := captureStdout(t, func() {
		captureStderr(t, func() { err = cmdList(nil) })
	})
	if err != nil {
		t.Fatalf("cmdList: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d:\n%s", len(lines), out)
	}
	if lines[0] != "beta (2 tools): eta, zeta" {
		t.Errorf("line 0 = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "broken: error: ") || !strings.Contains(lines[1], "nonexistent") {
		t.Errorf("line 1 = %q, want broken server's error", lines[1])
	}
	if lines[2] != "alpha (1 tool): only" {
		t.Errorf("line 2 = %q", lines[2])
	}
	if lines[3] != "off: disabled" {
		t.Errorf("line 3 = %q", lines[3])
	}
}

func TestFormatListLine(t *testing.T) {
	if got := formatListLine("s", nil, errors.New("initialize:\n  transport closed")); got != "s: error: initialize: transport closed" {
		t.Errorf("error line = %q", got)
	}
	if got := formatListLine("s", nil, nil); got != "s (0 tools)" {
		t.Errorf("empty line = %q", got)
	}

	var many []toolOutput
	for i := 0; i < listNameLimit+3; i++ {
		many = append(many, toolOutput{Name: fmt.Sprintf("t%04d", i)})
	}
	got := formatListLine("big", many, nil)
	if !strings.HasPrefix(got, fmt.Sprintf("big (%d tools): t0000, ", listNameLimit+3)) {
		t.Errorf("big line starts %q", got[:40])
	}
	if !strings.HasSuffix(got, ", ... +3 more (mcp tools big --query <word>)") {
		t.Errorf("big line should end with a +3 more hint, got ...%q", got[len(got)-60:])
	}
	if strings.Contains(got, fmt.Sprintf("t%04d", listNameLimit)) {
		t.Errorf("names past the limit should be omitted")
	}
}

func TestCmdListNoServers(t *testing.T) {
	setupTestConfigDir(t)
	var err error
	stderr := captureStderr(t, func() {
		if out := captureStdout(t, func() { err = cmdList(nil) }); out != "" {
			t.Errorf("stdout = %q, want empty", out)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "mcp add") {
		t.Errorf("stderr should point at mcp add, got %q", stderr)
	}
}

func TestCmdListRejectsServerArg(t *testing.T) {
	setupTestConfigDir(t)
	err := cmdList([]string{"alpha"})
	if err == nil || !strings.Contains(err.Error(), "mcp tools <server>") {
		t.Errorf("want error pointing at mcp tools <server>, got %v", err)
	}
}

func TestCmdListHelp(t *testing.T) {
	var err error
	stderr := captureStderr(t, func() { err = cmdList([]string{"--help"}) })
	if err != nil || !strings.Contains(stderr, "Usage: mcp list") {
		t.Errorf("--help: err=%v stderr=%q", err, stderr)
	}
}
