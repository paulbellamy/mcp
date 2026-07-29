package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Custom parameter header (x-mcp-header -> Mcp-Param-*) tests for the
// 2026-07-28 streamable HTTP transport.

func TestExtractHeaderParams_Valid(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		want   []headerParam
	}{
		{
			name:   "no annotations",
			schema: `{"type":"object","properties":{"q":{"type":"string"}}}`,
			want:   nil,
		},
		{
			name:   "empty schema",
			schema: "",
			want:   nil,
		},
		{
			name: "top-level string",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":"Region"}}}`,
			want: []headerParam{{Path: []string{"region"}, Name: "Region"}},
		},
		{
			name: "nested through properties chain",
			schema: `{"type":"object","properties":{
				"config":{"type":"object","properties":{
					"region":{"type":"string","x-mcp-header":"Region"}}}}}`,
			want: []headerParam{{Path: []string{"config", "region"}, Name: "Region"}},
		},
		{
			name: "multiple primitives sorted by name",
			schema: `{"type":"object","properties":{
				"count":{"type":"integer","x-mcp-header":"Count"},
				"dry_run":{"type":"boolean","x-mcp-header":"Dry-Run"},
				"region":{"type":"string","x-mcp-header":"Region"}}}`,
			want: []headerParam{
				{Path: []string{"count"}, Name: "Count"},
				{Path: []string{"dry_run"}, Name: "Dry-Run"},
				{Path: []string{"region"}, Name: "Region"},
			},
		},
		{
			name: "annotation-shaped data values ignored",
			schema: `{"type":"object","properties":{
				"cfg":{"type":"object","default":{"x-mcp-header":"NotAnAnnotation"}}}}`,
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractHeaderParams(json.RawMessage(tc.schema))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("params = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestExtractHeaderParams_Invalid(t *testing.T) {
	cases := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{
			name: "empty header name",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":""}}}`,
			wantErr: "token",
		},
		{
			name: "non-string annotation",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":42}}}`,
			wantErr: "must be a string",
		},
		{
			name: "invalid tchar space",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":"My Region"}}}`,
			wantErr: "token",
		},
		{
			name: "invalid tchar colon",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":"Region:1"}}}`,
			wantErr: "token",
		},
		{
			name: "invalid tchar non-ASCII",
			schema: `{"type":"object","properties":{
				"region":{"type":"string","x-mcp-header":"Regíon"}}}`,
			wantErr: "token",
		},
		{
			name: "case-insensitive duplicate",
			schema: `{"type":"object","properties":{
				"a":{"type":"string","x-mcp-header":"Region"},
				"b":{"type":"string","x-mcp-header":"REGION"}}}`,
			wantErr: "duplicates",
		},
		{
			name: "number type not permitted",
			schema: `{"type":"object","properties":{
				"ratio":{"type":"number","x-mcp-header":"Ratio"}}}`,
			wantErr: "type string, integer, or boolean",
		},
		{
			name: "array type not permitted",
			schema: `{"type":"object","properties":{
				"tags":{"type":"array","x-mcp-header":"Tags"}}}`,
			wantErr: "type string, integer, or boolean",
		},
		{
			name: "annotation on schema root",
			schema: `{"type":"object","x-mcp-header":"Root","properties":{
				"q":{"type":"string"}}}`,
			wantErr: "statically reachable",
		},
		{
			name: "under items",
			schema: `{"type":"object","properties":{
				"list":{"type":"array","items":{"type":"string","x-mcp-header":"Item"}}}}`,
			wantErr: "statically reachable",
		},
		{
			name: "under oneOf",
			schema: `{"type":"object","properties":{
				"v":{"oneOf":[{"type":"string","x-mcp-header":"V"}]}}}`,
			wantErr: "statically reachable",
		},
		{
			name: "under $ref target",
			schema: `{"type":"object",
				"properties":{"region":{"$ref":"#/$defs/r"}},
				"$defs":{"r":{"type":"string","x-mcp-header":"Region"}}}`,
			wantErr: "statically reachable",
		},
		{
			name: "under then",
			schema: `{"type":"object","properties":{"q":{"type":"string"}},
				"then":{"properties":{"r":{"type":"string","x-mcp-header":"R"}}}}`,
			wantErr: "statically reachable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params, err := extractHeaderParams(json.RawMessage(tc.schema))
			if err == nil {
				t.Fatalf("expected error, got params %+v", params)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q should contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestHeaderValuesForCall(t *testing.T) {
	params := []headerParam{
		{Path: []string{"region"}, Name: "Region"},
		{Path: []string{"count"}, Name: "Count"},
		{Path: []string{"verbose"}, Name: "Verbose"},
		{Path: []string{"dry"}, Name: "Dry"},
		{Path: []string{"config", "zone"}, Name: "Zone"},
		{Path: []string{"note"}, Name: "Note"},
		{Path: []string{"missing"}, Name: "Missing"},
		{Path: []string{"nothing"}, Name: "Nothing"},
		{Path: []string{"blob"}, Name: "Blob"},
		{Path: []string{"huge"}, Name: "Huge"},
		{Path: []string{"frac"}, Name: "Frac"},
	}
	args := map[string]any{
		"region":  "us-west1",
		"count":   float64(42), // JSON numbers decode as float64
		"verbose": true,
		"dry":     false,
		"config":  map[string]any{"zone": "z-1"},
		"note":    "Hello, 世界",
		"nothing": nil,
		"blob":    map[string]any{"k": "v"},
		"huge":    float64(1 << 53), // beyond ±(2^53−1)
		"frac":    1.5,
	}

	got := headerValuesForCall(params, args)
	want := map[string]string{
		"Mcp-Param-Region":  "us-west1",
		"Mcp-Param-Count":   "42",
		"Mcp-Param-Verbose": "true",
		"Mcp-Param-Dry":     "false",
		"Mcp-Param-Zone":    "z-1",
		"Mcp-Param-Note":    "=?base64?SGVsbG8sIOS4lueVjA==?=",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("headers = %v, want %v", got, want)
	}
}

func TestHeaderValuesForCall_IntBounds(t *testing.T) {
	params := []headerParam{{Path: []string{"n"}, Name: "N"}}
	cases := []struct {
		in   any
		want string // "" = header omitted
	}{
		{int(7), "7"},
		{int64(-3), "-3"},
		{float64(9007199254740991), "9007199254740991"},
		{float64(-9007199254740991), "-9007199254740991"},
		{float64(9007199254740992), ""},
		{int64(1) << 60, ""},
		{0.25, ""},
	}
	for _, tc := range cases {
		got := headerValuesForCall(params, map[string]any{"n": tc.in})
		if tc.want == "" {
			if len(got) != 0 {
				t.Errorf("%v: expected header omitted, got %v", tc.in, got)
			}
		} else if got["Mcp-Param-N"] != tc.want {
			t.Errorf("%v: header = %q, want %q", tc.in, got["Mcp-Param-N"], tc.want)
		}
	}
}

func TestProtocolSessionDecorate_PreservesExtraHeaders(t *testing.T) {
	s := newProtocolSession(&mockTransport{}, protocolVersionModern)
	req := jsonrpcRequest{
		JSONRPC:      jsonrpcVersion,
		ID:           1,
		Method:       "tools/call",
		Params:       toolCallParams{Name: "t", Arguments: map[string]any{}},
		ExtraHeaders: map[string]string{"Mcp-Param-Region": "us-west1"},
	}
	got := s.decorate(req)
	if got.ExtraHeaders["Mcp-Param-Region"] != "us-west1" {
		t.Errorf("decorate dropped ExtraHeaders: %v", got.ExtraHeaders)
	}
}

// annotatedDeploySchema requires the region argument mirrored as
// Mcp-Param-Region and count as Mcp-Param-Count.
const annotatedDeploySchema = `{"type":"object","properties":{
	"region":{"type":"string","x-mcp-header":"Region"},
	"count":{"type":"integer","x-mcp-header":"Count"}}}`

// newHeaderMCPServer is a modern (2026-07-28) mock whose tools/call
// validates the Mcp-Param-* headers against the request body, answering
// HeaderMismatch (-32020) inside an HTTP 400 when they disagree — the
// server-side check the custom-headers spec mandates.
func newHeaderMCPServer(t *testing.T, schema string) (*httptest.Server, *headerServerState) {
	t.Helper()
	state := &headerServerState{}

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
		case "tools/list":
			state.mu.Lock()
			state.listCalls++
			state.mu.Unlock()
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"tools":      []mcpTool{{Name: "deploy", InputSchema: json.RawMessage(schema)}},
			})
		case "tools/call":
			state.mu.Lock()
			state.callCalls++
			state.mu.Unlock()

			var params toolCallParams
			data, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(data, &params)
			region, _ := params.Arguments["region"].(string)
			count, _ := params.Arguments["count"].(float64)
			if r.Header.Get("Mcp-Param-Region") != region ||
				r.Header.Get("Mcp-Param-Count") != fmt.Sprintf("%d", int64(count)) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				resp.Error = &jsonrpcError{Code: codeHeaderMismatch, Message: "header/body mismatch"}
				_ = json.NewEncoder(w).Encode(resp)
				return
			}
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"content":    []contentBlock{{Type: "text", Text: "deployed:" + region}},
			})
		default:
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))

	return srv, state
}

