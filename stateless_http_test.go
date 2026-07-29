package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// modernServerState records what a modern mock server observed.
type modernServerState struct {
	mu          sync.Mutex
	methods     []string
	sawDelete   bool
	sessionEcho bool // client echoed a session ID we minted
}

func (s *modernServerState) record(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
}

func (s *modernServerState) sawMethod(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.methods {
		if m == method {
			return true
		}
	}
	return false
}

// newModernMCPServer speaks only the stateless 2026-07-28 revision. It
// validates the request metadata headers and per-request _meta on every
// POST, and mints a session ID that a conforming client must ignore.
func newModernMCPServer(t *testing.T, tools []mcpTool, ttlMs int64) (*httptest.Server, *modernServerState) {
	t.Helper()
	state := &modernServerState{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			state.mu.Lock()
			state.sawDelete = true
			state.mu.Unlock()
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Mcp-Session-Id") != "" {
			state.mu.Lock()
			state.sessionEcho = true
			state.mu.Unlock()
		}

		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("bad request body: %v", err)
		}
		state.record(req.Method)

		// Header validation per the streamable HTTP transport spec.
		if got := r.Header.Get("MCP-Protocol-Version"); got != protocolVersionModern {
			t.Errorf("%s: MCP-Protocol-Version = %q, want %s", req.Method, got, protocolVersionModern)
		}
		if got := r.Header.Get("Mcp-Method"); got != req.Method {
			t.Errorf("Mcp-Method = %q, body method %q", got, req.Method)
		}

		var params struct {
			Name         string         `json:"name"`
			URI          string         `json:"uri"`
			RequestState string         `json:"requestState"`
			Arguments    map[string]any `json:"arguments"`
			Meta         map[string]any `json:"_meta"`
		}
		data, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(data, &params)
		if params.Meta[metaProtocolVersion] != protocolVersionModern {
			t.Errorf("%s: _meta protocolVersion = %v", req.Method, params.Meta[metaProtocolVersion])
		}
		switch req.Method {
		case "tools/call":
			if got := r.Header.Get("Mcp-Name"); got != params.Name {
				t.Errorf("tools/call Mcp-Name = %q, want %q", got, params.Name)
			}
		case "resources/read":
			if got := r.Header.Get("Mcp-Name"); got != params.URI {
				t.Errorf("resources/read Mcp-Name = %q, want %q", got, params.URI)
			}
		}

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "server/discover":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{protocolVersionModern},
				"capabilities":      map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			})
		case "tools/list":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"tools":      tools,
				"ttlMs":      ttlMs,
				"cacheScope": "private",
			})
		case "tools/call":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"content":    []contentBlock{{Type: "text", Text: "called:" + params.Name}},
			})
		case "resources/read":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"contents":   []resourceContents{{URI: params.URI, Text: "resource-data"}},
			})
		case "initialize":
			t.Error("modern-only server received initialize")
			w.WriteHeader(http.StatusBadRequest)
			return
		default:
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}

		// A conforming modern client must ignore a minted session ID.
		w.Header().Set("Mcp-Session-Id", "modern-servers-do-not-do-this")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	return srv, state
}

func TestModernHTTP_EndToEnd(t *testing.T) {
	srv, state := newModernMCPServer(t, []mcpTool{{Name: "echo", Description: "echoes"}}, 60000)
	defer srv.Close()

	cfg := &ServerConfig{Name: "modern", Transport: "streamable-http", URL: srv.URL}
	transport, err := mcpConnect(cfg, "")
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := transport.(*protocolSession); !ok {
		t.Fatalf("expected modern session, got %T", transport)
	}

	tools, ttlMs, err := listAllTools(transport, "modern")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Errorf("unexpected tools: %+v", tools)
	}
	if ttlMs != 60000 {
		t.Errorf("ttlMs = %d, want 60000", ttlMs)
	}

	out, err := executeToolCall(transport, "echo", map[string]any{"msg": "hi"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "called:echo" || out.IsError {
		t.Errorf("unexpected call output: %+v", out)
	}

	read, err := readResource(transport, "file:///notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Contents) != 1 || read.Contents[0].Text != "resource-data" {
		t.Errorf("unexpected read output: %+v", read)
	}

	if err := mcpPing(transport); err != nil {
		t.Errorf("ping: %v", err)
	}

	_ = transport.Close()

	if state.sawMethod("initialize") {
		t.Error("client sent initialize to a modern server")
	}
	if state.sessionEcho {
		t.Error("client echoed a session ID in stateless mode")
	}
	if state.sawDelete {
		t.Error("client sent legacy session DELETE in stateless mode")
	}
}

// newLegacyMCPServer speaks only the pre-2026 revision: server/discover is
// rejected with the given HTTP status (and optional body), initialize mints
// a session, and subsequent requests require it.
func newLegacyMCPServer(t *testing.T, probeStatus int, probeBody string) (*httptest.Server, *modernServerState) {
	t.Helper()
	state := &modernServerState{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK) // notification
			return
		}
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)
		state.record(req.Method)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
			})
			w.Header().Set("Mcp-Session-Id", "sess-42")
		case "tools/call":
			// Legacy framing: session required, no modern metadata headers.
			if got := r.Header.Get("Mcp-Session-Id"); got != "sess-42" {
				t.Errorf("tools/call Mcp-Session-Id = %q, want sess-42", got)
			}
			if r.Header.Get("MCP-Protocol-Version") != "" || r.Header.Get("Mcp-Method") != "" {
				t.Error("legacy request carries modern metadata headers")
			}
			var params toolCallParams
			data, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(data, &params)
			resp.Result, _ = json.Marshal(toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "legacy:" + params.Name}},
			})
		default:
			// server/discover (and anything else unknown) is rejected at the
			// HTTP layer, the way pre-2026 session-enforcing servers do.
			w.WriteHeader(probeStatus)
			_, _ = w.Write([]byte(probeBody))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	return srv, state
}

