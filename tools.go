package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// mcpConnect creates a transport, sends initialize + initialized, returns the transport.
func mcpConnect(server *ServerConfig, authToken string) (Transport, error) {
	var transport Transport
	var err error

	switch server.Transport {
	case "stdio":
		// Try daemon first — server already initialized
		if t, err := NewDaemonTransport(server.Name); err == nil {
			return t, nil
		}
		transport, err = NewStdioTransport(server.Command, server.Args)
	case "streamable-http":
		transport = NewHTTPTransport(server.URL, authToken)
	default:
		return nil, fmt.Errorf("unsupported transport %q", server.Transport)
	}

	if err != nil {
		return nil, fmt.Errorf("create transport: %w", err)
	}

	// MCP initialize handshake
	initResp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: "2025-03-26",
			Capabilities:    clientCapabilities{},
			ClientInfo: clientInfo{
				Name:    "mcp-cli",
				Version: Version,
			},
		},
	})
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if initResp.Error != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	// Send initialized notification
	if err := transport.Notify(jsonrpcNotification{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	}); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("send initialized notification: %w", err)
	}

	return transport, nil
}

// discoverTools connects to a server, lists tools, and returns them.
func discoverTools(server *ServerConfig, authToken string) ([]toolOutput, error) {
	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transport.Close() }()

	return listAllTools(transport, server.Name)
}

// listAllTools fetches all tools from a connected transport, handling pagination.
func listAllTools(transport Transport, serverName string) ([]toolOutput, error) {
	var allTools []toolOutput
	var cursor string
	const maxPages = 100

	for page := 0; page < maxPages; page++ {
		var params any
		if cursor != "" {
			params = toolsListParams{Cursor: cursor}
		}

		resp, err := transport.Send(jsonrpcRequest{
			JSONRPC: jsonrpcVersion,
			ID:      nextID(),
			Method:  "tools/list",
			Params:  params,
		})

		if err != nil {
			return nil, fmt.Errorf("list tools: %w", err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("list tools: %s", resp.Error.Message)
		}

		var result toolsListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("unmarshal tools: %w", err)
		}

		for _, t := range result.Tools {
			allTools = append(allTools, toolOutput{
				Server:      serverName,
				Name:        t.Name,
				Description: t.Description,
				InputSchema: t.InputSchema,
			})
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	if cursor != "" {
		logStderr("warning: tools list truncated after %d pages", maxPages)
	}

	return allTools, nil
}

// mcpPing sends a ping request to verify server liveness.
func mcpPing(transport Transport) error {
	resp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "ping",
	})
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	if resp.Error != nil {
		return fmt.Errorf("ping: %s", resp.Error.Message)
	}
	return nil
}

// getToolsForServer returns tools for a server, using cache if fresh.
func getToolsForServer(server *ServerConfig, refresh bool) ([]toolOutput, error) {
	// Try cache first (unless refresh requested)
	if !refresh {
		cached, err := loadCachedTools(server.Name)
		if err != nil {
			logStderr("warning: cache read failed: %v", err)
		}
		if cached != nil {
			return cached, nil
		}
	}

	// Get auth token if available
	authToken, err := getAuthToken(server.Name)
	if err != nil {
		logStderr("warning: auth token load failed: %v", err)
	}

	// Discover fresh
	tools, err := discoverTools(server, authToken)
	if err != nil {
		return nil, err
	}

	// Cache the results
	if cacheErr := saveCachedTools(server.Name, tools); cacheErr != nil {
		logStderr("warning: cache write failed: %v", cacheErr)
	}

	return tools, nil
}

// getAuthToken loads and optionally refreshes the auth token for a server.
// Uses file locking to prevent concurrent refresh races across processes.
func getAuthToken(name string) (string, error) {
	tokens, err := loadAuth(name)
	if err != nil {
		return "", err
	}
	if tokens == nil {
		return "", nil
	}

	// Check if token needs refresh
	if tokens.ExpiresAt > 0 && tokens.ExpiresAt-30 < time.Now().Unix() {
		if tokens.RefreshToken != "" && tokens.TokenURL != "" {
			refreshed, err := refreshTokenWithLock(name, tokens)
			if err != nil {
				logStderr("warning: token refresh failed: %v", err)
				// Fall through and try the expired token anyway
			} else {
				return refreshed.AccessToken, nil
			}
		}
	}

	return tokens.AccessToken, nil
}

// refreshTokenWithLock acquires an exclusive file lock before refreshing,
// then re-checks whether another process already refreshed the token.
func refreshTokenWithLock(name string, tokens *AuthTokens) (*AuthTokens, error) {
	unlock, err := lockFile(authPath(name))
	if err != nil {
		return nil, fmt.Errorf("acquire lock: %w", err)
	}
	defer unlock()

	// Re-read after acquiring lock — another process may have refreshed already
	fresh, err := loadAuth(name)
	if err != nil {
		return nil, err
	}
	if fresh != nil && fresh.ExpiresAt > 0 && fresh.ExpiresAt-30 >= time.Now().Unix() {
		return fresh, nil
	}

	logStderr("token expired, refreshing...")
	refreshed, err := refreshOAuthToken(tokens)
	if err != nil {
		return nil, err
	}

	if err := saveAuth(name, refreshed); err != nil {
		logStderr("warning: failed to save refreshed token: %v", err)
	}

	return refreshed, nil
}

