package main

import (
	"encoding/json"
	"sync/atomic"
)

var nextRequestID atomic.Int64

func nextID() int {
	return int(nextRequestID.Add(1))
}

const jsonrpcVersion = "2.0"

// JSON-RPC 2.0 types

type jsonrpcRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  any `json:"params,omitempty"`
}

type jsonrpcNotification struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  any `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string {
	return e.Message
}

// MCP protocol types

type initializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    clientCapabilities `json:"capabilities"`
	ClientInfo      clientInfo         `json:"clientInfo"`
}

type clientCapabilities struct{}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type toolsListResult struct {
	Tools      []mcpTool `json:"tools"`
	NextCursor string    `json:"nextCursor,omitempty"`
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type toolCallParams struct {
	Name      string                 `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Data     string `json:"data,omitempty"`
}

// CLI output types

type toolOutput struct {
	Server      string          `json:"server"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

type callOutput struct {
	Content   string `json:"content"`
	IsError   bool   `json:"isError"`
	Truncated bool   `json:"truncated,omitempty"`
}

type streamEvent struct {
	Type string `json:"type"` // "progress"
	Data string `json:"data,omitempty"`
}

type authOutput struct {
	AuthURL string `json:"auth_url,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Status  string `json:"status"` // "pending", "complete"
	Server  string `json:"server,omitempty"`
}
