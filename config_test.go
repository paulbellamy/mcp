package main

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// loadPendingAuth loads pending OAuth state for a server (test-only helper).
func loadPendingAuth(name string) (*PendingAuth, error) {
	var pending PendingAuth
	found, err := readJSON(pendingAuthPath(name), &pending)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &pending, nil
}

func TestServerCRUD(t *testing.T) {
	setupTestConfigDir(t)

	// Initially empty
	servers, err := loadServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("expected 0 servers, got %d", len(servers))
	}

	// Add a server
	s1 := ServerConfig{Name: "test-server", Transport: "streamable-http", URL: "http://localhost:8080"}
	if err := addServerConfig(s1); err != nil {
		t.Fatal(err)
	}

	servers, _ = loadServers()
	if len(servers) != 1 || servers[0].Name != "test-server" {
		t.Fatalf("expected 1 server 'test-server', got %+v", servers)
	}

	// Get server
	got, err := getServerConfig("test-server")
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != "http://localhost:8080" {
		t.Errorf("expected URL 'http://localhost:8080', got %q", got.URL)
	}

	// Get nonexistent
	_, err = getServerConfig("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent server")
	}

	// Replace on add (same name)
	s1updated := ServerConfig{Name: "test-server", Transport: "streamable-http", URL: "http://localhost:9090"}
	if err := addServerConfig(s1updated); err != nil {
		t.Fatal(err)
	}
	servers, _ = loadServers()
	if len(servers) != 1 || servers[0].URL != "http://localhost:9090" {
		t.Fatalf("expected updated URL, got %+v", servers)
	}

	// Add another server
	s2 := ServerConfig{Name: "second", Transport: "stdio", Command: "echo"}
	if err := addServerConfig(s2); err != nil {
		t.Fatal(err)
	}
	servers, _ = loadServers()
	if len(servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(servers))
	}

	// Remove first
	if err := removeServerConfig("test-server"); err != nil {
		t.Fatal(err)
	}
	servers, _ = loadServers()
	if len(servers) != 1 || servers[0].Name != "second" {
		t.Fatalf("expected 1 server 'second', got %+v", servers)
	}

	// Remove nonexistent
	if err := removeServerConfig("nonexistent"); err == nil {
		t.Error("expected error removing nonexistent server")
	}
}

func TestAuthTokenSaveLoad(t *testing.T) {
	setupTestConfigDir(t)

	// Load missing
	tokens, err := loadAuth("test")
	if err != nil {
		t.Fatal(err)
	}
	if tokens != nil {
		t.Fatal("expected nil tokens for missing server")
	}

	// Save and load
	auth := &AuthTokens{
		AccessToken:  "at-123",
		RefreshToken: "rt-456",
		ExpiresAt:    time.Now().Unix() + 3600,
		ClientID:     "cid",
	}
	if err := saveAuth("test", auth); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadAuth("test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil tokens")
	}
	if loaded.AccessToken != "at-123" {
		t.Errorf("expected access token 'at-123', got %q", loaded.AccessToken)
	}
	if loaded.RefreshToken != "rt-456" {
		t.Errorf("expected refresh token 'rt-456', got %q", loaded.RefreshToken)
	}
}

func TestPendingAuthSaveLoad(t *testing.T) {
	setupTestConfigDir(t)

	// Load missing
	pending, err := loadPendingAuth("test")
	if err != nil {
		t.Fatal(err)
	}
	if pending != nil {
		t.Fatal("expected nil pending for missing server")
	}

	// Save and load
	pa := &PendingAuth{
		Nonce:        "nonce-abc",
		CodeVerifier: "verifier-xyz",
		ClientID:     "client-123",
		TokenURL:     "https://auth.example.com/token",
		ServerName:   "test",
		CreatedAt:    time.Now().Unix(),
	}
	if err := savePendingAuth("test", pa); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadPendingAuth("test")
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil {
		t.Fatal("expected non-nil pending auth")
	}
	if loaded.Nonce != "nonce-abc" {
		t.Errorf("expected nonce 'nonce-abc', got %q", loaded.Nonce)
	}
	if loaded.CodeVerifier != "verifier-xyz" {
		t.Errorf("expected code verifier 'verifier-xyz', got %q", loaded.CodeVerifier)
	}
}

func TestFindPendingAuthByNonce(t *testing.T) {
	setupTestConfigDir(t)

	// No pending auths
	found, _, err := findPendingAuthByNonce("missing")
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Fatal("expected nil for missing nonce")
	}

	// Save two pending auths
	pa1 := &PendingAuth{Nonce: "nonce-1", ServerName: "server1", CreatedAt: time.Now().Unix()}
	pa2 := &PendingAuth{Nonce: "nonce-2", ServerName: "server2", CreatedAt: time.Now().Unix()}
	if err := savePendingAuth("server1", pa1); err != nil {
		t.Fatal(err)
	}
	if err := savePendingAuth("server2", pa2); err != nil {
		t.Fatal(err)
	}

	// Find by nonce
	found, path, err := findPendingAuthByNonce("nonce-2")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected to find pending auth")
	}
	if found.ServerName != "server2" {
		t.Errorf("expected server 'server2', got %q", found.ServerName)
	}
	if path == "" {
		t.Error("expected non-empty path")
	}

	// Not found
	found, _, _ = findPendingAuthByNonce("nonce-missing")
	if found != nil {
		t.Error("expected nil for missing nonce")
	}
}

