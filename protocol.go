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
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
	// ExtraHeaders carries per-request HTTP headers (the Mcp-Param-* custom
	// parameter headers) for modern streamable HTTP. Never serialized into
	// the JSON-RPC body; non-HTTP transports ignore it.
	ExtraHeaders map[string]string `json:"-"`
}

type jsonrpcNotification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *jsonrpcError) Error() string {
	return e.Message
}

// MCP protocol types

// Protocol revisions the CLI speaks. Modern (2026-07-28 and later) servers
// are stateless: every request carries its protocol version and capabilities
// in _meta and there is no initialize handshake. Legacy servers negotiate a
// session via initialize.
const (
	protocolVersionModern = "2026-07-28"
	protocolVersionLegacy = "2025-03-26"
)

const clientName = "mcp-cli"

// Reserved _meta keys carried on every modern request.
const (
	metaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	metaClientInfo         = "io.modelcontextprotocol/clientInfo"
	metaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
)

// Subscriptions pattern (2026-07-28): subscriptions/listen is a long-lived
// request whose response stream carries opted-in change notifications. It
// replaces the removed HTTP GET stream.
const (
	methodSubscriptionsListen = "subscriptions/listen"
	// methodSubscriptionAcknowledged is the stream notification confirming
	// which change notifications the server accepted for a subscription.
	methodSubscriptionAcknowledged = "notifications/subscriptions/acknowledged"
	// metaSubscriptionID is the _meta key correlating each stream
	// notification with the listen request id that subscribed to it.
	metaSubscriptionID = "io.modelcontextprotocol/subscriptionId"
)

type subscriptionsListenParams struct {
	Notifications subscriptionNotifications `json:"notifications"`
}

type subscriptionNotifications struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}

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

// codeMethodNotFound is treated as "feature unsupported" (e.g. a server without
// resources support) rather than a hard failure.
const codeMethodNotFound = -32601

// Error codes reserved by the 2026-07-28 spec (range -32020 to -32099).
const (
	codeHeaderMismatch                  = -32020
	codeMissingRequiredClientCapability = -32021
	codeUnsupportedProtocolVersion      = -32022
)

// unsupportedVersionData is the data payload of an
// UnsupportedProtocolVersionError (-32022).
type unsupportedVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested,omitempty"`
}

// discoverResult is the result of server/discover, which modern servers must
// implement. The CLI uses it both for version selection and as the
// backward-compatibility probe.
type discoverResult struct {
	SupportedVersions []string        `json:"supportedVersions"`
	Capabilities      json.RawMessage `json:"capabilities,omitempty"`
	Instructions      string          `json:"instructions,omitempty"`
}

// Result envelope types (2026-07-28). An absent resultType from an
// earlier-protocol server is treated as "complete".
const (
	resultTypeComplete      = "complete"
	resultTypeInputRequired = "input_required"
)

// inputRequest is one server-initiated request embedded in an
// InputRequiredResult under the multi round-trip request (MRTR) pattern.
type inputRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type toolsListParams struct {
	Cursor string `json:"cursor,omitempty"`
}

type toolsListResult struct {
	Tools      []mcpTool `json:"tools"`
	NextCursor string    `json:"nextCursor,omitempty"`
	// TTLMs is the 2026-07-28 freshness hint bounding how long the listing
	// may be cached. 0 means the server provided none.
	TTLMs      int64  `json:"ttlMs,omitempty"`
	CacheScope string `json:"cacheScope,omitempty"`
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
	// RequestState echoes an InputRequiredResult's opaque state on an MRTR
	// retry. Never set on the initial request.
	RequestState string `json:"requestState,omitempty"`
}

type resourceReadResult struct {
	Contents []resourceContents `json:"contents"`
	// 2026-07-28 result envelope; see toolCallResult.
	ResultType    string                  `json:"resultType,omitempty"`
	InputRequests map[string]inputRequest `json:"inputRequests,omitempty"`
	RequestState  string                  `json:"requestState,omitempty"`
}

type resourceContents struct {
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

type toolCallParams struct {
	Name string `json:"name"`
	// Arguments is always serialized, even when empty. Per the MCP spec the
	// field is optional, but some servers reject a tools/call that omits it.
	// A nil map would marshal to null, so executeToolCall ensures it is a
	// non-nil map; an empty map marshals to {}.
	Arguments map[string]any `json:"arguments"`
	// RequestState echoes an InputRequiredResult's opaque state on an MRTR
	// retry. Never set on the initial request.
	RequestState string `json:"requestState,omitempty"`
}

type toolCallResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
	// ResultType is the 2026-07-28 result envelope discriminator. Absent
	// (legacy servers) means "complete". "input_required" carries
	// InputRequests and/or RequestState per the MRTR pattern.
	ResultType    string                  `json:"resultType,omitempty"`
	InputRequests map[string]inputRequest `json:"inputRequests,omitempty"`
	RequestState  string                  `json:"requestState,omitempty"`
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

// resourceOutput is one row for both concrete resources (URI) and templates
// (URITemplate), so `mcp resources` can list them together.
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

// listenOutput is one JSON line printed by `mcp listen`: the server's
// acknowledgment, a change notification, or the graceful close marker.
type listenOutput struct {
	Type          string          `json:"type"` // "acknowledged", "notification", "closed"
	Notifications json.RawMessage `json:"notifications,omitempty"`
	Method        string          `json:"method,omitempty"`
	Params        json.RawMessage `json:"params,omitempty"`
}

type authOutput struct {
	AuthURL string `json:"auth_url,omitempty"`
	Nonce   string `json:"nonce,omitempty"`
	Status  string `json:"status"` // "pending", "complete"
	Server  string `json:"server,omitempty"`
}
