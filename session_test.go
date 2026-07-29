package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// rpcResult builds a JSON-RPC result response for a request.
func rpcResult(t *testing.T, req jsonrpcRequest, v any) jsonrpcResponse {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
		Result:  data,
	}
}

// rpcError builds a JSON-RPC error response for a request.
func rpcError(req jsonrpcRequest, code int, message string, data string) jsonrpcResponse {
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
		Error:   &jsonrpcError{Code: code, Message: message},
	}
	if data != "" {
		resp.Error.Data = json.RawMessage(data)
	}
	return resp
}

// paramsAsMap re-marshals request params into a generic map.
func paramsAsMap(t *testing.T, params any) map[string]any {
	t.Helper()
	if params == nil {
		return nil
	}
	data, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// requireModernMeta asserts the params carry the required 2026-07-28 _meta
// fields and returns the _meta map.
func requireModernMeta(t *testing.T, params any) map[string]any {
	t.Helper()
	m := paramsAsMap(t, params)
	if m == nil {
		t.Fatal("modern request has no params (must carry _meta)")
	}
	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("params missing _meta: %v", m)
	}
	if got := meta[metaProtocolVersion]; got != protocolVersionModern {
		t.Errorf("_meta protocolVersion = %v, want %s", got, protocolVersionModern)
	}
	if _, ok := meta[metaClientCapabilities]; !ok {
		t.Error("_meta missing clientCapabilities")
	}
	info, ok := meta[metaClientInfo].(map[string]any)
	if !ok || info["name"] != clientName {
		t.Errorf("_meta clientInfo = %v, want name %q", meta[metaClientInfo], clientName)
	}
	return meta
}

func TestProtocolSession_InjectsMetaWithoutParams(t *testing.T) {
	var seen any
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			seen = req.Params
			return rpcResult(t, req, map[string]any{}), nil
		},
	}
	session := newProtocolSession(mock, protocolVersionModern)

	if _, err := session.Send(jsonrpcRequest{JSONRPC: "2.0", ID: nextID(), Method: "tools/list"}); err != nil {
		t.Fatal(err)
	}
	requireModernMeta(t, seen)
}

func TestProtocolSession_InjectsMetaPreservingParams(t *testing.T) {
	var seen any
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			seen = req.Params
			return rpcResult(t, req, map[string]any{}), nil
		},
	}
	session := newProtocolSession(mock, protocolVersionModern)

	_, err := session.Send(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      nextID(),
		Method:  "tools/call",
		Params:  toolCallParams{Name: "echo", Arguments: map[string]any{"msg": "hi"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	requireModernMeta(t, seen)
	m := paramsAsMap(t, seen)
	if m["name"] != "echo" {
		t.Errorf("params name = %v, want echo", m["name"])
	}
	args, _ := m["arguments"].(map[string]any)
	if args["msg"] != "hi" {
		t.Errorf("params arguments = %v", m["arguments"])
	}
}

func TestProtocolSession_PreservesExistingMetaKeys(t *testing.T) {
	var seen any
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			seen = req.Params
			return rpcResult(t, req, map[string]any{}), nil
		},
	}
	session := newProtocolSession(mock, protocolVersionModern)

	_, err := session.Send(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      nextID(),
		Method:  "tools/call",
		Params: map[string]any{
			"name":  "echo",
			"_meta": map[string]any{"progressToken": "tok-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	meta := requireModernMeta(t, seen)
	if meta["progressToken"] != "tok-1" {
		t.Errorf("progressToken clobbered: %v", meta)
	}
}

// negotiationMock returns a transport whose behavior is driven per-method,
// recording the methods of all requests and notifications sent.
func negotiationMock(t *testing.T, handle func(req jsonrpcRequest) (jsonrpcResponse, error)) (*mockTransport, *[]string) {
	t.Helper()
	var methods []string
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			methods = append(methods, req.Method)
			return handle(req)
		},
		notifyFunc: func(notif jsonrpcNotification) error {
			methods = append(methods, notif.Method)
			return nil
		},
	}
	return mock, &methods
}

func TestNegotiateProtocol_ModernServer(t *testing.T) {
	mock, methods := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		if req.Method != "server/discover" {
			t.Errorf("unexpected method %q", req.Method)
		}
		requireModernMeta(t, req.Params)
		return rpcResult(t, req, map[string]any{
			"resultType":        "complete",
			"supportedVersions": []string{protocolVersionModern},
			"capabilities":      map[string]any{"tools": map[string]any{}},
		}), nil
	})

	got, err := negotiateProtocol(mock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*protocolSession); !ok {
		t.Fatalf("expected modern protocolSession, got %T", got)
	}
	if len(*methods) != 1 || (*methods)[0] != "server/discover" {
		t.Errorf("expected only server/discover, got %v", *methods)
	}
}

