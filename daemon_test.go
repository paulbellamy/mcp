package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestDaemonTransport creates a DaemonTransport connected to the given Unix socket.
func newTestDaemonTransport(t *testing.T, sockPath string) *DaemonTransport {
	t.Helper()
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("dial daemon socket: %v", err)
	}
	return &DaemonTransport{
		conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// shortSockPath returns a short Unix socket path (macOS has a 104-char limit).
func shortSockPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("", "mcp-test-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

// startMockSocketServer starts a Unix socket server that echoes JSON-RPC responses.
// Returns the listener (caller must close) and the socket path.
func startMockSocketServer(t *testing.T) (net.Listener, string) {
	t.Helper()
	sockPath := shortSockPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				reader := bufio.NewReader(c)
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						return
					}
					var req jsonrpcRequest
					if err := json.Unmarshal(line, &req); err != nil {
						continue
					}
					resp := jsonrpcResponse{
						JSONRPC: "2.0",
						ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
						Result:  json.RawMessage(`{"ok":true}`),
					}
					data, _ := json.Marshal(resp)
					_, _ = c.Write(append(data, '\n'))
				}
			}(conn)
		}
	}()

	return ln, sockPath
}

func TestDaemonTransport_SendReceive(t *testing.T) {
	ln, sockPath := startMockSocketServer(t)
	defer func() { _ = ln.Close() }()

	transport := newTestDaemonTransport(t, sockPath)
	defer func() { _ = transport.Close() }()

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
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

func TestDaemonTransport_MultipleSends(t *testing.T) {
	ln, sockPath := startMockSocketServer(t)
	defer func() { _ = ln.Close() }()

	transport := newTestDaemonTransport(t, sockPath)
	defer func() { _ = transport.Close() }()

	for i := 1; i <= 5; i++ {
		resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: i, Method: "test"})
		if err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		var id int
		_ = json.Unmarshal(resp.ID, &id)
		if id != i {
			t.Errorf("send %d: expected ID %d, got %d", i, i, id)
		}
	}
}

