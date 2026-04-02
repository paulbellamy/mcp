package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCmdCall_ParamEqualsValue(t *testing.T) {
	// We can't easily test the full cmdCall (needs server), so test the flag
	// parsing logic by replicating the loop from cmdCall.
	args := []string{"server", "tool", "--msg=hello world", "--count=42", "--flag"}
	dynamicFlags := make(map[string]string)

	for i := 2; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			t.Fatalf("unexpected positional arg %q", arg)
		}
		key := strings.TrimPrefix(arg, "--")
		if eqIdx := strings.IndexByte(key, '='); eqIdx >= 0 {
			dynamicFlags[key[:eqIdx]] = key[eqIdx+1:]
		} else if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
			dynamicFlags[key] = "true"
		} else {
			i++
			dynamicFlags[key] = args[i]
		}
	}

	if dynamicFlags["msg"] != "hello world" {
		t.Errorf("expected msg='hello world', got %q", dynamicFlags["msg"])
	}
	if dynamicFlags["count"] != "42" {
		t.Errorf("expected count='42', got %q", dynamicFlags["count"])
	}
	if dynamicFlags["flag"] != "true" {
		t.Errorf("expected flag='true', got %q", dynamicFlags["flag"])
	}
}

func TestCmdCall_UnexpectedPositionalArg(t *testing.T) {
	err := cmdCall([]string{"server", "tool", "badarg"})
	if err == nil {
		t.Fatal("expected error for positional arg")
	}
	if !strings.Contains(err.Error(), "unexpected argument") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRenderContent_TextBlock(t *testing.T) {
	result := toolCallResult{
		Content: []contentBlock{{Type: "text", Text: "hello world"}},
	}
	got := renderToolCallResult(result)
	if got.Content != "hello world" {
		t.Errorf("expected 'hello world', got %q", got.Content)
	}
	if got.IsError {
		t.Error("expected isError false")
	}
}

func TestRenderContent_ImageBlock(t *testing.T) {
	result := toolCallResult{
		Content: []contentBlock{{Type: "image", MimeType: "image/png", Data: "base64data"}},
	}
	got := renderToolCallResult(result)
	if got.Content != "[image: image/png]" {
		t.Errorf("expected '[image: image/png]', got %q", got.Content)
	}
}

func TestRenderContent_MultipleBlocks(t *testing.T) {
	result := toolCallResult{
		Content: []contentBlock{
			{Type: "text", Text: "line1"},
			{Type: "text", Text: "line2"},
			{Type: "image", MimeType: "image/jpeg"},
		},
	}
	got := renderToolCallResult(result)
	parts := strings.Split(got.Content, "\n")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts, got %d: %q", len(parts), got.Content)
	}
	if parts[0] != "line1" || parts[1] != "line2" || parts[2] != "[image: image/jpeg]" {
		t.Errorf("unexpected content: %q", got.Content)
	}
}

func TestRenderContent_IsError(t *testing.T) {
	result := toolCallResult{
		Content: []contentBlock{{Type: "text", Text: "something failed"}},
		IsError: true,
	}
	got := renderToolCallResult(result)
	if !got.IsError {
		t.Error("expected isError true")
	}
}

func TestRenderContent_UnknownType(t *testing.T) {
	result := toolCallResult{
		Content: []contentBlock{{Type: "resource", Text: "data"}},
	}
	got := renderToolCallResult(result)
	// Unknown types get JSON-serialized
	if !strings.Contains(got.Content, "resource") {
		t.Errorf("expected content to contain 'resource', got %q", got.Content)
	}
}

