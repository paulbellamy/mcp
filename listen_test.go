package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// listenLine is a decoded `mcp listen` stdout line.
type listenLine struct {
	Type          string                    `json:"type"`
	Notifications subscriptionNotifications `json:"notifications"`
	Method        string                    `json:"method"`
	Params        json.RawMessage           `json:"params"`
}

func decodeListenLines(t *testing.T, stdout string) []listenLine {
	t.Helper()
	var lines []listenLine
	for _, raw := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if raw == "" {
			continue
		}
		var line listenLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("stdout line is not JSON: %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	return lines
}

// newListenSSEServer speaks the modern protocol and answers
// subscriptions/listen with an SSE stream: acknowledged, two notifications
// (with keep-alive comment lines interleaved), then a graceful-close
// response. It asserts the filter params and per-request _meta.
func newListenSSEServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			t.Error("client fell back to the removed HTTP GET stream")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		if req.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
			resp.Result, _ = json.Marshal(map[string]any{
				"resultType":        "complete",
				"supportedVersions": []string{protocolVersionModern},
			})
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		if req.Method != methodSubscriptionsListen {
			t.Errorf("unexpected method %q", req.Method)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		var params struct {
			Notifications subscriptionNotifications `json:"notifications"`
			Meta          map[string]any            `json:"_meta"`
		}
		data, _ := json.Marshal(req.Params)
		_ = json.Unmarshal(data, &params)
		want := subscriptionNotifications{ToolsListChanged: true, ResourceSubscriptions: []string{"file:///a.txt"}}
		if !reflect.DeepEqual(params.Notifications, want) {
			t.Errorf("listen params = %+v, want %+v", params.Notifications, want)
		}
		if params.Meta[metaProtocolVersion] != protocolVersionModern {
			t.Errorf("listen _meta protocolVersion = %v", params.Meta[metaProtocolVersion])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		meta := fmt.Sprintf(`{"io.modelcontextprotocol/subscriptionId":%d}`, req.ID)
		event := func(s string) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", s)
			flusher.Flush()
		}

		// Keep-alive comment lines must be ignored by the client.
		_, _ = fmt.Fprintln(w, ": keep-alive")
		event(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":%s,"notifications":{"toolsListChanged":true,"resourceSubscriptions":["file:///a.txt"]}}}`, meta))
		_, _ = fmt.Fprintln(w, ": keep-alive")
		flusher.Flush()
		event(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":%s}}`, meta))
		event(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"_meta":%s,"uri":"file:///a.txt"}}`, meta))
		// Graceful close: the final response with an empty result.
		event(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":{}}`, req.ID))
	}))
}

func TestListen_HTTPEndToEnd(t *testing.T) {
	setupTestConfigDir(t)
	srv := newListenSSEServer(t)
	defer srv.Close()

	var err error
	stdout := captureStdout(t, func() {
		err = cmdListen([]string{srv.URL, "--tools", "--resource", "file:///a.txt"})
	})
	if err != nil {
		t.Fatalf("cmdListen: %v", err)
	}

	lines := decodeListenLines(t, stdout)
	if len(lines) != 4 {
		t.Fatalf("expected 4 JSON lines, got %d: %q", len(lines), stdout)
	}
	if lines[0].Type != "acknowledged" {
		t.Errorf("line 0 type = %q, want acknowledged", lines[0].Type)
	}
	if !lines[0].Notifications.ToolsListChanged {
		t.Errorf("acknowledged notifications = %+v, want toolsListChanged", lines[0].Notifications)
	}
	if lines[1].Type != "notification" || lines[1].Method != "notifications/tools/list_changed" {
		t.Errorf("line 1 = %+v, want tools/list_changed notification", lines[1])
	}
	if lines[2].Type != "notification" || lines[2].Method != "notifications/resources/updated" {
		t.Errorf("line 2 = %+v, want resources/updated notification", lines[2])
	}
	if lines[3].Type != "closed" {
		t.Errorf("line 3 type = %q, want closed", lines[3].Type)
	}
}