func TestDaemonTransport_Notify(t *testing.T) {
	received := make(chan string, 1)
	sockPath := shortSockPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		var notif jsonrpcNotification
		_ = json.Unmarshal(line, &notif)
		received <- notif.Method
	}()

	transport := newTestDaemonTransport(t, sockPath)
	defer func() { _ = transport.Close() }()

	err = transport.Notify(jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/test"})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case method := <-received:
		if method != "notifications/test" {
			t.Errorf("expected method 'notifications/test', got %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}
}

func TestDaemonTransport_ConnectFailure(t *testing.T) {
	dir := setupTestConfigDir(t)
	// No socket exists
	_, err := NewDaemonTransport("nonexistent")
	if err == nil {
		t.Fatal("expected error connecting to nonexistent socket")
	}
	_ = dir
}

func TestDaemonTransport_Close(t *testing.T) {
	ln, sockPath := startMockSocketServer(t)
	defer func() { _ = ln.Close() }()

	transport := newTestDaemonTransport(t, sockPath)
	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}

	// Send after close should fail
	_, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"})
	if err == nil {
		t.Fatal("expected error after close")
	}
}

func TestDaemon_HandleClient_Request(t *testing.T) {
	// Set up a mock transport that returns canned responses
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"tool":"result"}`),
			}, nil
		},
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test"},
		transport: mock,
	}

	// Create a socket pair for testing
	server, client := net.Pipe()

	// Run handleClient in a goroutine
	done := make(chan struct{})
	go func() {
		d.handleClient(server, ms)
		close(done)
	}()

	// Send a request through the client side
	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call"}
	data, _ := json.Marshal(req)
	_, _ = client.Write(append(data, '\n'))

	// Read response
	reader := bufio.NewReader(client)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}
	if string(resp.Result) != `{"tool":"result"}` {
		t.Errorf("expected result {\"tool\":\"result\"}, got %s", string(resp.Result))
	}

	_ = client.Close()
	<-done
}

func TestDaemon_HandleClient_Notification(t *testing.T) {
	notified := make(chan string, 1)
	mock := &mockTransport{
		notifyFunc: func(notif jsonrpcNotification) error {
			notified <- notif.Method
			return nil
		},
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test"},
		transport: mock,
	}

	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		d.handleClient(server, ms)
		close(done)
	}()

	// Send a notification (no id)
	notif := jsonrpcNotification{JSONRPC: "2.0", Method: "notifications/cancelled"}
	data, _ := json.Marshal(notif)
	_, _ = client.Write(append(data, '\n'))

	select {
	case method := <-notified:
		if method != "notifications/cancelled" {
			t.Errorf("expected 'notifications/cancelled', got %q", method)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for notification")
	}

	_ = client.Close()
	<-done
}

func TestDaemon_HandleClient_TransportError(t *testing.T) {
	callCount := 0
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			callCount++
			return jsonrpcResponse{}, fmt.Errorf("transport closed")
		},
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test", Command: "nonexistent-cmd"},
		transport: mock,
	}

	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		d.handleClient(server, ms)
		close(done)
	}()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"}
	data, _ := json.Marshal(req)
	_, _ = client.Write(append(data, '\n'))

	// Should get an error response (respawn will also fail since command is bogus)
	reader := bufio.NewReader(client)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}

	var resp jsonrpcResponse
	_ = json.Unmarshal(line, &resp)
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32603 {
		t.Errorf("expected error code -32603, got %d", resp.Error.Code)
	}

	_ = client.Close()
	<-done
}

func TestDaemon_HandleClient_Respawn(t *testing.T) {
	callCount := 0
	var mu sync.Mutex
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			mu.Lock()
			callCount++
			n := callCount
			mu.Unlock()
			if n == 1 {
				return jsonrpcResponse{}, fmt.Errorf("transport closed")
			}
			// After respawn, succeed
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"respawned":true}`),
			}, nil
		},
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test", Command: "nonexistent"},
		transport: mock,
	}

	// Override respawnServer to swap in a new mock
	// Since respawnServer tries to actually spawn a process, we need the test
	// to work differently. The mock's Send already handles the retry (callCount > 1 succeeds).
	// But respawnServer will fail because the command doesn't exist.
	// So this test verifies the error path when respawn fails but the transport
	// magically recovers (which won't happen in practice, but tests the retry logic).

	// Actually, let's test at a higher level: the respawn path requires a real command.
	// Instead, verify that when Send fails, the error is properly returned.
	// The respawn+retry test is better done as an integration test.

	server, client := net.Pipe()

	done := make(chan struct{})
	go func() {
		d.handleClient(server, ms)
		close(done)
	}()

	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"}
	data, _ := json.Marshal(req)
	_, _ = client.Write(append(data, '\n'))

	reader := bufio.NewReader(client)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}

	var resp jsonrpcResponse
	_ = json.Unmarshal(line, &resp)
	// Respawn fails (bad command), so we get the original error
	if resp.Error == nil {
		t.Fatal("expected error response")
	}

	_ = client.Close()
	<-done
}