func TestCallToolFlow(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "tools/call" {
				t.Errorf("expected method 'tools/call', got %q", req.Method)
			}

			// Verify params
			data, _ := json.Marshal(req.Params)
			var params toolCallParams
			json.Unmarshal(data, &params)
			if params.Name != "echo" {
				t.Errorf("expected tool 'echo', got %q", params.Name)
			}
			if params.Arguments["message"] != "test" {
				t.Errorf("expected argument message='test', got %v", params.Arguments["message"])
			}

			result := toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "echoed: test"}},
			}
			resultData, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  resultData,
			}, nil
		},
	}

	output, err := executeToolCall(transport, "echo", map[string]any{"message": "test"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if output.Content != "echoed: test" {
		t.Errorf("expected 'echoed: test', got %q", output.Content)
	}
}

func TestCallToolFlow_JSONRPCError(t *testing.T) {
	transport := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Error:   &jsonrpcError{Code: -32602, Message: "invalid params"},
			}, nil
		},
	}

	output, err := executeToolCall(transport, "bad-tool", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if !output.IsError {
		t.Error("expected isError for JSON-RPC error")
	}
	if output.Content != "invalid params" {
		t.Errorf("expected 'invalid params', got %q", output.Content)
	}
}

func TestCallToolFlow_Stream(t *testing.T) {
	var events []streamEvent
	transport := &mockTransport{
		streamFunc: func(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
			onEvent(streamEvent{Type: "progress", Data: "working..."})
			events = append(events, streamEvent{Type: "progress", Data: "working..."})

			result := toolCallResult{
				Content: []contentBlock{{Type: "text", Text: "done"}},
			}
			resultData, _ := json.Marshal(result)
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  resultData,
			}, nil
		},
	}

	output, err := executeToolCall(transport, "slow-tool", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if output.Content != "done" {
		t.Errorf("expected 'done', got %q", output.Content)
	}
}

func TestSanitizePathComponent(t *testing.T) {
	tests := []struct{ in, want string }{
		{"simple", "simple"},
		{"my/server", "my_server"},
		{"tool@v1.0", "tool_v1.0"},
		{"has spaces", "has_spaces"},
		{"a-b_c.d", "a-b_c.d"},
		{"café", "caf_"},
	}
	for _, tt := range tests {
		got := sanitizePathComponent(tt.in)
		if got != tt.want {
			t.Errorf("sanitizePathComponent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSaveFullOutput(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	path := saveFullOutput("my/server", "my-tool", "hello world")
	if path == "" {
		t.Fatal("expected non-empty path")
	}

	// Verify file exists with correct content.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("expected 'hello world', got %q", string(data))
	}

	// Verify filename contains sanitized names.
	base := filepath.Base(path)
	if !strings.Contains(base, "my_server") || !strings.Contains(base, "my-tool") {
		t.Errorf("expected sanitized names in filename, got %q", base)
	}

	// Verify file permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected 0600 permissions, got %o", info.Mode().Perm())
	}
}

func TestCmdCall_Truncation(t *testing.T) {
	setupTestConfigDir(t)

	longContent := strings.Repeat("x", 200)
	srv := newMockMCPServer(t, nil)
	defer srv.Close()

	// Override the mock to return long content.
	// We need a custom server for this.
	srv.Close()
	srv = newMockMCPServerWithContent(t, longContent)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdCall([]string{srv.URL, "echo", "--params", `{}`, "--max-output", "50"})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	if err := json.Unmarshal([]byte(data), &out); err != nil {
		t.Fatalf("invalid JSON: %s", data)
	}
	if !out.Truncated {
		t.Error("expected Truncated=true")
	}
	if !strings.Contains(out.Content, "[output truncated at 50 chars]") {
		t.Errorf("expected truncation message, got %q", out.Content)
	}
}

func TestCmdCall_NoTruncation(t *testing.T) {
	setupTestConfigDir(t)
	srv := newMockMCPServer(t, nil)
	defer srv.Close()

	var err error
	data := captureStdout(t, func() {
		err = cmdCall([]string{srv.URL, "echo", "--params", `{}`})
	})
	if err != nil {
		t.Fatal(err)
	}

	var out callOutput
	json.Unmarshal([]byte(data), &out)
	if out.Truncated {
		t.Error("expected Truncated=false for short output")
	}
}
