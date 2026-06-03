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

// codeMethodNotFound is the JSON-RPC error code a server returns when it does
// not implement a method (e.g. a server with no resources support replying to
// resources/list). We treat it as "feature unsupported" rather than a failure.
const codeMethodNotFound = -32601

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

// Resource protocol types

type resourcesListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type resourcesListResult struct {
	Resources  []mcpResource `json:"resources"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type mcpResource struct {
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type resourceTemplatesListResult struct {
	ResourceTemplates []mcpResourceTemplate `json:"resourceTemplates"`
	NextCursor        string                `json:"nextCursor,omitempty"`
}

type mcpResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type resourceReadParams struct {
	URI string `json:"uri"`
}

type resourceReadResult struct {
	Contents []resourceContents `json:"contents"`
}

type resourceContents struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
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

// resourceOutput is the unified `mcp resources` row for both concrete
// resources (URI set) and resource templates (URITemplate set).
type resourceOutput struct {
	Server      string `json:"server"`
	URI         string `json:"uri,omitempty"`
	URITemplate string `json:"uriTemplate,omitempty"`
	Name        string `json:"name,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
	Size        int64  `json:"size,omitempty"`
}

type readOutput struct {
	Contents  []readContent `json:"contents"`
	Truncated bool          `json:"truncated,omitempty"`
}

type readContent struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
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