func TestAtomicWrite_FilePermissions(t *testing.T) {
	setupTestConfigDir(t)

	// Set a permissive umask so that os.Chmod is the only thing restricting perms
	oldUmask := syscall.Umask(0000)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	if err := saveAuth("perm-test", &AuthTokens{AccessToken: "secret"}); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(authPath("perm-test"))
	if err != nil {
		t.Fatal(err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected permissions 0600, got %04o", perm)
	}
}

func TestConcurrentServerAdd(t *testing.T) {
	setupTestConfigDir(t)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s := ServerConfig{
				Name:      fmt.Sprintf("server-%d", n),
				Transport: "streamable-http",
				URL:       fmt.Sprintf("http://localhost:%d", 8000+n),
			}
			if err := addServerConfig(s); err != nil {
				t.Errorf("addServerConfig(%d): %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Concurrent writes are last-writer-wins; verify the file is valid JSON
	// and contains at least one server (no corruption).
	servers, err := loadServers()
	if err != nil {
		t.Fatalf("loadServers returned invalid data after concurrent writes: %v", err)
	}
	if len(servers) == 0 {
		t.Error("expected at least one server after concurrent adds")
	}
}

func TestValidateServerName(t *testing.T) {
	valid := []string{"myserver", "my-server", "my.server", "my_server", "Server1", "a", "abc123"}
	for _, name := range valid {
		if err := validateServerName(name); err != nil {
			t.Errorf("expected %q to be valid, got: %v", name, err)
		}
	}

	invalid := []string{"", "-server", ".server", "_server", "my server", "my/server", "../etc", "a@b", "server;rm"}
	for _, name := range invalid {
		if err := validateServerName(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}

func TestFindPendingAuthByNonce_Expired(t *testing.T) {
	setupTestConfigDir(t)

	pa := &PendingAuth{
		Nonce:      "expired-nonce",
		ServerName: "expserver",
		CreatedAt:  time.Now().Add(-15 * time.Minute).Unix(),
	}
	if err := savePendingAuth("expserver", pa); err != nil {
		t.Fatal(err)
	}

	found, _, err := findPendingAuthByNonce("expired-nonce")
	if err != nil {
		t.Fatal(err)
	}
	if found != nil {
		t.Error("expected nil for expired pending auth")
	}

	// Verify file was cleaned up
	loaded, err := loadPendingAuth("expserver")
	if err != nil {
		t.Fatal(err)
	}
	if loaded != nil {
		t.Error("expected expired pending auth file to be removed")
	}
}

func TestToolCacheSaveLoadTTL(t *testing.T) {
	setupTestConfigDir(t)

	// Load missing
	tools, err := loadCachedTools("test")
	if err != nil {
		t.Fatal(err)
	}
	if tools != nil {
		t.Fatal("expected nil for missing cache")
	}

	// Save and load
	toolList := []toolOutput{
		{Server: "test", Name: "tool1", Description: "desc1"},
		{Server: "test", Name: "tool2", Description: "desc2"},
	}
	if err := saveCachedTools("test", toolList); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadCachedTools("test")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 cached tools, got %d", len(loaded))
	}
	if loaded[0].Name != "tool1" || loaded[1].Name != "tool2" {
		t.Errorf("unexpected tools: %+v", loaded)
	}

	// Expired cache — write directly with old timestamp
	cache := ToolCache{
		Tools:    toolList,
		CachedAt: time.Now().Add(-20 * time.Minute).Unix(),
	}
	writeJSON(cachePath("expired"), cache)

	expired, err := loadCachedTools("expired")
	if err != nil {
		t.Fatal(err)
	}
	if expired != nil {
		t.Error("expected nil for expired cache")
	}
}

func TestLoadCachedToolsStale(t *testing.T) {
	setupTestConfigDir(t)

	// Missing cache returns nil.
	tools, err := loadCachedToolsStale("missing")
	if err != nil {
		t.Fatal(err)
	}
	if tools != nil {
		t.Fatal("expected nil for missing cache")
	}

	// Write an expired cache (old timestamp).
	toolList := []toolOutput{
		{Server: "test", Name: "tool1", Description: "desc1"},
	}
	cache := ToolCache{
		Tools:    toolList,
		CachedAt: time.Now().Add(-20 * time.Minute).Unix(),
	}
	writeJSON(cachePath("stale"), cache)

	// loadCachedTools returns nil (expired).
	fresh, err := loadCachedTools("stale")
	if err != nil {
		t.Fatal(err)
	}
	if fresh != nil {
		t.Error("expected nil from loadCachedTools for expired cache")
	}

	// loadCachedToolsStale returns the tools despite TTL.
	stale, err := loadCachedToolsStale("stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].Name != "tool1" {
		t.Errorf("expected stale cache to return tool1, got %+v", stale)
	}
}

func TestValidateToolName(t *testing.T) {
	valid := []string{"echo", "my-tool", "mcp__server__tool", "ns/tool", "a.b.c", "tool:action"}
	for _, name := range valid {
		if err := validateToolName(name); err != nil {
			t.Errorf("expected %q to be valid, got: %v", name, err)
		}
	}

	invalid := []string{"", "-tool", ".tool", "/tool", ":tool", "tool name", "tool;rm", "tool@host"}
	for _, name := range invalid {
		if err := validateToolName(name); err == nil {
			t.Errorf("expected %q to be invalid", name)
		}
	}
}
