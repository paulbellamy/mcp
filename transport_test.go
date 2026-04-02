package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPTransport_PlainJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
			Result:  json.RawMessage(`{"tools":[]}`),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	resp, err := transport.Send(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/list",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("expected result")
	}
}

func TestHTTPTransport_SSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// Send progress event
		_, _ = fmt.Fprintln(w, "data: {\"type\":\"progress\"}")
		_, _ = fmt.Fprintln(w)
		flusher.Flush()

		// Send response with matching ID
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
			Result:  json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`),
		}
		data, _ := json.Marshal(resp)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	var events []streamEvent
	resp, err := transport.SendStreaming(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      5,
		Method:  "tools/call",
	}, func(evt streamEvent) {
		events = append(events, evt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}
	if len(events) == 0 {
		t.Error("expected at least one progress event")
	}
}

func TestHTTPTransport_SSE_MultiLineData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req jsonrpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)

		// Send progress as a multi-line data event (two data: lines per SSE spec)
		_, _ = fmt.Fprintln(w, "data: line1")
		_, _ = fmt.Fprintln(w, "data: line2")
		_, _ = fmt.Fprintln(w) // blank line = event boundary
		flusher.Flush()

		// Send actual response (single data: line + blank line delimiter)
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
			Result:  json.RawMessage(`{"content":[{"type":"text","text":"done"}]}`),
		}
		data, _ := json.Marshal(resp)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	var events []streamEvent
	resp, err := transport.SendStreaming(jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      10,
		Method:  "tools/call",
	}, func(evt streamEvent) {
		events = append(events, evt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}
	// Multi-line data: lines should be concatenated with \n
	if len(events) == 0 {
		t.Fatal("expected at least one progress event")
	}
	if events[0].Data != "line1\nline2" {
		t.Errorf("expected multi-line data 'line1\\nline2', got %q", events[0].Data)
	}
}

func TestHTTPTransport_SessionIDCapture(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Mcp-Session-Id", "test-session-123")
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{
			JSONRPC: "2.0",
			ID:      json.RawMessage("1"),
			Result:  json.RawMessage("{}"),
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if transport.sessionID != "test-session-123" {
		t.Errorf("expected session ID 'test-session-123', got %q", transport.sessionID)
	}
}

func TestHTTPTransport_AuthHeader(t *testing.T) {
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage("{}")}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "my-secret-token")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if receivedAuth != "Bearer my-secret-token" {
		t.Errorf("expected auth header 'Bearer my-secret-token', got %q", receivedAuth)
	}
}

func TestHTTPTransport_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("server error"))
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error for 500 status")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to contain '500', got %q", err.Error())
	}
}

func TestHTTPTransport_Notification(t *testing.T) {
	var received bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	err := transport.Notify(jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		t.Fatal(err)
	}
	if !received {
		t.Error("server did not receive notification")
	}
}

func TestHTTPTransport_SessionIDSent(t *testing.T) {
	var receivedSessionID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedSessionID = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		resp := jsonrpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1"), Result: json.RawMessage("{}")}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	transport.sessionID = "existing-session"
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if receivedSessionID != "existing-session" {
		t.Errorf("expected session ID 'existing-session', got %q", receivedSessionID)
	}
}

// initForTest sets up internal state for test transports constructed without NewStdioTransport.
func (t *StdioTransport) initForTest() {
	t.pending = make(map[string]chan stdioResult)
	go t.readLoop()
}

// newTestStdioTransport creates a StdioTransport from pipes for testing.
func newTestStdioTransport(stdin io.WriteCloser, stdout io.Reader) *StdioTransport {
	t := &StdioTransport{
		stdin:  stdin,
		reader: bufio.NewReader(stdout),
	}
	t.initForTest()
	return t
}

func TestStdioTransport_SendReceive(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	// Mock server goroutine
	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var req jsonrpcRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"tools":[]}`),
			}
			data, _ := json.Marshal(resp)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))
		}
	}()

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}

	var id int
	_ = json.Unmarshal(resp.ID, &id)
	if id != 1 {
		t.Errorf("expected response ID 1, got %d", id)
	}
}