// cmdTools handles the `mcp tools` command.
func cmdTools(args []string) error {
	var serverFilter, query string
	var refresh, jsonOutput, full bool

	// Parse args: mcp tools [server] [--query <search>] [--refresh] [--json] [--full]
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--query", "-q":
			if i+1 >= len(args) {
				return fmt.Errorf("--query requires a value")
			}
			i++
			query = args[i]
		case "--refresh":
			refresh = true
		case "--json":
			jsonOutput = true
		case "--full":
			full = true
		default:
			if strings.HasPrefix(args[i], "--") || strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s", args[i])
			}
			if serverFilter == "" {
				serverFilter = args[i]
			} else {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	// Ad-hoc URL: connect directly, skip config lookup.
	if serverFilter != "" && isURL(serverFilter) {
		server, authToken, err := resolveServer(serverFilter)
		if err != nil {
			return err
		}
		tools, err := discoverTools(server, authToken)
		if err != nil {
			return err
		}
		return outputToolsList(tools, query, jsonOutput, full)
	}

	if serverFilter != "" {
		if err := validateServerName(serverFilter); err != nil {
			return err
		}
	}

	servers, err := loadServers()
	if err != nil {
		return err
	}

	if len(servers) == 0 && serverFilter == "" {
		return outputJSON([]toolOutput{})
	}

	var (
		allTools []toolOutput
		mu       sync.Mutex
		wg       sync.WaitGroup
	)
	sem := make(chan struct{}, 5)
	for _, s := range servers {
		if serverFilter != "" && s.Name != serverFilter {
			continue
		}
		if !s.IsEnabled() {
			continue
		}
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tools, err := getToolsForServer(&s, refresh)
			if err != nil {
				logStderr("warning: failed to get tools from %q: %v", s.Name, err)
				return
			}
			mu.Lock()
			allTools = append(allTools, tools...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	return outputToolsList(allTools, query, jsonOutput, full)
}

// outputToolsList sorts, filters, and outputs a list of tools.
// When full is false, the inputSchema field is stripped from JSON output —
// callers should fetch full schemas on demand via `mcp schema <server> <tool>`.
func outputToolsList(tools []toolOutput, query string, jsonOutput, full bool) error {
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Server != tools[j].Server {
			return tools[i].Server < tools[j].Server
		}
		return tools[i].Name < tools[j].Name
	})

	if query != "" {
		q := strings.ToLower(query)
		var filtered []toolOutput
		for _, t := range tools {
			if strings.Contains(strings.ToLower(t.Name), q) ||
				strings.Contains(strings.ToLower(t.Description), q) {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	if tools == nil {
		tools = []toolOutput{}
	}

	if jsonOutput || !isStdoutTTY() {
		if !full {
			compact := make([]toolOutput, len(tools))
			for i, t := range tools {
				compact[i] = toolOutput{
					Server:      t.Server,
					Name:        t.Name,
					Description: t.Description,
				}
			}
			return outputJSON(compact)
		}
		return outputJSON(tools)
	}
	return printToolsHuman(tools)
}

// isStdoutTTY returns true if stdout is a terminal.
func isStdoutTTY() bool {
	stat, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return (stat.Mode() & os.ModeCharDevice) != 0
}

// printToolsHuman formats tools as CLI-style help text grouped by server.
func printToolsHuman(tools []toolOutput) error {
	if len(tools) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "No tools found.")
		return nil
	}

	// Find max tool name length for alignment.
	maxNameLen := 0
	for _, t := range tools {
		if len(t.Name) > maxNameLen {
			maxNameLen = len(t.Name)
		}
	}

	// Print tools grouped by server (already sorted by server, then name).
	lastServer := ""
	serverCount := 0
	for _, t := range tools {
		if t.Server != lastServer {
			if lastServer != "" {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			// Count tools for this server.
			serverCount = 0
			for _, u := range tools {
				if u.Server == t.Server {
					serverCount++
				}
			}
			noun := "tools"
			if serverCount == 1 {
				noun = "tool"
			}
			_, _ = fmt.Fprintf(os.Stdout, "%s (%d %s)\n", t.Server, serverCount, noun)
			lastServer = t.Server
		}
		if t.Description == "" {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", t.Name)
		} else {
			// Indent continuation lines to align with the description column.
			pad := strings.Repeat(" ", 2+maxNameLen+2)
			lines := strings.Split(t.Description, "\n")
			_, _ = fmt.Fprintf(os.Stdout, "  %-*s  %s\n", maxNameLen, t.Name, lines[0])
			for _, line := range lines[1:] {
				_, _ = fmt.Fprintf(os.Stdout, "%s%s\n", pad, line)
			}
		}
	}

	return nil
}
