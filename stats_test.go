package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokens_Empty(t *testing.T) {
	if estimateTokens(nil) != 1 {
		// nil marshals to "null" which is 4 chars / 4 = 1
		t.Errorf("estimateTokens(nil) should be 1, got %d", estimateTokens(nil))
	}
}

func TestEstimateTokens_String(t *testing.T) {
	// "abcdefghij" is 12 bytes when JSON-marshaled (with quotes), so ~3 tokens.
	got := estimateTokens("abcdefghij")
	if got != 3 {
		t.Errorf("expected 3 tokens, got %d", got)
	}
}

func TestCmdStats_NoServers(t *testing.T) {
	setupTestConfigDir(t)

	stderr := captureStderr(t, func() {
		err := cmdStats(nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(stderr, "No servers configured") {
		t.Errorf("expected 'No servers configured' message, got %q", stderr)
	}
}

func TestCmdStats_UnknownFlag(t *testing.T) {
	err := cmdStats([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestCmdStats_Summary(t *testing.T) {
	setupTestConfigDir(t)

	if err := addServerConfig(ServerConfig{
		Name:      "alpha",
		Transport: "streamable-http",
		URL:       "http://example.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	// Pre-populate cache so getToolsForServer doesn't try to connect.
	tools := []toolOutput{
		{
			Server:      "alpha",
			Name:        "search",
			Description: "Search for things",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"What to look for"}}}`),
		},
		{
			Server:      "alpha",
			Name:        "get",
			Description: "Get an item",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
		},
	}
	if err := saveCachedTools("alpha", tools); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		err := cmdStats(nil)
		if err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(stdout, "alpha") {
		t.Errorf("expected server name in output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Total") {
		t.Errorf("expected Total row:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Schema Tokens") || !strings.Contains(stdout, "Summary Tokens") {
		t.Errorf("expected column headers:\n%s", stdout)
	}
}

func TestCmdStats_FullIncludesPerToolBreakdown(t *testing.T) {
	setupTestConfigDir(t)

	if err := addServerConfig(ServerConfig{
		Name:      "beta",
		Transport: "streamable-http",
		URL:       "http://example.invalid",
	}); err != nil {
		t.Fatal(err)
	}

	tools := []toolOutput{
		{
			Server:      "beta",
			Name:        "echo",
			Description: "Echoes input",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		},
	}
	if err := saveCachedTools("beta", tools); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		if err := cmdStats([]string{"--full"}); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(stdout, "echo") {
		t.Errorf("expected per-tool 'echo' line in --full output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "Echoes input") {
		t.Errorf("expected description in --full output:\n%s", stdout)
	}
}

func TestCmdStats_SkipsDisabledServers(t *testing.T) {
	setupTestConfigDir(t)

	disabled := false
	if err := addServerConfig(ServerConfig{
		Name:      "off",
		Transport: "streamable-http",
		URL:       "http://example.invalid",
		Enabled:   &disabled,
	}); err != nil {
		t.Fatal(err)
	}
	if err := saveCachedTools("off", []toolOutput{
		{Server: "off", Name: "ghost", InputSchema: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatal(err)
	}

	stdout := captureStdout(t, func() {
		if err := cmdStats(nil); err != nil {
			t.Fatal(err)
		}
	})

	if strings.Contains(stdout, "off") || strings.Contains(stdout, "ghost") {
		t.Errorf("disabled server should not appear:\n%s", stdout)
	}
}