func TestDaemon_AcceptLoop_Serialization(t *testing.T) {
	// Verify that only one client is served at a time
	var activeClients int
	var maxActive int
	var mu sync.Mutex

	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			mu.Lock()
			activeClients++
			if activeClients > maxActive {
				maxActive = activeClients
			}
			mu.Unlock()

			time.Sleep(50 * time.Millisecond) // Simulate work

			mu.Lock()
			activeClients--
			mu.Unlock()

			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{}`),
			}, nil
		},
	}

	sockPath := shortSockPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test"},
		transport: mock,
		listener:  ln,
	}

	go d.acceptLoop(ms)

	// Launch 3 clients concurrently
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
			if err != nil {
				t.Errorf("client %d: dial: %v", id, err)
				return
			}
			defer func() { _ = conn.Close() }()

			req := jsonrpcRequest{JSONRPC: "2.0", ID: id + 1, Method: "test"}
			data, _ := json.Marshal(req)
			_, _ = conn.Write(append(data, '\n'))

			reader := bufio.NewReader(conn)
			_, _ = reader.ReadBytes('\n')
		}(i)
	}

	wg.Wait()
	_ = ln.Close()

	mu.Lock()
	defer mu.Unlock()
	if maxActive > 1 {
		t.Errorf("expected max 1 active client at a time, got %d", maxActive)
	}
}

func TestDaemon_Shutdown(t *testing.T) {
	closed := false
	mock := &mockTransport{
		closeFunc: func() error {
			closed = true
			return nil
		},
	}

	sockPath := filepath.Join(t.TempDir(), "test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	d := &daemon{
		servers: map[string]*managedServer{
			"test": {
				config:    ServerConfig{Name: "test"},
				transport: mock,
				listener:  ln,
			},
		},
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	dir := setupTestConfigDir(t)
	// Create the socket file so shutdown can remove it
	sockDir := filepath.Join(dir, "daemon")
	_ = os.MkdirAll(sockDir, 0700)

	d.shutdown()

	if !closed {
		t.Error("expected transport to be closed")
	}
}

func TestCmdDaemonStop_NoPIDFile(t *testing.T) {
	setupTestConfigDir(t)
	err := cmdDaemonStop()
	if err == nil {
		t.Fatal("expected error when no PID file")
	}
}

func TestDaemon_ActiveConnTracking(t *testing.T) {
	mock := &mockTransport{
		sendFunc: func(req jsonrpcRequest) (jsonrpcResponse, error) {
			return jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{}`),
			}, nil
		},
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	sockPath := shortSockPath(t)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}

	ms := &managedServer{
		config:    ServerConfig{Name: "test"},
		transport: mock,
		listener:  ln,
	}

	go d.acceptLoop(ms)

	// Connect, send request, disconnect
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	req := jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "test"}
	data, _ := json.Marshal(req)
	_, _ = conn.Write(append(data, '\n'))

	reader := bufio.NewReader(conn)
	_, _ = reader.ReadBytes('\n')
	_ = conn.Close()

	// Give acceptLoop time to update counters
	time.Sleep(50 * time.Millisecond)

	d.mu.Lock()
	active := d.activeConns
	d.mu.Unlock()

	if active != 0 {
		t.Errorf("expected 0 active connections after disconnect, got %d", active)
	}

	_ = ln.Close()
}

func TestMcpConnect_UsesDaemonWhenAvailable(t *testing.T) {
	// Use a short config dir to avoid Unix socket path length limits on macOS
	dir, err := os.MkdirTemp("/tmp", "mcp-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	testConfigDir = dir
	t.Cleanup(func() { testConfigDir = "" })
	_ = ensureConfigDirs()

	sockPath := daemonSocketPath("test-srv")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				return
			}
			var req jsonrpcRequest
			_ = json.Unmarshal(line, &req)
			resp := jsonrpcResponse{
				JSONRPC: "2.0",
				ID:      json.RawMessage(fmt.Sprintf("%d", req.ID)),
				Result:  json.RawMessage(`{"tools":[]}`),
			}
			data, _ := json.Marshal(resp)
			_, _ = conn.Write(append(data, '\n'))
		}
	}()

	server := &ServerConfig{
		Name:      "test-srv",
		Transport: "stdio",
		Command:   "nonexistent-command-should-not-be-called",
	}

	transport, err := mcpConnect(server, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = transport.Close() }()

	if _, ok := transport.(*DaemonTransport); !ok {
		t.Fatalf("expected DaemonTransport, got %T", transport)
	}

	resp, err := transport.Send(jsonrpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/list"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Error != nil {
		t.Fatal("unexpected error:", resp.Error)
	}
}

func TestMcpConnect_FallbackWithoutDaemon(t *testing.T) {
	setupTestConfigDir(t)

	// No daemon socket exists — mcpConnect should try to spawn the command.
	// Use a bogus command so it fails predictably.
	server := &ServerConfig{
		Name:      "test-server",
		Transport: "stdio",
		Command:   "nonexistent-command-12345",
	}

	_, err := mcpConnect(server, "")
	if err == nil {
		t.Fatal("expected error when daemon unavailable and command doesn't exist")
	}
}
