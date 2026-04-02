//go:build integration

package main

import (
	"os/exec"
	"testing"
)

func TestIntegration_FullLifecycle(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not found in PATH")
	}

	server := &ServerConfig{
		Name:      "test-everything",
		Transport: "stdio",
		Command:   "npx",
		Args:      []string{"@modelcontextprotocol/server-everything"},
	}

	transport, err := mcpConnect(server, "")
	if err != nil {
		t.Skipf("could not connect to test server: %v", err)
	}
	defer func() { _ = transport.Close() }()

	// List tools
	tools, err := listAllTools(transport, "test")
	if err != nil {
		t.Fatal("list tools:", err)
	}
	if len(tools) == 0 {
		t.Error("expected at least one tool")
	}
	t.Logf("discovered %d tools", len(tools))

	// Ping
	if err := mcpPing(transport); err != nil {
		t.Error("ping:", err)
	}
}
