package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestListAllTools_SinglePage(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			result := toolsListResult{
				Tools: []mcpTool{
					{Name: "tool1", Description: "first tool"},
					{Name: "tool2", Description: "second tool"},
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

	tools, err := listAllTools(transport, "test-server")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "tool1" || tools[1].Name != "tool2" {
		t.Errorf("unexpected tools: %+v", tools)
	}
	if tools[0].Server != "test-server" {
		t.Errorf("expected server 'test-server', got %q", tools[0].Server)
	}
}

func TestListAllTools_Pagination(t *testing.T) {
	callCount := 0
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			callCount++
			var result toolsListResult

			switch callCount {
			case 1:
				result = toolsListResult{
					Tools:      []mcpTool{{Name: "tool1"}},
					NextCursor: "cursor-page2",
				}
			case 2:
				// Verify cursor was sent
				if req.Params != nil {
					params, _ := json.Marshal(req.Params)
					var p toolsListParams
					_ = json.Unmarshal(params, &p)
					if p.Cursor != "cursor-page2" {
						t.Errorf("expected cursor 'cursor-page2', got %q", p.Cursor)
					}
				}
				result = toolsListResult{
					Tools:      []mcpTool{{Name: "tool2"}, {Name: "tool3"}},
					NextCursor: "cursor-page3",
				}
			case 3:
				result = toolsListResult{
					Tools: []mcpTool{{Name: "tool4"}},
					// No NextCursor — last page
				}
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

	tools, err := listAllTools(transport, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Fatalf("expected 4 tools across 3 pages, got %d", len(tools))
	}
	if callCount != 3 {
		t.Errorf("expected 3 requests, got %d", callCount)
	}
	names := []string{tools[0].Name, tools[1].Name, tools[2].Name, tools[3].Name}
	expected := []string{"tool1", "tool2", "tool3", "tool4"}
	for i, n := range names {
		if n != expected[i] {
			t.Errorf("tool %d: expected %q, got %q", i, expected[i], n)
		}
	}
}

func TestListAllTools_ErrorResponse(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: -32600, Message: "bad request"},
			}, nil
		},
	}

	_, err := listAllTools(transport, "srv")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "list tools: bad request" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestListAllTools_TransportError(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{}, fmt.Errorf("connection refused")
		},
	}

	_, err := listAllTools(transport, "srv")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestMcpPing_Success(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "ping" {
				t.Errorf("expected method 'ping', got %q", req.Method)
			}
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage("{}"),
			}, nil
		},
	}

	if err := mcpPing(transport); err != nil {
		t.Fatal(err)
	}
}

func TestMcpPing_Error(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: -32601, Message: "method not found"},
			}, nil
		},
	}

	err := mcpPing(transport)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "ping: method not found" {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMcpPing_TransportError(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{}, fmt.Errorf("timeout")
		},
	}

	err := mcpPing(transport)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPrintToolsHuman_SingleServer(t *testing.T) {
	tools := []toolOutput{
		{Server: "my-server", Name: "search", Description: "Search the knowledge base"},
		{Server: "my-server", Name: "get-doc", Description: "Get a document"},
	}

	var err error
	output := captureStdout(t, func() {
		err = printToolsHuman(tools)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output, "my-server (2 tools)") {
		t.Errorf("expected server header, got:\n%s", output)
	}
	if !strings.Contains(output, "search") || !strings.Contains(output, "get-doc") {
		t.Errorf("expected tool names, got:\n%s", output)
	}
	if !strings.Contains(output, "Search the knowledge base") {
		t.Errorf("expected descriptions, got:\n%s", output)
	}
}

func TestPrintToolsHuman_MultiServer(t *testing.T) {
	tools := []toolOutput{
		{Server: "alpha", Name: "tool1", Description: "First"},
		{Server: "beta", Name: "tool2", Description: "Second"},
	}

	var err error
	output := captureStdout(t, func() {
		err = printToolsHuman(tools)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output, "alpha (1 tool)") {
		t.Errorf("expected 'alpha (1 tool)', got:\n%s", output)
	}
	if !strings.Contains(output, "beta (1 tool)") {
		t.Errorf("expected 'beta (1 tool)', got:\n%s", output)
	}
}

func TestPrintToolsHuman_Empty(t *testing.T) {
	// Empty list prints to stderr, not stdout.
	var err error
	output := captureStderr(t, func() {
		err = printToolsHuman(nil)
	})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(output, "No tools found") {
		t.Errorf("expected 'No tools found', got %q", output)
	}
}

func TestOutputToolsList_CompactByDefault(t *testing.T) {
	tools := []toolOutput{
		{
			Server:      "srv",
			Name:        "search",
			Description: "Search items",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		},
	}

	out := captureStdout(t, func() {
		if err := outputToolsList(tools, "", true, false); err != nil {
			t.Fatal(err)
		}
	})

	var got []toolOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(got))
	}
	if got[0].Name != "search" {
		t.Errorf("expected name 'search', got %q", got[0].Name)
	}
	if len(got[0].InputSchema) != 0 {
		t.Errorf("expected inputSchema to be omitted, got %s", string(got[0].InputSchema))
	}

	// And it shouldn't appear in the raw JSON either.
	if strings.Contains(out, "inputSchema") {
		t.Errorf("compact JSON should not contain 'inputSchema':\n%s", out)
	}
}

func TestOutputToolsList_FullIncludesSchema(t *testing.T) {
	tools := []toolOutput{
		{
			Server:      "srv",
			Name:        "search",
			Description: "Search items",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		},
	}

	out := captureStdout(t, func() {
		if err := outputToolsList(tools, "", true, true); err != nil {
			t.Fatal(err)
		}
	})

	var got []toolOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %s", out)
	}
	if len(got) != 1 || len(got[0].InputSchema) == 0 {
		t.Errorf("expected inputSchema to be present in --full output:\n%s", out)
	}
}
