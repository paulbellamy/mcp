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

func TestListAllResources_SinglePage(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "resources/list" {
				t.Errorf("expected method 'resources/list', got %q", req.Method)
			}
			result := resourcesListResult{
				Resources: []mcpResource{
					{URI: "file:///a.txt", Name: "a", Description: "first"},
					{URI: "file:///b.txt", Name: "b"},
				},
			}
			data, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  data,
			}, nil
		},
	}

	resources, err := listAllResources(transport, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}
	if resources[0].URI != "file:///a.txt" || resources[1].URI != "file:///b.txt" {
		t.Errorf("unexpected resources: %+v", resources)
	}
	if resources[0].Server != "srv" {
		t.Errorf("expected server 'srv', got %q", resources[0].Server)
	}
}

func TestListAllResources_Pagination(t *testing.T) {
	callCount := 0
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			callCount++
			var result resourcesListResult
			switch callCount {
			case 1:
				result = resourcesListResult{
					Resources:  []mcpResource{{URI: "r1"}},
					NextCursor: "page2",
				}
			case 2:
				params, _ := json.Marshal(req.Params)
				var p resourcesListParams
				_ = json.Unmarshal(params, &p)
				if p.Cursor != "page2" {
					t.Errorf("expected cursor 'page2', got %q", p.Cursor)
				}
				result = resourcesListResult{Resources: []mcpResource{{URI: "r2"}}}
			default:
				t.Fatal("too many calls")
			}
			data, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  data,
			}, nil
		},
	}

	resources, err := listAllResources(transport, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources across 2 pages, got %d", len(resources))
	}
	if callCount != 2 {
		t.Errorf("expected 2 requests, got %d", callCount)
	}
}

func TestListAllResources_MethodNotFound(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"},
			}, nil
		},
	}

	resources, err := listAllResources(transport, "srv")
	if err != nil {
		t.Fatalf("method-not-found should be soft, got error: %v", err)
	}
	if resources != nil {
		t.Errorf("expected nil resources, got %+v", resources)
	}
}

func TestListAllResources_OtherErrorPropagates(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: -32000, Message: "boom"},
			}, nil
		},
	}

	_, err := listAllResources(transport, "srv")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListAllResourceTemplates(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "resources/templates/list" {
				t.Errorf("expected 'resources/templates/list', got %q", req.Method)
			}
			result := resourceTemplatesListResult{
				ResourceTemplates: []mcpResourceTemplate{
					{URITemplate: "file:///{path}", Name: "files"},
				},
			}
			data, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  data,
			}, nil
		},
	}

	templates, err := listAllResourceTemplates(transport, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 1 {
		t.Fatalf("expected 1 template, got %d", len(templates))
	}
	if templates[0].URITemplate != "file:///{path}" {
		t.Errorf("unexpected template: %+v", templates[0])
	}
	if templates[0].URI != "" {
		t.Errorf("template should have empty URI, got %q", templates[0].URI)
	}
}

func TestListAllResourceTemplates_MethodNotFound(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"},
			}, nil
		},
	}

	templates, err := listAllResourceTemplates(transport, "srv")
	if err != nil {
		t.Fatalf("method-not-found should be soft, got: %v", err)
	}
	if templates != nil {
		t.Errorf("expected nil, got %+v", templates)
	}
}

func TestReadResource(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "resources/read" {
				t.Errorf("expected method 'resources/read', got %q", req.Method)
			}
			data, _ := json.Marshal(req.Params)
			var p resourceReadParams
			_ = json.Unmarshal(data, &p)
			if p.URI != "file:///a.txt" {
				t.Errorf("expected uri 'file:///a.txt', got %q", p.URI)
			}
			result := resourceReadResult{
				Contents: []resourceContents{
					{URI: p.URI, MimeType: "text/plain", Text: "hello"},
				},
			}
			rd, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  rd,
			}, nil
		},
	}

	out, err := readResource(transport, "file:///a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(out.Contents))
	}
	if out.Contents[0].Text != "hello" {
		t.Errorf("expected text 'hello', got %q", out.Contents[0].Text)
	}
	if out.Contents[0].MimeType != "text/plain" {
		t.Errorf("expected mimeType 'text/plain', got %q", out.Contents[0].MimeType)
	}
}