func TestListen_AbruptCloseIsError(t *testing.T) {
	setupTestConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		if req.Method == "server/discover" {
			w.Header().Set("Content-Type", "application/json")
			resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
			resp.Result, _ = json.Marshal(map[string]any{"supportedVersions": []string{protocolVersionModern}})
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Stream the acknowledgment, then drop the connection without ever
		// sending the final response.
		w.Header().Set("Content-Type", "text/event-stream")
		meta := fmt.Sprintf(`{"io.modelcontextprotocol/subscriptionId":%d}`, req.ID)
		_, _ = fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/subscriptions/acknowledged\",\"params\":{\"_meta\":%s,\"notifications\":{\"toolsListChanged\":true}}}\n\n", meta)
	}))
	defer srv.Close()

	var err error
	captureStdout(t, func() {
		err = cmdListen([]string{srv.URL, "--tools"})
	})
	if err == nil {
		t.Fatal("expected error on abrupt stream close")
	}
	if !strings.Contains(err.Error(), "closed unexpectedly") {
		t.Errorf("expected abrupt-close error, got %v", err)
	}
}

func TestParseListenArgs(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantServer string
		wantParams subscriptionsListenParams
		wantErr    string
	}{
		{
			name:       "tools only",
			args:       []string{"srv", "--tools"},
			wantServer: "srv",
			wantParams: subscriptionsListenParams{Notifications: subscriptionNotifications{ToolsListChanged: true}},
		},
		{
			name:       "all filters",
			args:       []string{"srv", "--tools", "--prompts", "--resources", "--resource", "file:///a.txt", "--resource", "file:///b.txt", "--json"},
			wantServer: "srv",
			wantParams: subscriptionsListenParams{Notifications: subscriptionNotifications{
				ToolsListChanged:      true,
				PromptsListChanged:    true,
				ResourcesListChanged:  true,
				ResourceSubscriptions: []string{"file:///a.txt", "file:///b.txt"},
			}},
		},
		{
			name:       "resource only",
			args:       []string{"--resource", "file:///a.txt", "srv"},
			wantServer: "srv",
			wantParams: subscriptionsListenParams{Notifications: subscriptionNotifications{ResourceSubscriptions: []string{"file:///a.txt"}}},
		},
		{
			name:    "no filters",
			args:    []string{"srv"},
			wantErr: "at least one of",
		},
		{
			name:    "no server",
			args:    []string{"--tools"},
			wantErr: "usage: mcp listen",
		},
		{
			name:    "unknown flag",
			args:    []string{"srv", "--bogus"},
			wantErr: "unknown flag",
		},
		{
			name:    "resource missing value",
			args:    []string{"srv", "--resource"},
			wantErr: "--resource requires a value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, params, help, err := parseListenArgs(tc.args)
			if help {
				t.Fatal("unexpected help request")
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if server != tc.wantServer {
				t.Errorf("server = %q, want %q", server, tc.wantServer)
			}
			if !reflect.DeepEqual(params, tc.wantParams) {
				t.Errorf("params = %+v, want %+v", params, tc.wantParams)
			}
		})
	}
}

func TestListen_LegacyServerFailsFast(t *testing.T) {
	setupTestConfigDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			t.Error("client fell back to the removed HTTP GET stream")
			w.WriteHeader(http.StatusMethodNotAllowed)
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

		if req.Method != "initialize" {
			// Legacy servers reject unknown pre-initialize requests.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("Bad Request: No valid session ID provided"))
			return
		}
		w.Header().Set("Mcp-Session-Id", "sess-1")
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		resp.Result, _ = json.Marshal(map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}})
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	err := cmdListen([]string{srv.URL, "--tools"})
	if err == nil {
		t.Fatal("expected error against a legacy server")
	}
	if !strings.Contains(err.Error(), "does not support subscriptions/listen (pre-2026 protocol)") {
		t.Errorf("expected actionable pre-2026 error, got %v", err)
	}
}