func TestNegotiateProtocol_LegacyFallbackOnMethodNotFound(t *testing.T) {
	mock, methods := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		switch req.Method {
		case "server/discover":
			return rpcError(req, codeMethodNotFound, "method not found", ""), nil
		case "initialize":
			var params initializeParams
			data, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(data, &params)
			if params.ProtocolVersion != protocolVersionLegacy {
				t.Errorf("initialize protocolVersion = %q, want %s", params.ProtocolVersion, protocolVersionLegacy)
			}
			return rpcResult(t, req, map[string]any{"protocolVersion": protocolVersionLegacy}), nil
		}
		t.Errorf("unexpected method %q", req.Method)
		return jsonrpcResponse{}, fmt.Errorf("unexpected method")
	})

	got, err := negotiateProtocol(mock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*protocolSession); ok {
		t.Fatal("expected bare legacy transport, got modern session")
	}
	want := []string{"server/discover", "initialize", "notifications/initialized"}
	if fmt.Sprint(*methods) != fmt.Sprint(want) {
		t.Errorf("methods = %v, want %v", *methods, want)
	}
}

func TestNegotiateProtocol_LegacyFallbackOnPreInitError(t *testing.T) {
	// Some legacy servers reject any pre-initialize request with their own
	// error rather than -32601.
	mock, _ := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		switch req.Method {
		case "server/discover":
			return rpcError(req, -32002, "Received request before initialization was complete", ""), nil
		case "initialize":
			return rpcResult(t, req, map[string]any{"protocolVersion": protocolVersionLegacy}), nil
		}
		return jsonrpcResponse{}, fmt.Errorf("unexpected method %q", req.Method)
	})

	got, err := negotiateProtocol(mock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*protocolSession); ok {
		t.Fatal("expected legacy transport")
	}
}

func TestNegotiateProtocol_UnsupportedVersionIsNotLegacy(t *testing.T) {
	// A recognized modern error must surface, not trigger initialize.
	mock, methods := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		return rpcError(req, codeUnsupportedProtocolVersion, "Unsupported protocol version",
			`{"supported":["2027-01-01"],"requested":"2026-07-28"}`), nil
	})

	_, err := negotiateProtocol(mock)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "2027-01-01") {
		t.Errorf("error should list the server's supported versions, got: %v", err)
	}
	for _, m := range *methods {
		if m == "initialize" {
			t.Error("must not fall back to initialize on a modern error")
		}
	}
}

func TestNegotiateProtocol_NoMutualVersionFallsBack(t *testing.T) {
	// A discover result without a version we speak → dual-era servers still
	// accept initialize.
	mock, methods := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		switch req.Method {
		case "server/discover":
			return rpcResult(t, req, map[string]any{"supportedVersions": []string{"2025-11-25"}}), nil
		case "initialize":
			return rpcResult(t, req, map[string]any{"protocolVersion": protocolVersionLegacy}), nil
		}
		return jsonrpcResponse{}, fmt.Errorf("unexpected method %q", req.Method)
	})

	got, err := negotiateProtocol(mock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*protocolSession); ok {
		t.Fatal("expected legacy transport")
	}
	if (*methods)[1] != "initialize" {
		t.Errorf("methods = %v", *methods)
	}
}

func TestNegotiateProtocol_StdioProbeErrorFallsBack(t *testing.T) {
	// On stdio a silent legacy server surfaces as a probe timeout; the
	// client must fall back rather than fail.
	mock, _ := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		switch req.Method {
		case "server/discover":
			return jsonrpcResponse{}, fmt.Errorf("stdio read timed out after 60s")
		case "initialize":
			return rpcResult(t, req, map[string]any{"protocolVersion": protocolVersionLegacy}), nil
		}
		return jsonrpcResponse{}, fmt.Errorf("unexpected method %q", req.Method)
	})

	got, err := negotiateProtocol(mock)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.(*protocolSession); ok {
		t.Fatal("expected legacy transport")
	}
}

func TestNegotiateProtocol_BrokenTransport(t *testing.T) {
	mock, _ := negotiationMock(t, func(req jsonrpcRequest) (jsonrpcResponse, error) {
		return jsonrpcResponse{}, fmt.Errorf("transport closed")
	})

	_, err := negotiateProtocol(mock)
	if err == nil {
		t.Fatal("expected error when probe and handshake both fail")
	}
	if !strings.Contains(err.Error(), "initialize") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMcpPing_ModernSessionUsesDiscover(t *testing.T) {
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			if req.Method != "server/discover" {
				t.Errorf("expected server/discover, got %q", req.Method)
			}
			return rpcResult(t, req, map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{protocolVersionModern},
			}), nil
		},
	}

	if err := mcpPing(newProtocolSession(mock, protocolVersionModern)); err != nil {
		t.Fatal(err)
	}
}