func TestReadResource_JSONRPCError(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: -32602, Message: "resource not found"},
			}, nil
		},
	}

	_, err := readResource(transport, "file:///missing.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "resource not found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestTruncateReadOutput(t *testing.T) {
	setupTestConfigDir(t)

	out := readOutput{Contents: []readContent{{Text: strings.Repeat("x", 200)}}}
	truncateReadOutput(&out, 50, "srv", "file:///a.txt")

	if !out.Truncated {
		t.Error("expected Truncated=true")
	}
	if !strings.Contains(out.Contents[0].Text, "[output truncated at 50 chars]") {
		t.Errorf("expected truncation note, got %q", out.Contents[0].Text)
	}
	// First 50 chars preserved before the note.
	if !strings.HasPrefix(out.Contents[0].Text, strings.Repeat("x", 50)) {
		t.Errorf("expected 50 preserved chars, got %q", out.Contents[0].Text)
	}
}

func TestTruncateReadOutput_NoTruncation(t *testing.T) {
	out := readOutput{Contents: []readContent{{Text: "short"}}}
	truncateReadOutput(&out, 50, "srv", "file:///a.txt")
	if out.Truncated {
		t.Error("expected Truncated=false for short content")
	}
	if out.Contents[0].Text != "short" {
		t.Errorf("content should be unchanged, got %q", out.Contents[0].Text)
	}
}

func TestTruncateReadOutput_Disabled(t *testing.T) {
	out := readOutput{Contents: []readContent{{Text: strings.Repeat("x", 200)}}}
	truncateReadOutput(&out, 0, "srv", "file:///a.txt")
	if out.Truncated {
		t.Error("maxOutput=0 should disable truncation")
	}
	if len(out.Contents[0].Text) != 200 {
		t.Errorf("expected full content, got %d chars", len(out.Contents[0].Text))
	}
}

func TestOutputResourcesList_JSON(t *testing.T) {
	resources := []resourceOutput{
		{Server: "srv", URI: "file:///b", Name: "b"},
		{Server: "srv", URI: "file:///a", Name: "a"},
	}

	out := captureStdout(t, func() {
		if err := outputResourcesList(resources, "", true); err != nil {
			t.Fatal(err)
		}
	})

	var got []resourceOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(got))
	}
	// Sorted by uri within the server.
	if got[0].URI != "file:///a" || got[1].URI != "file:///b" {
		t.Errorf("expected sorted by uri, got %+v", got)
	}
}

func TestOutputResourcesList_QueryFilter(t *testing.T) {
	resources := []resourceOutput{
		{Server: "srv", URI: "file:///report.txt", Description: "quarterly report"},
		{Server: "srv", URI: "file:///image.png", Description: "a picture"},
	}

	out := captureStdout(t, func() {
		if err := outputResourcesList(resources, "report", true); err != nil {
			t.Fatal(err)
		}
	})

	var got []resourceOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 filtered resource, got %d", len(got))
	}
	if got[0].URI != "file:///report.txt" {
		t.Errorf("unexpected match: %+v", got[0])
	}
}

func TestPrintResourcesHuman(t *testing.T) {
	resources := []resourceOutput{
		{Server: "notion", URI: "notion://page/1", Title: "My Page", Description: "about X"},
		{Server: "notion", URITemplate: "notion://db/{id}", Name: "query"},
	}

	out := captureStdout(t, func() {
		if err := printResourcesHuman(resources); err != nil {
			t.Fatal(err)
		}
	})

	if !strings.Contains(out, "notion (2 resources)") {
		t.Errorf("expected server header, got:\n%s", out)
	}
	if !strings.Contains(out, "notion://page/1") || !strings.Contains(out, "My Page") {
		t.Errorf("expected concrete resource, got:\n%s", out)
	}
	if !strings.Contains(out, "notion://db/{id}") || !strings.Contains(out, "(template)") {
		t.Errorf("expected template marker, got:\n%s", out)
	}
}

