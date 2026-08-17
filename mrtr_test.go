package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Multi round-trip request (MRTR) and resultType envelope tests for the
// 2026-07-28 protocol revision.

func TestExecuteToolCall_ResultTypeComplete(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{
				"resultType": "complete",
				"content":    []contentBlock{{Type: "text", Text: "done"}},
			}), nil
		},
	}

	out, err := executeToolCall(transport, "echo", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "done" || out.IsError {
		t.Errorf("unexpected output: %+v", out)
	}
}

func TestExecuteToolCall_RequestStateRetry(t *testing.T) {
	var calls int
	var firstID, secondID int
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			calls++
			data, _ := json.Marshal(req.Params)
			var params toolCallParams
			_ = json.Unmarshal(data, &params)

			switch calls {
			case 1:
				firstID = req.ID
				if params.RequestState != "" {
					t.Errorf("initial request must not carry requestState, got %q", params.RequestState)
				}
				return rpcResult(t, req, map[string]any{
					"resultType":   "input_required",
					"requestState": "AEAD-protected blob",
				}), nil
			case 2:
				secondID = req.ID
				if params.RequestState != "AEAD-protected blob" {
					t.Errorf("retry requestState = %q, want the opaque state echoed verbatim", params.RequestState)
				}
				if params.Name != "echo" {
					t.Errorf("retry must repeat the original request, got tool %q", params.Name)
				}
				return rpcResult(t, req, map[string]any{
					"resultType": "complete",
					"content":    []contentBlock{{Type: "text", Text: "finished"}},
				}), nil
			}
			t.Fatal("too many calls")
			return jsonrpcResponse{}, nil
		},
	}

	out, err := executeToolCall(transport, "echo", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Content != "finished" {
		t.Errorf("unexpected output: %+v", out)
	}
	if calls != 2 {
		t.Errorf("expected 2 round trips, got %d", calls)
	}
	if firstID == secondID {
		t.Error("retry must use a fresh JSON-RPC request ID")
	}
}

func TestExecuteToolCall_InputRequestsUnsupported(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{
				"resultType": "input_required",
				"inputRequests": map[string]any{
					"github_login": map[string]any{
						"method": "elicitation/create",
						"params": map[string]any{"message": "Please provide your GitHub username"},
					},
				},
				"requestState": "blob",
			}), nil
		},
	}

	out, err := executeToolCall(transport, "login", nil, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsError {
		t.Fatal("expected an error output for unsupported interactive input")
	}
	if !strings.Contains(out.Content, "elicitation/create") {
		t.Errorf("output should name the requested method: %q", out.Content)
	}
	if !strings.Contains(out.Content, "github_login") {
		t.Errorf("output should name the request id: %q", out.Content)
	}
}

func TestExecuteToolCall_InputRequiredWithoutStateOrRequests(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{"resultType": "input_required"}), nil
		},
	}

	_, err := executeToolCall(transport, "echo", nil, false, nil)
	if err == nil {
		t.Fatal("expected error for input_required with neither inputRequests nor requestState")
	}
}

func TestExecuteToolCall_InputRequiredLoopBounded(t *testing.T) {
	var calls int
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			calls++
			return rpcResult(t, req, map[string]any{
				"resultType":   "input_required",
				"requestState": fmt.Sprintf("state-%d", calls),
			}), nil
		},
	}

	_, err := executeToolCall(transport, "echo", nil, false, nil)
	if err == nil {
		t.Fatal("expected error when the server never completes")
	}
	if calls != maxInputRequiredRetries {
		t.Errorf("expected %d attempts, got %d", maxInputRequiredRetries, calls)
	}
}

func TestExecuteToolCall_UnknownResultType(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{"resultType": "task_pending"}), nil
		},
	}

	_, err := executeToolCall(transport, "echo", nil, false, nil)
	if err == nil {
		t.Fatal("expected error for unrecognized resultType")
	}
	if !strings.Contains(err.Error(), "task_pending") {
		t.Errorf("error should name the result type: %v", err)
	}
}

func TestReadResource_RequestStateRetry(t *testing.T) {
	var calls int
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			calls++
			data, _ := json.Marshal(req.Params)
			var params resourceReadParams
			_ = json.Unmarshal(data, &params)
			if params.URI != "file:///a.txt" {
				t.Errorf("uri = %q", params.URI)
			}

			if calls == 1 {
				return rpcResult(t, req, map[string]any{
					"resultType":   "input_required",
					"requestState": "resource-state",
				}), nil
			}
			if params.RequestState != "resource-state" {
				t.Errorf("retry requestState = %q", params.RequestState)
			}
			return rpcResult(t, req, map[string]any{
				"resultType": "complete",
				"contents":   []resourceContents{{URI: params.URI, Text: "hello"}},
			}), nil
		},
	}

	out, err := readResource(transport, "file:///a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 || out.Contents[0].Text != "hello" {
		t.Errorf("unexpected output: %+v", out)
	}
	if calls != 2 {
		t.Errorf("expected 2 round trips, got %d", calls)
	}
}

func TestReadResource_InputRequestsUnsupported(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return rpcResult(t, req, map[string]any{
				"resultType": "input_required",
				"inputRequests": map[string]any{
					"consent": map[string]any{"method": "elicitation/create"},
				},
			}), nil
		},
	}

	_, err := readResource(transport, "file:///a.txt")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "elicitation/create") {
		t.Errorf("error should name the requested method: %v", err)
	}
}

func TestListAllTools_TTLMinAcrossPages(t *testing.T) {
	var calls int
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			calls++
			if calls == 1 {
				return rpcResult(t, req, map[string]any{
					"tools":      []mcpTool{{Name: "a"}},
					"nextCursor": "page2",
					"ttlMs":      60000,
				}), nil
			}
			return rpcResult(t, req, map[string]any{
				"tools": []mcpTool{{Name: "b"}},
				"ttlMs": 30000,
			}), nil
		},
	}

	tools, ttlMs, err := listAllTools(transport, "srv")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if ttlMs == nil || *ttlMs != 30000 {
		t.Errorf("ttlMs = %v, want the smallest hint 30000", ttlMs)
	}
}
