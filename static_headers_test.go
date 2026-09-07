package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestParseHeaderFlag(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantName  string
		wantValue string
		wantErr   bool
	}{
		{"simple", "X-Org-Id: acme", "X-Org-Id", "acme", false},
		{"canonicalized", "x-org-id: acme", "X-Org-Id", "acme", false},
		{"bearer with colon in value", "Authorization: Bearer a:b:c", "Authorization", "Bearer a:b:c", false},
		{"env ref preserved", "Authorization: Bearer ${KEY}", "Authorization", "Bearer ${KEY}", false},
		{"trims surrounding space", "  X-Foo :   bar  ", "X-Foo", "bar", false},
		{"no colon", "X-Org-Id acme", "", "", true},
		{"empty value", "X-Org-Id:   ", "", "", true},
		{"empty value no space", "X-Org-Id:", "", "", true},
		{"bad name token", "X Org: v", "", "", true},
		{"control char in value", "X-Foo: a\nb", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotValue, err := parseHeaderFlag(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseHeaderFlag(%q) = (%q,%q), want error", tc.in, gotName, gotValue)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHeaderFlag(%q) unexpected error: %v", tc.in, err)
			}
			if gotName != tc.wantName || gotValue != tc.wantValue {
				t.Errorf("parseHeaderFlag(%q) = (%q,%q), want (%q,%q)", tc.in, gotName, gotValue, tc.wantName, tc.wantValue)
			}
		})
	}
}

func TestParseHeaderFlags_LastWins(t *testing.T) {
	got, err := parseHeaderFlags([]string{"X-Org-Id: one", "x-org-id: two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["X-Org-Id"] != "two" {
		t.Errorf("parseHeaderFlags last-wins failed: %v", got)
	}
}

func TestResolveHeaders(t *testing.T) {
	t.Setenv("TEST_HDR_KEY", "cog_secret")
	t.Setenv("TEST_HDR_ORG", "acme")

	t.Run("literal passthrough", func(t *testing.T) {
		got, err := resolveHeaders(map[string]string{"X-Org-Id": "acme"})
		if err != nil {
			t.Fatal(err)
		}
		if got["X-Org-Id"] != "acme" {
			t.Errorf("got %v", got)
		}
	})

	t.Run("env expansion", func(t *testing.T) {
		got, err := resolveHeaders(map[string]string{
			"Authorization": "Bearer ${TEST_HDR_KEY}",
			"X-Org-Id":      "${TEST_HDR_ORG}",
		})
		if err != nil {
			t.Fatal(err)
		}
		if got["Authorization"] != "Bearer cog_secret" {
			t.Errorf("Authorization = %q", got["Authorization"])
		}
		if got["X-Org-Id"] != "acme" {
			t.Errorf("X-Org-Id = %q", got["X-Org-Id"])
		}
	})

	t.Run("unset var errors", func(t *testing.T) {
		_, err := resolveHeaders(map[string]string{"Authorization": "Bearer ${TEST_HDR_MISSING}"})
		if err == nil {
			t.Fatal("expected error for unset env var")
		}
	})

	t.Run("nil in nil out", func(t *testing.T) {
		got, err := resolveHeaders(nil)
		if err != nil || got != nil {
			t.Errorf("resolveHeaders(nil) = (%v, %v)", got, err)
		}
	})
}

func TestExpandHeaderEnv_NonRefsLiteral(t *testing.T) {
	// A lone $ or a $NAME without braces is not a reference; left verbatim.
	got, err := expandHeaderEnv("price is $5 for ${x} costs")
	if err == nil {
		t.Fatalf("expected error for unset ${x}, got %q", got)
	}
	got, err = expandHeaderEnv("a $bare literal $ sign")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a $bare literal $ sign" {
		t.Errorf("got %q", got)
	}
}

// recordingHandler captures the headers of the last request it served.
type recordingHandler struct {
	mu   sync.Mutex
	last http.Header
}