func TestListen_MethodNotFoundGetsSameMessage(t *testing.T) {
	setupTestConfigDir(t)
	// A server that negotiates as modern but answers subscriptions/listen
	// with -32601 gets the same actionable message.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req jsonrpcRequest
		_ = json.Unmarshal(body, &req)

		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID))}
		if req.Method == "server/discover" {
			resp.Result, _ = json.Marshal(map[string]any{"supportedVersions": []string{protocolVersionModern}})
		} else {
			resp.Error = &jsonrpcError{Code: codeMethodNotFound, Message: "method not found"}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	err := cmdListen([]string{srv.URL, "--tools"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "does not support subscriptions/listen (pre-2026 protocol)") {
		t.Errorf("expected actionable pre-2026 error, got %v", err)
	}
}

func TestRunListen_LegacyTransportRejected(t *testing.T) {
	sent := false
	mock := &mockTransport{
		streamFunc: func(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
			sent = true
			return jsonrpcResponse{}, nil
		},
	}
	err := runListen(mock, subscriptionsListenParams{Notifications: subscriptionNotifications{ToolsListChanged: true}})
	if err == nil || !strings.Contains(err.Error(), "pre-2026 protocol") {
		t.Fatalf("expected pre-2026 error, got %v", err)
	}
	if sent {
		t.Error("listen request was sent over a legacy transport")
	}
}

func TestRunListen_StdioCorrelation(t *testing.T) {
	var gotParams struct {
		Notifications subscriptionNotifications `json:"notifications"`
		Meta          map[string]any            `json:"_meta"`
	}
	mock := &mockTransport{
		streamFunc: func(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
			if req.Method != methodSubscriptionsListen {
				t.Errorf("method = %q, want %s", req.Method, methodSubscriptionsListen)
			}
			data, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(data, &gotParams)

			meta := fmt.Sprintf(`{"io.modelcontextprotocol/subscriptionId":%d}`, req.ID)
			onEvent(streamEvent{Type: "progress", Data: fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":%s,"notifications":{"resourcesListChanged":true}}}`, meta)})
			onEvent(streamEvent{Type: "progress", Data: fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/resources/list_changed","params":{"_meta":%s}}`, meta)})
			// A notification for someone else's subscription must be skipped.
			onEvent(streamEvent{Type: "progress", Data: `{"jsonrpc":"2.0","method":"notifications/resources/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":999999}}}`})
			// Non-notification noise must be skipped, not crash.
			onEvent(streamEvent{Type: "progress", Data: `not json`})
			return jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", req.ID)), Result: json.RawMessage(`{}`)}, nil
		},
	}
	mock.lastTimeout = 5 * time.Second
	transport := newProtocolSession(mock, protocolVersionModern)

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			err = runListen(transport, subscriptionsListenParams{Notifications: subscriptionNotifications{ResourcesListChanged: true}})
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	if !gotParams.Notifications.ResourcesListChanged {
		t.Errorf("listen params = %+v, want resourcesListChanged", gotParams.Notifications)
	}
	if gotParams.Meta[metaProtocolVersion] != protocolVersionModern {
		t.Errorf("listen _meta protocolVersion = %v", gotParams.Meta[metaProtocolVersion])
	}
	if mock.lastTimeout != 0 {
		t.Errorf("listen call timeout = %v, want 0 (no timeout)", mock.lastTimeout)
	}

	lines := decodeListenLines(t, stdout)
	if len(lines) != 3 {
		t.Fatalf("expected 3 JSON lines, got %d: %q", len(lines), stdout)
	}
	if lines[0].Type != "acknowledged" || !lines[0].Notifications.ResourcesListChanged {
		t.Errorf("line 0 = %+v, want acknowledged with resourcesListChanged", lines[0])
	}
	if lines[1].Type != "notification" || lines[1].Method != "notifications/resources/list_changed" {
		t.Errorf("line 1 = %+v, want resources/list_changed notification", lines[1])
	}
	if lines[2].Type != "closed" {
		t.Errorf("line 2 type = %q, want closed", lines[2].Type)
	}

	if !strings.Contains(stderr, "different subscription") {
		t.Errorf("expected unrelated-subscription warning on stderr, got %q", stderr)
	}
}

func TestStdioTransport_StreamingNotificationSink(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var req jsonrpcRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)

			// Two notifications, then the final response.
			_, _ = fmt.Fprintln(serverStdoutWriter, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}`)
			_, _ = fmt.Fprintln(serverStdoutWriter, `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///a.txt"}}`)
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{}`),
			}
			data, _ := json.Marshal(resp)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))
		}
	}()

	var events []streamEvent
	resp, err := transport.SendStreaming(jsonrpcRequest{JSONRPC: "2.0", ID: 3, Method: methodSubscriptionsListen}, func(evt streamEvent) {
		events = append(events, evt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 notification events, got %d: %+v", len(events), events)
	}
	if events[0].Type != "progress" || !strings.Contains(events[0].Data, "notifications/tools/list_changed") {
		t.Errorf("event 0 = %+v", events[0])
	}
	if !strings.Contains(events[1].Data, "notifications/resources/updated") {
		t.Errorf("event 1 = %+v", events[1])
	}

	// The sink must be unregistered once the final response arrived.
	transport.mu.Lock()
	registered := transport.notifyHandler != nil
	transport.mu.Unlock()
	if registered {
		t.Error("notification sink still registered after SendStreaming returned")
	}
}
