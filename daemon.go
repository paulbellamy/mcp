package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const daemonIdleTimeout = 30 * time.Minute

type managedServer struct {
	config    ServerConfig
	transport Transport
	listener  net.Listener
	mu        sync.Mutex // serializes client access
}

type daemon struct {
	servers      map[string]*managedServer
	mu           sync.Mutex
	activeConns  int
	lastActivity time.Time
	done         chan struct{}
}

func cmdDaemon(args []string) error {
	if len(args) > 0 && args[0] == "stop" {
		return cmdDaemonStop()
	}
	return cmdDaemonRun()
}

func cmdDaemonStop() error {
	data, err := os.ReadFile(daemonPIDPath())
	if err != nil {
		return fmt.Errorf("daemon not running (no PID file)")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return fmt.Errorf("invalid PID file: %w", err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		_ = os.Remove(daemonPIDPath())
		return fmt.Errorf("signal process: %w", err)
	}
	logStderr("sent SIGTERM to daemon (PID %d)", pid)
	return nil
}

func cmdDaemonRun() error {
	// Check if already running
	if data, err := os.ReadFile(daemonPIDPath()); err == nil {
		pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
		if pid > 0 {
			if proc, err := os.FindProcess(pid); err == nil {
				if err := proc.Signal(syscall.Signal(0)); err == nil {
					return fmt.Errorf("daemon already running (PID %d)", pid)
				}
			}
		}
		_ = os.Remove(daemonPIDPath())
	}

	// Ensure daemon socket directory exists
	if err := os.MkdirAll(daemonSocketDir(), 0700); err != nil {
		return fmt.Errorf("create daemon dir: %w", err)
	}

	// Clean up stale sockets
	entries, _ := os.ReadDir(daemonSocketDir())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sock") {
			_ = os.Remove(filepath.Join(daemonSocketDir(), e.Name()))
		}
	}

	// Write PID file
	if err := atomicWrite(daemonPIDPath(), []byte(strconv.Itoa(os.Getpid())+"\n")); err != nil {
		return fmt.Errorf("write PID file: %w", err)
	}
	defer func() { _ = os.Remove(daemonPIDPath()) }()

	servers, err := loadServers()
	if err != nil {
		return err
	}

	d := &daemon{
		servers:      make(map[string]*managedServer),
		lastActivity: time.Now(),
		done:         make(chan struct{}),
	}

	for _, s := range servers {
		if !s.IsEnabled() || s.Transport != "stdio" {
			continue
		}
		ms, err := d.startServer(s)
		if err != nil {
			logStderr("warning: failed to start %q: %v", s.Name, err)
			continue
		}
		d.servers[s.Name] = ms
		logStderr("started %q", s.Name)
	}

	if len(d.servers) == 0 {
		logStderr("no stdio servers to manage")
		return nil
	}

	// Start accept loops
	for _, ms := range d.servers {
		go d.acceptLoop(ms)
	}

	// Idle timeout watcher
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				d.mu.Lock()
				active := d.activeConns
				last := d.lastActivity
				d.mu.Unlock()
				if active == 0 && time.Since(last) >= daemonIdleTimeout {
					close(d.done)
					return
				}
			case <-d.done:
				return
			}
		}
	}()

	logStderr("daemon ready (%d servers)", len(d.servers))

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		logStderr("received %v, shutting down", sig)
	case <-d.done:
		logStderr("idle timeout, shutting down")
	}

	d.shutdown()
	return nil
}

func (d *daemon) startServer(config ServerConfig) (*managedServer, error) {
	transport, err := NewStdioTransport(config.Command, config.Args)
	if err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	// MCP initialize handshake
	initResp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: "2025-03-26",
			Capabilities:    clientCapabilities{},
			ClientInfo:      clientInfo{Name: "mcp-cli", Version: Version},
		},
	})
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if initResp.Error != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	if err := transport.Notify(jsonrpcNotification{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	}); err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("initialized notification: %w", err)
	}

	// Create Unix socket
	sockPath := daemonSocketPath(config.Name)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("listen: %w", err)
	}
	_ = os.Chmod(sockPath, 0600)

	return &managedServer{
		config:    config,
		transport: transport,
		listener:  ln,
	}, nil
}

func (d *daemon) acceptLoop(ms *managedServer) {
	for {
		conn, err := ms.listener.Accept()
		if err != nil {
			return // listener closed
		}

		d.mu.Lock()
		d.activeConns++
		d.mu.Unlock()

		// Serialized: one client at a time per server
		ms.mu.Lock()
		d.handleClient(conn, ms)
		ms.mu.Unlock()

		d.mu.Lock()
		d.activeConns--
		d.lastActivity = time.Now()
		d.mu.Unlock()
	}
}

func (d *daemon) handleClient(conn net.Conn, ms *managedServer) {
	defer func() { _ = conn.Close() }()
	reader := bufio.NewReader(conn)

	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}

		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		// Peek to determine if request or notification
		var peek struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(line, &peek); err != nil {
			continue
		}

		if peek.ID == nil || string(peek.ID) == "null" {
			// Notification — forward, no response
			var notif jsonrpcNotification
			if err := json.Unmarshal(line, &notif); err == nil {
				_ = ms.transport.Notify(notif)
			}
			continue
		}

		// Request — forward and return response
		var req jsonrpcRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		resp, err := ms.transport.Send(req)
		if err != nil {
			// Server may have crashed — try respawn once
			if respawnErr := d.respawnServer(ms); respawnErr == nil {
				resp, err = ms.transport.Send(req)
			}
		}

		var respData []byte
		if err != nil {
			errResp := jsonrpcResponse{
				JSONRPC: jsonrpcVersion,
				ID:      peek.ID,
				Error:   &jsonrpcError{Code: -32603, Message: err.Error()},
			}
			respData, _ = json.Marshal(errResp)
		} else {
			respData, _ = json.Marshal(resp)
		}

		if _, writeErr := conn.Write(append(respData, '\n')); writeErr != nil {
			return
		}
	}
}

func (d *daemon) respawnServer(ms *managedServer) error {
	logStderr("respawning %q...", ms.config.Name)
	_ = ms.transport.Close()

	transport, err := NewStdioTransport(ms.config.Command, ms.config.Args)
	if err != nil {
		return err
	}

	initResp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: "2025-03-26",
			Capabilities:    clientCapabilities{},
			ClientInfo:      clientInfo{Name: "mcp-cli", Version: Version},
		},
	})
	if err != nil {
		_ = transport.Close()
		return err
	}
	if initResp.Error != nil {
		_ = transport.Close()
		return fmt.Errorf("%s", initResp.Error.Message)
	}

	if err := transport.Notify(jsonrpcNotification{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	}); err != nil {
		_ = transport.Close()
		return err
	}

	ms.transport = transport
	logStderr("respawned %q", ms.config.Name)
	return nil
}

func (d *daemon) shutdown() {
	for name, ms := range d.servers {
		_ = ms.listener.Close()
		_ = os.Remove(daemonSocketPath(name))
		_ = ms.transport.Close()
	}
}