type headerServerState struct {
	mu        sync.Mutex
	listCalls int
	callCalls int
}

func (s *headerServerState) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listCalls, s.callCalls
}

func TestCmdCall_McpParamHeadersEndToEnd(t *testing.T) {
	setupTestConfigDir(t)
	srv, state := newHeaderMCPServer(t, annotatedDeploySchema)
	defer srv.Close()

	if err := addServerConfig(ServerConfig{
		Name:      "hdr",
		Transport: "streamable-http",
		URL:       srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	// Fresh cache carrying the annotated schema, as `mcp tools` leaves it.
	if err := saveCachedTools("hdr", []toolOutput{
		{Server: "hdr", Name: "deploy", InputSchema: json.RawMessage(annotatedDeploySchema)},
	}, 0); err != nil {
		t.Fatal(err)
	}

	var err error
	data := captureStdout(t, func() {
		err = cmdCall([]string{"hdr", "deploy", "--params", `{"region":"us-west1","count":3}`})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("invalid JSON output: %s", data)
	}
	if out.Content != "deployed:us-west1" || out.IsError {
		t.Errorf("unexpected output: %+v", out)
	}
	if _, calls := state.counts(); calls != 1 {
		t.Errorf("expected exactly 1 tools/call (headers correct on first try), got %d", calls)
	}
}

func TestCmdCall_HeaderMismatchRefreshesAndRetries(t *testing.T) {
	setupTestConfigDir(t)
	srv, state := newHeaderMCPServer(t, annotatedDeploySchema)
	defer srv.Close()

	if err := addServerConfig(ServerConfig{
		Name:      "hdr",
		Transport: "streamable-http",
		URL:       srv.URL,
	}); err != nil {
		t.Fatal(err)
	}
	// Stale-schema cache from before the server added annotations: the first
	// call sends no Mcp-Param-* headers and the server answers -32020.
	if err := saveCachedTools("hdr", []toolOutput{
		{Server: "hdr", Name: "deploy", InputSchema: json.RawMessage(`{"type":"object","properties":{"region":{"type":"string"},"count":{"type":"integer"}}}`)},
	}, 0); err != nil {
		t.Fatal(err)
	}

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			err = cmdCall([]string{"hdr", "deploy", "--params", `{"region":"eu-central1","count":2}`})
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("invalid JSON output: %s", stdout)
	}
	if out.Content != "deployed:eu-central1" || out.IsError {
		t.Errorf("unexpected output: %+v", out)
	}
	lists, calls := state.counts()
	if lists != 1 {
		t.Errorf("expected 1 tools/list refresh, got %d", lists)
	}
	if calls != 2 {
		t.Errorf("expected 2 tools/call attempts (mismatch then retry), got %d", calls)
	}
	if !strings.Contains(stderr, "header mismatch") {
		t.Errorf("expected a header-mismatch warning on stderr, got: %q", stderr)
	}
}

func TestCmdCall_SecondHeaderMismatchSurfaced(t *testing.T) {
	setupTestConfigDir(t)

	// A modern server that answers every tools/call with a raw JSON-RPC
	// -32020 error (HTTP 200), exercising the non-400 delivery path.
	var callCalls int
	var mu sync.Mutex
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
		case "tools/list":
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType": "complete",
				"tools":      []mcpTool{{Name: "deploy", InputSchema: json.RawMessage(annotatedDeploySchema)}},
			})
		case "tools/call":
			mu.Lock()
			callCalls++
			mu.Unlock()
			resp.Error = &jsonrpcError{Code: codeHeaderMismatch, Message: "header/body mismatch"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	if err := addServerConfig(ServerConfig{
		Name:      "hdr2",
		Transport: "streamable-http",
		URL:       srv.URL,
	}); err != nil {
		t.Fatal(err)
	}

	var err error
	captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmdCall([]string{"hdr2", "deploy", "--params", `{"region":"x","count":1}`})
		})
	})
	if err == nil {
		t.Fatal("expected the second -32020 to surface as an error")
	}
	if !strings.Contains(err.Error(), "header/body mismatch") {
		t.Errorf("error should carry the server's message: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCalls != 2 {
		t.Errorf("expected exactly 2 tools/call attempts (one retry), got %d", callCalls)
	}
}