func (h *recordingHandler) capture(r *http.Request) {
	h.mu.Lock()
	h.last = r.Header.Clone()
	h.mu.Unlock()
}

func (h *recordingHandler) get(key string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last.Get(key)
}

func TestHTTPTransport_StaticHeaders(t *testing.T) {
	rec := &recordingHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage("{}")})
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	transport.setHeaders(map[string]string{"X-Org-Id": "acme", "Authorization": "Bearer static-key"})
	if _, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.get("X-Org-Id"); got != "acme" {
		t.Errorf("X-Org-Id = %q, want acme", got)
	}
	// With no OAuth token, the configured Authorization stands.
	if got := rec.get("Authorization"); got != "Bearer static-key" {
		t.Errorf("Authorization = %q, want static", got)
	}
}

func TestHTTPTransport_TokenOverridesStaticAuthorization(t *testing.T) {
	rec := &recordingHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage("{}")})
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "oauth-token")
	transport.setHeaders(map[string]string{"Authorization": "Bearer static-key", "X-Org-Id": "acme"})
	if _, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.get("Authorization"); got != "Bearer oauth-token" {
		t.Errorf("Authorization = %q, want OAuth token to win", got)
	}
	if got := rec.get("X-Org-Id"); got != "acme" {
		t.Errorf("X-Org-Id = %q, want acme", got)
	}
}

func TestHTTPTransport_StaticHeaders_OnNotify(t *testing.T) {
	rec := &recordingHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	transport.setHeaders(map[string]string{"X-Org-Id": "acme"})
	if err := transport.Notify(jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
	if got := rec.get("X-Org-Id"); got != "acme" {
		t.Errorf("X-Org-Id on notify = %q, want acme", got)
	}
}

// newRecordingModernServer speaks the minimum of the modern protocol needed
// for mcpConnect + tools/list, and records the headers of the last request.
func newRecordingModernServer(t *testing.T) (*httptest.Server, *recordingHandler) {
	t.Helper()
	rec := &recordingHandler{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.capture(r)
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "server/discover":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{protocolVersionModern},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			})
		case "tools/list":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"tools":      []mcpTool{{Name: "echo", Description: "echoes"}},
			})
		default:
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	return srv, rec
}

func TestCmdAdd_Headers_EndToEnd(t *testing.T) {
	setupTestConfigDir(t)
	t.Setenv("TEST_DEVIN_KEY", "cog_live_secret")

	srv, rec := newRecordingModernServer(t)
	defer srv.Close()

	_ = captureStderr(t, func() {
		err := cmdAdd([]string{"devin", srv.URL,
			"--header", "X-Org-Id: acme",
			"-H", "Authorization: Bearer ${TEST_DEVIN_KEY}",
		})
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	})

	// Persisted raw (env ref intact); expansion is deferred to connect time.
	cfg, err := getServerConfig("devin")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Headers["X-Org-Id"] != "acme" {
		t.Errorf("stored X-Org-Id = %q", cfg.Headers["X-Org-Id"])
	}
	if cfg.Headers["Authorization"] != "Bearer ${TEST_DEVIN_KEY}" {
		t.Errorf("stored Authorization = %q, want raw env ref", cfg.Headers["Authorization"])
	}

	if got := rec.get("X-Org-Id"); got != "acme" {
		t.Errorf("server X-Org-Id = %q", got)
	}
	if got := rec.get("Authorization"); got != "Bearer cog_live_secret" {
		t.Errorf("server Authorization = %q, want expanded secret", got)
	}
}

func TestCmdAdd_Headers_RejectedForStdio(t *testing.T) {
	setupTestConfigDir(t)
	// --header BEFORE --stdio is meaningless (stdio has no HTTP headers).
	err := cmdAdd([]string{"foo", "-H", "X-Org-Id: acme", "--stdio", "echo", "hi"})
	if err == nil {
		t.Fatal("expected error: --header before --stdio")
	}
}