func TestPrintResourcesHuman_Empty(t *testing.T) {
	out := captureStderr(t, func() {
		if err := printResourcesHuman(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "No resources found") {
		t.Errorf("expected 'No resources found', got %q", out)
	}
}

// newMockResourceServer creates an httptest.Server that speaks enough of the
// MCP resource protocol for command-level tests.
func newMockResourceServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
		case "resources/list":
			resp.Result, _ = json.Marshal(resourcesListResult{
				Resources: []mcpResource{
					{URI: "notion://page/1", Name: "page-1", Description: "first page"},
				},
			})
		case "resources/templates/list":
			resp.Result, _ = json.Marshal(resourceTemplatesListResult{
				ResourceTemplates: []mcpResourceTemplate{
					{URITemplate: "notion://page/{id}", Name: "page"},
				},
			})
		case "resources/read":
			data, _ := json.Marshal(req.Params)
			var p resourceReadParams
			_ = json.Unmarshal(data, &p)
			resp.Result, _ = json.Marshal(resourceReadResult{
				Contents: []resourceContents{
					{URI: p.URI, MimeType: "text/plain", Text: "contents of " + p.URI},
				},
			})
		default:
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestCmdResources_AdhocURL(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockResourceServer(t)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdResources([]string{srv.URL, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var got []resourceOutput
	if err := json.Unmarshal([]byte(data), &got); err != nil {
		t.Fatalf("invalid JSON: %s", data)
	}
	if len(got) != 2 {
		t.Fatalf("expected 1 resource + 1 template, got %d: %s", len(got), data)
	}

	var sawResource, sawTemplate bool
	for _, r := range got {
		if r.URI == "notion://page/1" {
			sawResource = true
		}
		if r.URITemplate == "notion://page/{id}" {
			sawTemplate = true
		}
	}
	if !sawResource {
		t.Errorf("expected concrete resource in output: %s", data)
	}
	if !sawTemplate {
		t.Errorf("expected template in output: %s", data)
	}
}

func TestCmdRead_AdhocURL(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockResourceServer(t)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdRead([]string{srv.URL, "notion://page/1"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out readOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("invalid JSON: %s", data)
	}
	if len(out.Contents) != 1 {
		t.Fatalf("expected 1 content, got %d", len(out.Contents))
	}
	if out.Contents[0].Text != "contents of notion://page/1" {
		t.Errorf("unexpected content: %q", out.Contents[0].Text)
	}
}

func TestCmdRead_Truncation(t *testing.T) {
	setupTestConfigDir(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(body, &raw)
		if _, hasID := raw["id"]; !hasID {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		switch req.Method {
		case "initialize":
			resp.Result, _ = json.Marshal(map[string]any{
				"protocolVersion": "2025-03-26",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]string{"name": "mock"},
			})
		case "resources/read":
			resp.Result, _ = json.Marshal(resourceReadResult{
				Contents: []resourceContents{{Text: strings.Repeat("x", 200)}},
			})
		default:
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdRead([]string{srv.URL, "file:///big.txt", "--max-output", "50"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out readOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("invalid JSON: %s", data)
	}
	if !out.Truncated {
		t.Error("expected Truncated=true")
	}
	if !strings.Contains(out.Contents[0].Text, "[output truncated at 50 chars]") {
		t.Errorf("expected truncation note, got %q", out.Contents[0].Text)
	}
}

func TestCmdRead_MissingArgs(t *testing.T) {
	err := cmdRead([]string{"server"})
	if err == nil {
		t.Fatal("expected error for missing uri")
	}
	if !strings.Contains(err.Error(), "usage") {
		t.Errorf("expected usage error, got: %v", err)
	}
}

func TestCmdRead_InvalidMaxOutput(t *testing.T) {
	err := cmdRead([]string{"server", "uri", "--max-output", "abc"})
	if err == nil {
		t.Fatal("expected error for invalid max-output")
	}
	if !strings.Contains(err.Error(), "max-output") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListAllResources_NoSpuriousTruncationWarning(t *testing.T) {
	// Two pages that complete normally must NOT emit a truncation warning,
	// and resource Size must be propagated into the output.
	callCount := 0
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			callCount++
			var result resourcesListResult
			if callCount == 1 {
				result = resourcesListResult{
					Resources:  []mcpResource{{URI: "r1", Size: 42}},
					NextCursor: "p2",
				}
			} else {
				result = resourcesListResult{Resources: []mcpResource{{URI: "r2"}}}
			}
			data, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  data,
			}, nil
		},
	}

	var resources []resourceOutput
	var listErr error
	stderr := captureStderr(t, func() {
		resources, listErr = listAllResources(transport, "srv")
	})
	if listErr != nil {
		t.Fatal(listErr)
	}
	if strings.Contains(stderr, "truncated") {
		t.Errorf("unexpected truncation warning on normal completion: %q", stderr)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}
	if resources[0].Size != 42 {
		t.Errorf("expected Size 42 to be propagated, got %d", resources[0].Size)
	}
}

func TestValidateResourceURI(t *testing.T) {
	valid := []string{"notion://page/123", "https://example.com/a?b=c#d", "file:///tmp/x.txt"}
	for _, u := range valid {
		if err := validateResourceURI(u); err != nil {
			t.Errorf("valid uri %q rejected: %v", u, err)
		}
	}

	if err := validateResourceURI(""); err == nil {
		t.Error("empty uri should be rejected")
	}
	if err := validateResourceURI("file:///a\nb"); err == nil {
		t.Error("uri with control character should be rejected")
	}
	if err := validateResourceURI("x://" + strings.Repeat("a", 3000)); err == nil {
		t.Error("over-long uri should be rejected")
	}
}
