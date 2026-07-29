package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// protocolSession wraps a Transport for servers speaking the stateless
// 2026-07-28 protocol revision. Every outgoing request is decorated with the
// required per-request _meta fields; the underlying transport adds the
// era-specific HTTP headers.
type protocolSession struct {
	transport Transport
	version   string
}

func newProtocolSession(t Transport, version string) *protocolSession {
	return &protocolSession{transport: t, version: version}
}

// decorate merges the required protocol _meta fields into the request params,
// materializing an empty params object when the request had none (modern
// requests must always carry _meta). Existing _meta keys such as
// progressToken are preserved.
func (s *protocolSession) decorate(req jsonrpcRequest) jsonrpcRequest {
	params := make(map[string]any)
	if req.Params != nil {
		if data, err := json.Marshal(req.Params); err == nil {
			_ = json.Unmarshal(data, &params)
		}
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		meta = make(map[string]any)
	}
	meta[metaProtocolVersion] = s.version
	meta[metaClientCapabilities] = clientCapabilities{}
	meta[metaClientInfo] = clientInfo{Name: clientName, Version: Version}
	params["_meta"] = meta
	req.Params = params
	return req
}

func (s *protocolSession) Send(req jsonrpcRequest) (jsonrpcResponse, error) {
	return s.transport.Send(s.decorate(req))
}

func (s *protocolSession) SendStreaming(req jsonrpcRequest, onEvent func(streamEvent)) (jsonrpcResponse, error) {
	return s.transport.SendStreaming(s.decorate(req), onEvent)
}

func (s *protocolSession) Notify(notif jsonrpcNotification) error {
	return s.transport.Notify(notif)
}

func (s *protocolSession) Close() error {
	return s.transport.Close()
}

func (s *protocolSession) SetTimeout(d time.Duration) {
	s.transport.SetTimeout(d)
}

// protocolVersionSetter is implemented by transports that vary per-request
// framing by protocol era (HTTP headers). An empty version selects legacy
// behavior.
type protocolVersionSetter interface {
	setProtocolVersion(version string)
}

// negotiateProtocol determines which protocol era the server behind transport
// speaks and returns a ready-to-use transport. It probes with server/discover
// (which 2026-07-28 servers must implement). A successful probe yields a
// stateless session; a recognized modern error (UnsupportedProtocolVersion)
// is surfaced without fallback; anything else identifies a legacy server and
// the client falls back to the initialize handshake. The era decision holds
// for the lifetime of the connection.
func negotiateProtocol(transport Transport) (Transport, error) {
	if v, ok := transport.(protocolVersionSetter); ok {
		v.setProtocolVersion(protocolVersionModern)
	}

	modern := newProtocolSession(transport, protocolVersionModern)
	resp, err := modern.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "server/discover",
	})
	if err != nil {
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) {
			// Modern servers also use 400 for version/capability/header
			// errors — inspect the body before falling back.
			if modernErr := parseModernError(statusErr.body); modernErr != nil {
				return nil, modernNegotiationError(modernErr)
			}
			switch statusErr.status {
			case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotAcceptable:
				return legacyHandshake(transport)
			}
			return nil, err
		}
		if _, isHTTP := transport.(*HTTPTransport); isHTTP {
			// Network-level failure — a legacy handshake over the same
			// broken connection cannot do better.
			return nil, err
		}
		// stdio: a legacy server may ignore or reject unknown
		// pre-initialize requests in arbitrary ways (including staying
		// silent until the probe times out) — fall back.
		return legacyHandshake(transport)
	}

	if resp.Error != nil {
		if resp.Error.Code == codeUnsupportedProtocolVersion {
			return nil, modernNegotiationError(resp.Error)
		}
		return legacyHandshake(transport)
	}

	var result discoverResult
	if err := json.Unmarshal(resp.Result, &result); err != nil || !slices.Contains(result.SupportedVersions, protocolVersionModern) {
		// No parseable modern discover result (a modern server must
		// advertise supportedVersions), or none we speak; a dual-era
		// server still accepts initialize.
		return legacyHandshake(transport)
	}

	return modern, nil
}

// legacyHandshake performs the pre-2026 initialize/initialized exchange and
// returns the bare transport for legacy-style requests.
func legacyHandshake(transport Transport) (Transport, error) {
	if v, ok := transport.(protocolVersionSetter); ok {
		v.setProtocolVersion("")
	}

	initResp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "initialize",
		Params: initializeParams{
			ProtocolVersion: protocolVersionLegacy,
			Capabilities:    clientCapabilities{},
			ClientInfo:      clientInfo{Name: clientName, Version: Version},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if initResp.Error != nil {
		return nil, fmt.Errorf("initialize: %s", initResp.Error.Message)
	}

	if err := transport.Notify(jsonrpcNotification{
		JSONRPC: jsonrpcVersion,
		Method:  "notifications/initialized",
	}); err != nil {
		return nil, fmt.Errorf("send initialized notification: %w", err)
	}

	return transport, nil
}

// parseModernError extracts a JSON-RPC error carrying a 2026-07-28 spec code
// from an HTTP error body. It returns nil when the body is not a recognized
// modern error (the legacy-fallback signal).
func parseModernError(body []byte) *jsonrpcError {
	var resp jsonrpcResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error == nil {
		return nil
	}
	switch resp.Error.Code {
	case codeHeaderMismatch, codeMissingRequiredClientCapability, codeUnsupportedProtocolVersion:
		return resp.Error
	}
	return nil
}

// modernNegotiationError renders a recognized modern negotiation error into
// an actionable message. Per spec, these must not trigger legacy fallback.
func modernNegotiationError(e *jsonrpcError) error {
	if e.Code == codeUnsupportedProtocolVersion {
		var data unsupportedVersionData
		_ = json.Unmarshal(e.Data, &data)
		if len(data.Supported) > 0 {
			return fmt.Errorf("server does not support protocol version %s (server supports: %s)",
				protocolVersionModern, strings.Join(data.Supported, ", "))
		}
		return fmt.Errorf("server does not support protocol version %s", protocolVersionModern)
	}
	return fmt.Errorf("server/discover: %s", e.Message)
}
