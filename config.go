package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const cacheTTL = 10 * time.Minute
const pendingAuthTTL = 10 * time.Minute

var validServerName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)
var validToolName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*$`)

// isURL returns true if s looks like a URL (contains "://").
// Valid server names never contain "://", so this is unambiguous.
func isURL(s string) bool {
	return strings.Contains(s, "://")
}

func validateServerName(name string) error {
	if !validServerName.MatchString(name) {
		return fmt.Errorf("invalid server name %q: must start with alphanumeric and contain only alphanumerics, dots, hyphens, or underscores", name)
	}
	return nil
}

func validateToolName(name string) error {
	if !validToolName.MatchString(name) {
		return fmt.Errorf("invalid tool name %q: must start with alphanumeric and contain only alphanumerics, dots, hyphens, underscores, colons, or slashes", name)
	}
	return nil
}

// ServerConfig represents a configured MCP server.
type ServerConfig struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"` // "stdio" or "streamable-http"
	URL       string   `json:"url,omitempty"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	Enabled   *bool    `json:"enabled,omitempty"` // nil or true = enabled
}

// IsEnabled returns whether the server is enabled (default true).
func (s *ServerConfig) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// AuthTokens represents stored OAuth tokens for a server.
type AuthTokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	TokenURL     string `json:"token_endpoint,omitempty"`
	Resource     string `json:"resource,omitempty"`
}

// PendingAuth represents in-flight OAuth state saved to disk.
type PendingAuth struct {
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"code_verifier"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
	TokenURL     string `json:"token_endpoint"`
	Resource     string `json:"resource"`
	RedirectURI  string `json:"redirect_uri"`
	ServerName   string `json:"server_name"`
	CreatedAt    int64  `json:"created_at"`
}

// ToolCache represents cached tool definitions for a server.
type ToolCache struct {
	Tools    []toolOutput `json:"tools"`
	CachedAt int64        `json:"cached_at"`
}

// testConfigDir overrides the config directory for tests.
var testConfigDir string

// configDir returns the base config directory (~/.config/mcp/).
func configDir() string {
	if testConfigDir != "" {
		return testConfigDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "mcp")
}

// ensureConfigDirs creates all necessary config subdirectories.
func ensureConfigDirs() error {
	dirs := []string{
		configDir(),
		filepath.Join(configDir(), "auth"),
		filepath.Join(configDir(), "cache"),
		filepath.Join(configDir(), "daemon"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return fmt.Errorf("create config dir %s: %w", d, err)
		}
	}
	return nil
}

// cleanupExpiredPendingAuth removes stale *.pending.json files older than pendingAuthTTL.
func cleanupExpiredPendingAuth() {
	dir := filepath.Join(configDir(), "auth")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".pending.json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) > pendingAuthTTL {
			_ = os.Remove(path)
		}
	}
}

// atomicWrite writes data to a file atomically via a temp file + rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Chmod(tmpPath, 0600); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

// readJSON reads and unmarshals a JSON file. Returns false if file doesn't exist.
func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal(data, v)
}

// writeJSON marshals and atomically writes a JSON file.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'))
}

// serversPath returns the path to servers.json.
func serversPath() string {
	return filepath.Join(configDir(), "servers.json")
}

// loadServers loads all configured servers.
func loadServers() ([]ServerConfig, error) {
	var servers []ServerConfig
	found, err := readJSON(serversPath(), &servers)
	if err != nil {
		return nil, err
	}
	if !found {
		return []ServerConfig{}, nil
	}
	return servers, nil
}

// saveServers saves the server list.
func saveServers(servers []ServerConfig) error {
	return writeJSON(serversPath(), servers)
}

// getServerConfig finds a server by name.
func getServerConfig(name string) (*ServerConfig, error) {
	servers, err := loadServers()
	if err != nil {
		return nil, err
	}
	for _, s := range servers {
		if s.Name == name {
			return &s, nil
		}
	}
	return nil, fmt.Errorf("server %q not found", name)
}

// resolveServer resolves a server name or ad-hoc URL to a config and auth token.
// For ad-hoc URLs, uses streamable-http transport and MCP_AUTH_TOKEN env var.
// For named servers, looks up config and loads/refreshes the auth token.
func resolveServer(nameOrURL string) (*ServerConfig, string, error) {
	if isURL(nameOrURL) {
		if err := validateEndpointURL(nameOrURL, "MCP server"); err != nil {
			return nil, "", err
		}
		return &ServerConfig{Transport: "streamable-http", URL: nameOrURL}, os.Getenv("MCP_AUTH_TOKEN"), nil
	}
	if err := validateServerName(nameOrURL); err != nil {
		return nil, "", err
	}
	server, err := getServerConfig(nameOrURL)
	if err != nil {
		return nil, "", err
	}
	if !server.IsEnabled() {
		return nil, "", fmt.Errorf("server %q is disabled", nameOrURL)
	}
	authToken, err := getAuthToken(nameOrURL)
	if err != nil {
		logStderr("warning: auth token load failed: %v", err)
	}
	return server, authToken, nil
}

// addServerConfig adds or updates a server config in servers.json.
func addServerConfig(server ServerConfig) error {
	unlock, err := lockFile(serversPath())
	if err != nil {
		return fmt.Errorf("lock servers.json: %w", err)
	}
	defer unlock()

	servers, err := loadServers()
	if err != nil {
		return err
	}

	// Replace if exists
	found := false
	for i, s := range servers {
		if s.Name == server.Name {
			servers[i] = server
			found = true
			break
		}
	}
	if !found {
		servers = append(servers, server)
	}

	return saveServers(servers)
}

// removeServerConfig removes a server and its associated auth/cache files.
func removeServerConfig(name string) error {
	unlock, err := lockFile(serversPath())
	if err != nil {
		return fmt.Errorf("lock servers.json: %w", err)
	}
	defer unlock()

	servers, err := loadServers()
	if err != nil {
		return err
	}

	filtered := make([]ServerConfig, 0, len(servers))
	for _, s := range servers {
		if s.Name != name {
			filtered = append(filtered, s)
		}
	}

	if len(filtered) == len(servers) {
		return fmt.Errorf("server %q not found", name)
	}

	if err := saveServers(filtered); err != nil {
		return err
	}

	// Clean up auth and cache files (ignore errors)
	os.Remove(authPath(name))
	os.Remove(pendingAuthPath(name))
	os.Remove(cachePath(name))

	return nil
}

// authPath returns the path for a server's auth tokens.
func authPath(name string) string {
	return filepath.Join(configDir(), "auth", name+".json")
}

// pendingAuthPath returns the path for a server's pending OAuth state.
func pendingAuthPath(name string) string {
	return filepath.Join(configDir(), "auth", name+".pending.json")
}

// cachePath returns the path for a server's tool cache.
func cachePath(name string) string {
	return filepath.Join(configDir(), "cache", name+".json")
}

// daemonSocketDir returns the directory for daemon Unix sockets.
func daemonSocketDir() string {
	return filepath.Join(configDir(), "daemon")
}

// daemonSocketPath returns the Unix socket path for a daemon-managed server.
func daemonSocketPath(name string) string {
	return filepath.Join(daemonSocketDir(), name+".sock")
}

// daemonPIDPath returns the path for the daemon PID file.
func daemonPIDPath() string {
	return filepath.Join(configDir(), "daemon.pid")
}

// loadAuth loads stored auth tokens for a server.
func loadAuth(name string) (*AuthTokens, error) {
	var tokens AuthTokens
	found, err := readJSON(authPath(name), &tokens)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return &tokens, nil
}

// saveAuth saves auth tokens for a server.
func saveAuth(name string, tokens *AuthTokens) error {
	return writeJSON(authPath(name), tokens)
}

// findPendingAuthByNonce finds a pending auth by its nonce across all servers.
func findPendingAuthByNonce(nonce string) (*PendingAuth, string, error) {
	dir := filepath.Join(configDir(), "auth")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}

	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			name := entry.Name()
			if strings.HasSuffix(name, ".pending.json") {
				var pending PendingAuth
				path := filepath.Join(dir, name)
				if found, err := readJSON(path, &pending); err == nil && found && pending.Nonce == nonce {
					if time.Since(time.Unix(pending.CreatedAt, 0)) > pendingAuthTTL {
						os.Remove(path)
						continue
					}
					return &pending, path, nil
				}
			}
		}
	}

	return nil, "", nil
}

// savePendingAuth saves pending OAuth state.
func savePendingAuth(name string, pending *PendingAuth) error {
	return writeJSON(pendingAuthPath(name), pending)
}

// loadCachedTools loads cached tools for a server. Returns nil if cache is stale or missing.
func loadCachedTools(name string) ([]toolOutput, error) {
	var cache ToolCache
	found, err := readJSON(cachePath(name), &cache)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	// Check TTL
	if time.Since(time.Unix(cache.CachedAt, 0)) > cacheTTL {
		return nil, nil
	}

	return cache.Tools, nil
}

// loadCachedToolsStale loads cached tools ignoring TTL. Returns nil if no cache file exists.
func loadCachedToolsStale(name string) ([]toolOutput, error) {
	var cache ToolCache
	found, err := readJSON(cachePath(name), &cache)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return cache.Tools, nil
}

// saveCachedTools saves tool definitions to cache.
func saveCachedTools(name string, tools []toolOutput) error {
	return writeJSON(cachePath(name), ToolCache{
		Tools:    tools,
		CachedAt: time.Now().Unix(),
	})
}
