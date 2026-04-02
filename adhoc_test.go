package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newMockMCPServer creates an httptest.Server that speaks enough MCP protocol
// for testing ad-hoc connections. It handles initialize, notifications/initialized,
// tools/list, tools/call, and ping.
func newMockMCPServer(t *testing.T, tools []mcpTool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}

		body, _ := io.ReadAll(r.Body)

		// Could be a notification (no id) or a request.
		var raw map[string]json.RawMessage
		json.Unmarshal(body, &raw)

		// Notification — no ID field.
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req jsonrpcRequest
		json.Unmarshal(body, &req)

		var resp jsonrpcResponse
		resp.JSONRPC = "2.0"
		resp.ID = json.RawMessage(fmt.Sprintf("%d", req.ID))

		switch req.Method {
		case "initialize":
			result, _ := json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
			resp.Result = result

		case "tools/list":
			result, _ := json.Marshal(toolsListResult{Tools: tools})
			resp.Result = result

		case "tools/call":
			data, _ := json.Marshal(req.Params)
			var params toolCallParams
			json.Unmarshal(data, &params)

			result, _ := json.Marshal(toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "called:" + params.Name}},
			})
			resp.Result = result

		case "ping":
			resp.Result = json.RawMessage("{}")

		default:
			resp.Error = &jsonrpcError{Code: -32601, Message: "method not found"}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

// newMockMCPServerWithContent creates a mock MCP server that returns the given
// content string from tools/call responses.
func newMockMCPServerWithContent(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req jsonrpcRequest
		json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
		case "tools/call":
			resp.Result, _ = json.Marshal(toolCallResult{
				Content: []contentBlock{{Type: "text", Text: content}},
			})
		default:
			resp.Error = &jsonrpcError{Code: -32601, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestIsURL(t *testing.T) {
	urls := []string{
		"http://localhost:8080",
		"https://example.com/mcp",
		"http://127.0.0.1:3000/path",
	}
	for _, u := range urls {
		if !isURL(u) {
			t.Errorf("expected %q to be detected as URL", u)
		}
	}

	names := []string{"myserver", "my-server", "my.server", "abc123"}
	for _, n := range names {
		if isURL(n) {
			t.Errorf("expected %q to NOT be detected as URL", n)
		}
	}
}

func TestCmdCall_AdhocURL(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockMCPServer(t, nil)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdCall([]string{srv.URL, "echo", "--params", `{"msg":"hi"}`})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("invalid JSON output: %s", data)
	}
	if out.Content != "called:echo" {
		t.Errorf("expected 'called:echo', got %q", out.Content)
	}
}

func TestCmdCall_AdhocURL_InvalidScheme(t *testing.T) {
	err := cmdCall([]string{"http://evil.example.com/mcp", "tool"})
	if err == nil {
		t.Fatal("expected error for non-localhost HTTP")
	}
	if !strings.Contains(err.Error(), "requires HTTPS") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCmdCall_AdhocURL_AuthToken(t *testing.T) {
	setupTestConfigDir(t)

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		gotAuth = r.Header.Get("Authorization")

		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req jsonrpcRequest
		json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
		case "tools/call":
			resp.Result, _ = json.Marshal(toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "ok"}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	t.Setenv("MCP_AUTH_TOKEN", "test-secret-token")

	var err error
	captureStdout(t, func() {
		err = cmdCall([]string{srv.URL, "tool", "--params", `{}`})
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer test-secret-token" {
		t.Errorf("expected 'Bearer test-secret-token', got %q", gotAuth)
	}
}

func TestCmdTools_AdhocURL(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockMCPServer(t, []mcpTool{
		{Name: "tool-a", Description: "first"},
		{Name: "tool-b", Description: "second"},
	})
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdTools([]string{srv.URL, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var tools []toolOutput
	if err := json.Unmarshal([]byte(data), &tools); err != nil {
		t.Fatalf("invalid JSON: %s", data)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "tool-a" || tools[1].Name != "tool-b" {
		t.Errorf("unexpected tools: %+v", tools)
	}
}

func TestCmdPing_AdhocURL(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockMCPServer(t, nil)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdPing([]string{srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(data, `"status": "ok"`) {
		t.Errorf("expected ok status, got: %s", data)
	}
}

func TestCmdCall_AdhocURL_DynamicFlagsAsStrings(t *testing.T) {
	setupTestConfigDir(t)

	var gotArgs map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}

		var req jsonrpcRequest
		json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
		case "tools/call":
			data, _ := json.Marshal(req.Params)
			var params toolCallParams
			json.Unmarshal(data, &params)
			gotArgs = params.Arguments
			resp.Result, _ = json.Marshal(toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "ok"}},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var err error
	captureStdout(t, func() {
		err = cmdCall([]string{srv.URL, "tool", "--count", "42", "--verbose"})
	})
	if err != nil {
		t.Fatal(err)
	}

	// Ad-hoc has no schema cache, so flags are passed as strings.
	if gotArgs["count"] != "42" {
		t.Errorf("expected count='42', got %v", gotArgs["count"])
	}
	if gotArgs["verbose"] != "true" {
		t.Errorf("expected verbose='true', got %v", gotArgs["verbose"])
	}
}

func TestCmdCall_RegisteredServer_StillWorks(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockMCPServer(t, nil)
	defer srv.Close()

	// Register a server the traditional way.
	if err := addServerConfig(ServerConfig{
		Name:      "registered",
		Transport: "streamable-http",
		URL:       srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	data := captureStdout(t, func() {
		err = cmdCall([]string{"registered", "echo", "--params", `{}`})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	json.Unmarshal([]byte(data), &out)
	if out.Content != "called:echo" {
		t.Errorf("expected 'called:echo', got %q", out.Content)
	}
}