func TestListAllTools_ModernHTTPDropsInvalidTool(t *testing.T) {
	invalid := `{"type":"object","properties":{"ratio":{"type":"number","x-mcp-header":"Ratio"}}}`
	srv, _ := newModernMCPServer(t, []mcpTool{
		{Name: "good", InputSchema: json.RawMessage(annotatedDeploySchema)},
		{Name: "bad", InputSchema: json.RawMessage(invalid)},
	}, 0)
	defer srv.Close()

	cfg := &ServerConfig{Name: "modern", Transport: "streamable-http", URL: srv.URL}
	transport, err := mcpConnect(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Close() }()

	var tools []toolOutput
	stderr := captureStderr(t, func() {
		tools, _, err = listAllTools(transport, "modern")
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "good" {
		t.Errorf("expected only the valid tool, got %+v", tools)
	}
	if !strings.Contains(stderr, `"bad"`) || !strings.Contains(stderr, "x-mcp-header") {
		t.Errorf("warning should name the dropped tool and reason, got: %q", stderr)
	}
}

func TestListAllTools_StdioIgnoresAnnotations(t *testing.T) {
	// Stdio clients MAY ignore annotations; ours does — nothing is dropped
	// even on a modern (protocolSession) stdio connection.
	invalid := `{"type":"object","properties":{"ratio":{"type":"number","x-mcp-header":"Ratio"}}}`
	inner := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{
				"tools": []mcpTool{
					{Name: "good", InputSchema: json.RawMessage(annotatedDeploySchema)},
					{Name: "bad", InputSchema: json.RawMessage(invalid)},
				},
			}), nil
		},
	}
	transport := newProtocolSession(inner, protocolVersionModern)

	tools, _, err := listAllTools(transport, "stdio")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Errorf("stdio must not drop tools over annotations, got %+v", tools)
	}
}
