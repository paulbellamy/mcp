package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// maxResponseBody is the maximum HTTP response body size (1 MB).
// This protects against memory exhaustion from oversized server responses.
const maxResponseBody = 1 << 20

func readResponseBody(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxResponseBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxResponseBody)
	}
	return data, nil
}

// Transport abstracts stdio and HTTP transports for MCP JSON-RPC.
type Transport interface {
	// Send sends a JSON-RPC request and returns the response.
	Send(req jsonrpcRequest) (jsonrpcResponse, error)
	// SendStreaming sends a request and calls onEvent for each SSE event.
	// The final response is returned.
	SendStreaming(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error)
	// Notify sends a JSON-RPC notification (no response expected).
	Notify(notif jsonrpcNotification) error
	// Close shuts down the transport.
	Close() error
	// SetTimeout sets the per-call timeout for subsequent Send and
	// SendStreaming requests. A value of 0 means no timeout.
	SetTimeout(d time.Duration)
}

// Default per-call timeouts. Send is used for short request/response work
// (initialize, tool calls that return promptly). Streaming may legitimately
// hold the connection open for progress events, so its default is longer.
const (
	defaultHTTPSendTimeout   = 2 * time.Minute
	defaultHTTPStreamTimeout = 5 * time.Minute
	defaultStdioCallTimeout  = 60 * time.Second
	defaultDaemonCallTimeout = 120 * time.Second
)

// contextWithOptionalTimeout returns a context with the given timeout, or a
// plain cancellable context if d <= 0 (meaning no timeout).
func contextWithOptionalTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(context.Background())
	}
	return context.WithTimeout(context.Background(), d)
}

// stdioResult is the result delivered from the reader goroutine to a waiting Send call.
type stdioResult struct {
	resp jsonrpcResponse
	err  error
}

// StdioTransport communicates with an MCP server via stdin/stdout of a child process.
// A single persistent reader goroutine dispatches responses to waiting callers by request ID.
type StdioTransport struct {
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	reader      *bufio.Reader
	mu          sync.Mutex // protects stdin writes, pending map, and closed flag
	pending     map[string]chan stdioResult
	closed      bool
	callTimeout time.Duration // 0 = no timeout
}

func NewStdioTransport(command string, args []string) (*StdioTransport, error) {
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start command %q: %w", command, err)
	}

	t := &StdioTransport{
		cmd:         cmd,
		stdin:       stdin,
		reader:      bufio.NewReader(stdout),
		pending:     make(map[string]chan stdioResult),
		callTimeout: defaultStdioCallTimeout,
	}

	go t.readLoop()

	return t, nil
}

// readLoop is the single persistent goroutine that reads stdout and dispatches
// responses to waiting callers by matching request IDs.
func (t *StdioTransport) readLoop() {
	for {
		line, err := t.reader.ReadBytes('\n')
		if err != nil {
			// Reader closed — deliver error to all pending requests
			t.mu.Lock()
			t.closed = true
			for id, ch := range t.pending {
				ch <- stdioResult{err: fmt.Errorf("read from stdout: %w", err)}
				delete(t.pending, id)
			}
			t.mu.Unlock()
			return
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Could be debug output, skip
			logStderr("transport: skipping non-JSON line")
			continue
		}

		// Notification — no id, skip
		if resp.ID == nil {
			continue
		}

		key := string(resp.ID)
		t.mu.Lock()
		ch, ok := t.pending[key]
		if ok {
			delete(t.pending, key)
		}
		t.mu.Unlock()

		if ok {
			ch <- stdioResult{resp: resp}
		} else {
			logStderr("transport: skipping response with unrecognized ID %s", key)
		}
	}
}

func (t *StdioTransport) Send(req jsonrpcRequest) (jsonrpcResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return jsonrpcResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	reqIDRaw, _ := json.Marshal(req.ID)
	key := string(reqIDRaw)
	ch := make(chan stdioResult, 1)

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return jsonrpcResponse{}, fmt.Errorf("transport closed")
	}
	t.pending[key] = ch
	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		delete(t.pending, key)
		t.mu.Unlock()
		return jsonrpcResponse{}, fmt.Errorf("write to stdin: %w", err)
	}
	t.mu.Unlock()

	if t.callTimeout <= 0 {
		// No timeout — wait indefinitely.
		r := <-ch
		return r.resp, r.err
	}

	timer := time.NewTimer(t.callTimeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		return r.resp, r.err
	case <-timer.C:
		t.mu.Lock()
		delete(t.pending, key)
		t.mu.Unlock()
		return jsonrpcResponse{}, fmt.Errorf("stdio read timed out after %s", t.callTimeout)
	}
}