func TestCmdAdd_Stdio_ChildFlagsPassThrough(t *testing.T) {
	setupTestConfigDir(t)
	// A stdio command's own flags after --stdio must reach it verbatim.
	_ = captureStderr(t, func() {
		// Nonexistent command so discovery fails fast, dodging the 60s stdio
		// handshake timeout a real command would incur.
		if err := cmdAdd([]string{"wrapped", "--stdio", "mcp-nonexistent-test-cmd",
			"--header", "X-Org-Id: acme", "-H", "Authorization: Bearer k"}); err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
	})
	cfg, err := getServerConfig("wrapped")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Transport != "stdio" || cfg.Command != "mcp-nonexistent-test-cmd" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	want := []string{"--header", "X-Org-Id: acme", "-H", "Authorization: Bearer k"}
	if fmt.Sprint(cfg.Args) != fmt.Sprint(want) {
		t.Errorf("child args = %v, want %v", cfg.Args, want)
	}
	if len(cfg.Headers) != 0 {
		t.Errorf("stdio server must not carry HTTP headers, got %v", cfg.Headers)
	}
}

func TestResolveServer_AdhocHeadersFromEnv(t *testing.T) {
	t.Setenv("TEST_ADHOC_KEY", "cog_adhoc")
	t.Setenv("MCP_HEADERS", "X-Org-Id: acme\nAuthorization: Bearer ${TEST_ADHOC_KEY}\n\n")

	server, _, err := resolveServer("https://example.com/mcp")
	if err != nil {
		t.Fatal(err)
	}
	// Stored raw (env ref preserved); expansion happens at connect time.
	if server.Headers["X-Org-Id"] != "acme" {
		t.Errorf("X-Org-Id = %q", server.Headers["X-Org-Id"])
	}
	if server.Headers["Authorization"] != "Bearer ${TEST_ADHOC_KEY}" {
		t.Errorf("Authorization = %q", server.Headers["Authorization"])
	}
}

func TestAdhoc_Headers_EndToEnd(t *testing.T) {
	t.Setenv("TEST_ADHOC_KEY", "cog_adhoc_secret")
	srv, rec := newRecordingModernServer(t)
	defer srv.Close()
	t.Setenv("MCP_HEADERS", "X-Org-Id: acme\nAuthorization: Bearer ${TEST_ADHOC_KEY}")

	// Ad-hoc URL: no config entry, headers sourced from MCP_HEADERS.
	server, authToken, err := resolveServer(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport, err := mcpConnect(server, authToken)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Close() }()
	if _, _, err := listAllTools(transport, "adhoc"); err != nil {
		t.Fatal(err)
	}
	if got := rec.get("X-Org-Id"); got != "acme" {
		t.Errorf("server X-Org-Id = %q", got)
	}
	if got := rec.get("Authorization"); got != "Bearer cog_adhoc_secret" {
		t.Errorf("server Authorization = %q", got)
	}
}

func TestResolveServer_AdhocBadHeaderErrors(t *testing.T) {
	t.Setenv("MCP_HEADERS", "not-a-valid-header-line")
	if _, _, err := resolveServer("https://example.com/mcp"); err == nil {
		t.Fatal("expected error for malformed MCP_HEADERS")
	}
}

func TestCmdAdd_Headers_ConnectExpansionError(t *testing.T) {
	setupTestConfigDir(t)
	srv, _ := newRecordingModernServer(t)
	defer srv.Close()

	// add tolerates the failed discovery, but the unset ${VAR} must surface
	// as a connect error, never a literal ${...} on the wire.
	if err := cmdAdd([]string{"devin", srv.URL, "-H", "Authorization: Bearer ${DEFINITELY_UNSET_HDR}"}); err != nil {
		t.Fatalf("cmdAdd should tolerate discovery failure, got %v", err)
	}
	cfg, err := getServerConfig("devin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcpConnect(cfg, ""); err == nil {
		t.Fatal("expected connect to error on unset env var in header")
	}
}