func TestStdioTransport_SkipNotifications(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	// Server sends a notification then the real response
	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var req jsonrpcRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)

			// Send notification (no id)
			notif := jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/progress"}
			nData, _ := json.Marshal(notif)
			_, _ = serverStdoutWriter.Write(append(nData, '\n'))

			// Send actual response
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"ok":true}`),
			}
			data, _ := json.Marshal(resp)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))
		}
	}()

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 42, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}

	var id int
	_ = json.Unmarshal(resp.ID, &id)
	if id != 42 {
		t.Errorf("expected response ID 42, got %d", id)
	}
}

func TestStdioTransport_SkipNonJSON(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var req jsonrpcRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)

			// Send non-JSON garbage
			_, _ = serverStdoutWriter.Write([]byte("some debug log output\n"))

			// Then the real response
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{}`),
			}
			data, _ := json.Marshal(resp)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))
		}
	}()

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 7, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}

	var id int
	_ = json.Unmarshal(resp.ID, &id)
	if id != 7 {
		t.Errorf("expected response ID 7, got %d", id)
	}
}

func TestStdioTransport_MismatchedID(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, serverStdoutWriter := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	go func() {
		defer func() { _ = serverStdoutWriter.Close() }()
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var req jsonrpcRequest
			_ = json.Unmarshal(scanner.Bytes(), &req)

			// Send a response with wrong ID first
			wrong := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage("999"),
				Result:  json.RawMessage(`{"wrong":true}`),
			}
			data, _ := json.Marshal(wrong)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))

			// Then send the correct response
			correct := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"correct":true}`),
			}
			data, _ = json.Marshal(correct)
			_, _ = serverStdoutWriter.Write(append(data, '\n'))
		}
	}()

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 42, Method: "test"})
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(resp.Result, []byte("correct")) {
		t.Errorf("expected correct response, got %s", string(resp.Result))
	}
}

func TestNewHTTPTransport_SetsClientTimeout(t *testing.T) {
	tr := NewHTTPTransport("http://example.com", "")
	if tr.client.Timeout != 10*time.Minute {
		t.Errorf("expected client timeout 10m, got %v", tr.client.Timeout)
	}
}

func TestHTTPTransport_ClientTimeoutFallback(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block until test completes
		<-done
	}))
	defer func() {
		close(done)
		srv.Close()
	}()

	transport := &HTTPTransport{
		url:    srv.URL,
		client: &http.Client{Timeout: 50 * time.Millisecond},
	}
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected timeout error")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "deadline exceeded") && !strings.Contains(errStr, "Timeout") {
		t.Errorf("expected deadline/timeout error, got %q", errStr)
	}
}

func TestHTTPTransport_ResponseBodyLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Write 2MB body — exceeds maxResponseBody (1MB)
		_, _ = w.Write(bytes.Repeat([]byte("x"), 2<<20))
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error for oversized response")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("expected 'exceeds' in error, got %q", err.Error())
	}
}

func TestHTTPTransport_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{not valid json`))
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("expected 'unmarshal' in error, got %q", err.Error())
	}
}

func TestHTTPTransport_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Empty body
	}))
	defer srv.Close()

	transport := NewHTTPTransport(srv.URL, "")
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error for empty body")
	}
}

func TestStdioTransport_Notify(t *testing.T) {
	serverStdinReader, clientStdinWriter := io.Pipe()
	clientStdoutReader, _ := io.Pipe()

	transport := newTestStdioTransport(clientStdinWriter, clientStdoutReader)

	// Read what the transport writes
	go func() {
		scanner := bufio.NewScanner(serverStdinReader)
		for scanner.Scan() {
			var notif jsonrpcNotification
			if err := json.Unmarshal(scanner.Bytes(), &notif); err != nil {
				t.Errorf("expected valid JSON notification: %v", err)
			}
			if notif.Method != "notifications/initialized" {
				t.Errorf("expected method 'notifications/initialized', got %q", notif.Method)
			}
		}
	}()

	err := transport.Notify(jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/initialized"})
	if err != nil {
		t.Fatal(err)
	}
}