// SetTimeout updates the per-call timeout for subsequent Send calls.
// A value of 0 means no timeout.
func (t *StdioTransport) SetTimeout(d time.Duration) {
	t.mu.Lock()
	t.callTimeout = d
	t.mu.Unlock()
}

func (t *StdioTransport) SendStreaming(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	// Stdio doesn't have SSE — just do a regular send
	return t.Send(req)
}

func (t *StdioTransport) Notify(notif jsonrpcNotification) error {
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	_, err = t.stdin.Write(append(data, '\n'))
	return err
}

func (t *StdioTransport) Close() error {
	_ = t.stdin.Close()

	// Wait up to 2s for process to exit gracefully
	exited := make(chan error, 1)
	go func() { exited <- t.cmd.Wait() }()

	select {
	case err := <-exited:
		return err
	case <-time.After(2 * time.Second):
		return t.cmd.Process.Kill()
	}
}

// HTTPTransport communicates with an MCP server via streamable HTTP.
type HTTPTransport struct {
	url            string
	authToken      string
	client         *http.Client
	sessionID      string
	requestTimeout time.Duration // for Send; 0 = no timeout
	streamTimeout  time.Duration // for SendStreaming; 0 = no timeout
}

func NewHTTPTransport(url string, authToken string) *HTTPTransport {
	return &HTTPTransport{
		url:            url,
		authToken:      authToken,
		client:         &http.Client{}, // per-request contexts drive timeouts
		requestTimeout: defaultHTTPSendTimeout,
		streamTimeout:  defaultHTTPStreamTimeout,
	}
}

// SetTimeout updates the per-call timeout used for both Send and SendStreaming.
// A value of 0 means no timeout.
func (t *HTTPTransport) SetTimeout(d time.Duration) {
	t.requestTimeout = d
	t.streamTimeout = d
}

func (t *HTTPTransport) Send(req jsonrpcRequest) (jsonrpcResponse, error) {
	ctx, cancel := contextWithOptionalTimeout(t.requestTimeout)
	defer cancel()
	return t.sendWithContext(ctx, req, nil)
}

func (t *HTTPTransport) SendStreaming(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	ctx, cancel := contextWithOptionalTimeout(t.streamTimeout)
	defer cancel()
	return t.sendWithContext(ctx, req, onEvent)
}

func (t *HTTPTransport) sendWithContext(ctx context.Context, req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return jsonrpcResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(data))
	if err != nil {
		return jsonrpcResponse{}, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if t.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+t.authToken)
	}
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}

	httpResp, err := t.client.Do(httpReq)
	if err != nil {
		return jsonrpcResponse{}, annotateTimeout(ctx, fmt.Errorf("http request: %w", err))
	}
	defer func() { _ = httpResp.Body.Close() }()

	if httpResp.StatusCode != http.StatusOK {
		body, _ := readResponseBody(httpResp.Body)
		return jsonrpcResponse{}, fmt.Errorf("http %d: %s", httpResp.StatusCode, string(body))
	}

	// Save session ID if provided
	if sid := httpResp.Header.Get("Mcp-Session-Id"); sid != "" {
		t.sessionID = sid
	}

	contentType := httpResp.Header.Get("Content-Type")

	if strings.HasPrefix(contentType, "text/event-stream") {
		reqIDRaw, _ := json.Marshal(req.ID)
		return t.readSSE(ctx, httpResp.Body, reqIDRaw, onEvent)
	}

	// Plain JSON response
	body, err := readResponseBody(httpResp.Body)
	if err != nil {
		return jsonrpcResponse{}, annotateTimeout(ctx, fmt.Errorf("read response body: %w", err))
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return jsonrpcResponse{}, fmt.Errorf("unmarshal response: %w", err)
	}

	return resp, nil
}

// annotateTimeout rewrites context-deadline errors into a clearer message
// pointing the user at --timeout. Other errors pass through unchanged.
func annotateTimeout(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("tool call timed out: server did not respond. Pass --timeout <duration> (e.g. --timeout 5m) to wait longer, or --timeout 0 to wait indefinitely")
}