func TestLegacyHTTP_FallbackOnStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"400 no session", http.StatusBadRequest, "Bad Request: No valid session ID provided"},
		{"404 not found", http.StatusNotFound, "not found"},
		{"405 method not allowed", http.StatusMethodNotAllowed, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, state := newLegacyMCPServer(t, tc.status, tc.body)
			defer srv.Close()

			cfg := &ServerConfig{Name: "legacy", Transport: "streamable-http", URL: srv.URL}
			transport, err := mcpConnect(cfg, "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = transport.Close() }()

			if _, ok := transport.(*protocolSession); ok {
				t.Fatal("expected legacy transport, got modern session")
			}
			if !state.sawMethod("initialize") {
				t.Error("client did not fall back to initialize")
			}

			out, err := executeToolCall(transport, "echo", nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if out.Content != "legacy:echo" {
				t.Errorf("unexpected output: %+v", out)
			}
		})
	}
}

func TestModernHTTP_UnsupportedVersionDoesNotFallBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)
		if req.Method == "initialize" {
			t.Error("client fell back to initialize on a modern error")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
			Error: &jsonrpcError{
				Code:    codeUnsupportedProtocolVersion,
				Message: "Unsupported protocol version",
				Data:    json.RawMessage(`{"supported":["2099-01-01"],"requested":"2026-07-28"}`),
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &ServerConfig{Name: "future", Transport: "streamable-http", URL: srv.URL}
	_, err := mcpConnect(cfg, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "2099-01-01") {
		t.Errorf("error should name the server's supported versions: %v", err)
	}
}

func TestModernHTTP_NetworkErrorDoesNotFallBack(t *testing.T) {
	// Connection refused must surface, not silently retry as legacy.
	cfg := &ServerConfig{Name: "down", Transport: "streamable-http", URL: "http://127.0.0.1:1"}
	_, err := mcpConnect(cfg, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "initialize") {
		t.Errorf("network failure should not reach the legacy handshake: %v", err)
	}
}

func TestEncodeHeaderValue(t *testing.T) {
	// Encoding examples from the streamable HTTP transport spec.
	cases := []struct {
		in, want string
	}{
		{"us-west1", "us-west1"},
		{"get_weather", "get_weather"},
		{"file:///projects/myapp/config.json", "file:///projects/myapp/config.json"},
		{"Hello, 世界", "=?base64?SGVsbG8sIOS4lueVjA==?="},
		{" padded ", "=?base64?IHBhZGRlZCA=?="},
		{"line1\nline2", "=?base64?bGluZTEKbGluZTI=?="},
		{"=?base64?literal?=", "=?base64?PT9iYXNlNjQ/bGl0ZXJhbD89?="},
	}
	for _, tc := range cases {
		if got := encodeHeaderValue(tc.in); got != tc.want {
			t.Errorf("encodeHeaderValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMcpNameForRequest(t *testing.T) {
	cases := []struct {
		method string
		params any
		want   string
		ok     bool
	}{
		{"tools/call", toolCallParams{Name: "search"}, "search", true},
		{"prompts/get", map[string]any{"name": "greeting"}, "greeting", true},
		{"resources/read", resourceReadParams{URI: "file:///a.txt"}, "file:///a.txt", true},
		{"tools/list", toolsListParams{}, "", false},
		{"server/discover", nil, "", false},
	}
	for _, tc := range cases {
		got, ok := mcpNameForRequest(jsonrpcRequest{Method: tc.method, Params: tc.params})
		if got != tc.want || ok != tc.ok {
			t.Errorf("mcpNameForRequest(%s) = %q, %v; want %q, %v", tc.method, got, ok, tc.want, tc.ok)
		}
	}
}

func TestModernHTTP_NonASCIIToolName(t *testing.T) {
	// A non-header-safe tool name must arrive base64-sentinel-encoded.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "server/discover":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{protocolVersionModern},
			})
		case "tools/call":
			if got := r.Header.Get("Mcp-Name"); got != "=?base64?5qSc57Si?=" {
				t.Errorf("Mcp-Name = %q, want base64 sentinel encoding", got)
			}
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"content":    []contentBlock{{Type: "text", Text: "ok"}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := &ServerConfig{Name: "intl", Transport: "streamable-http", URL: srv.URL}
	transport, err := mcpConnect(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Close() }()

	out, err := executeToolCall(transport, "検索", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "ok" {
		t.Errorf("unexpected output: %+v", out)
	}
}