func (t *HTTPTransport) readSSE(ctx context.Context, body io.Reader, reqIDRaw json.RawMessage, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var result jsonrpcResponse
	found := false

	// Per SSE spec: accumulate data: lines, dispatch on blank line
	var dataBuf strings.Builder

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "data: ") {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
			continue
		}

		// Non-empty, non-data line (e.g. "event:", "id:", "retry:") — skip
		if line != "" {
			continue
		}

		// Blank line = event boundary — process accumulated data
		if dataBuf.Len() == 0 {
			continue
		}

		data := dataBuf.String()
		dataBuf.Reset()

		// Once we have our response, stop reading
		if found {
			break
		}

		// Try to parse as JSON-RPC response
		var resp jsonrpcResponse
		if err := json.Unmarshal([]byte(data), &resp); err != nil {
			// Not a JSON-RPC message — treat as progress
			if onEvent != nil {
				onEvent(streamEvent{Type: "progress", Data: data})
			}
			continue
		}

		// Check if this is a response matching our request ID
		if resp.ID != nil && bytes.Equal(resp.ID, reqIDRaw) {
			result = resp
			found = true
			continue
		}

		// Could be a notification or progress
		if onEvent != nil {
			onEvent(streamEvent{Type: "progress", Data: data})
		}
	}

	if err := scanner.Err(); err != nil {
		return jsonrpcResponse{}, annotateTimeout(ctx, fmt.Errorf("read SSE stream: %w", err))
	}

	if !found {
		// Server closed the stream without ever sending our response. If the
		// context deadline fired, prefer that explanation.
		if ctxErr := ctx.Err(); errors.Is(ctxErr, context.DeadlineExceeded) {
			return jsonrpcResponse{}, annotateTimeout(ctx, ctxErr)
		}
		return jsonrpcResponse{}, fmt.Errorf("no response found in SSE stream for request ID %s", string(reqIDRaw))
	}

	return result, nil
}

func (t *HTTPTransport) Notify(notif jsonrpcNotification) error {
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.url, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if t.authToken != "" {
		httpReq.Header.Set("Authorization", "Bearer "+t.authToken)
	}
	if t.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", t.sessionID)
	}

	resp, err := t.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("http request: %w", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification failed: HTTP %d", resp.StatusCode)
	}

	return nil
}

func (t *HTTPTransport) Close() error {
	// Best-effort session termination per MCP spec
	if t.sessionID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "DELETE", t.url, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", t.sessionID)
			if t.authToken != "" {
				req.Header.Set("Authorization", "Bearer "+t.authToken)
			}
			resp, err := t.client.Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	return nil
}

// DaemonTransport connects to a daemon-managed server via Unix socket.
// The daemon keeps the server warm and already initialized.
type DaemonTransport struct {
	conn        net.Conn
	reader      *bufio.Reader
	mu          sync.Mutex
	callTimeout time.Duration // 0 = no read timeout
}

func NewDaemonTransport(serverName string) (*DaemonTransport, error) {
	sockPath := daemonSocketPath(serverName)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	return &DaemonTransport{
		conn:        conn,
		reader:      bufio.NewReader(conn),
		callTimeout: defaultDaemonCallTimeout,
	}, nil
}

// SetTimeout updates the per-call read timeout. 0 means no timeout.
func (t *DaemonTransport) SetTimeout(d time.Duration) {
	t.mu.Lock()
	t.callTimeout = d
	t.mu.Unlock()
}

func (t *DaemonTransport) Send(req jsonrpcRequest) (jsonrpcResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := json.Marshal(req)
	if err != nil {
		return jsonrpcResponse{}, fmt.Errorf("marshal: %w", err)
	}

	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := t.conn.Write(append(data, '\n')); err != nil {
		return jsonrpcResponse{}, fmt.Errorf("write: %w", err)
	}

	if t.callTimeout > 0 {
		_ = t.conn.SetReadDeadline(time.Now().Add(t.callTimeout))
	} else {
		_ = t.conn.SetReadDeadline(time.Time{})
	}
	for {
		line, err := t.reader.ReadBytes('\n')
		if err != nil {
			return jsonrpcResponse{}, fmt.Errorf("read: %w", err)
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var resp jsonrpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue
		}
		return resp, nil
	}
}

func (t *DaemonTransport) SendStreaming(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	return t.Send(req)
}

func (t *DaemonTransport) Notify(notif jsonrpcNotification) error {
	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_ = t.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = t.conn.Write(append(data, '\n'))
	return err
}

func (t *DaemonTransport) Close() error {
	return t.conn.Close()
}
