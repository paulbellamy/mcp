# Design: x-mcp-header, subscriptions/listen, CIMD

Follow-ups to the 2026-07-28 stateless protocol support (see
`protocol-2026-07-28.md`). Three independent features, implemented in this
order because the first two share transport plumbing.

## 1. Custom parameter headers (`x-mcp-header` → `Mcp-Param-*`)

Spec: Streamable HTTP transport §Custom Headers from Tool Parameters.
Clients on streamable HTTP MUST mirror designated tool arguments into
`Mcp-Param-{Name}` headers and MUST reject tool definitions with invalid
annotations.

### Rules being implemented

- An `inputSchema` property may carry `"x-mcp-header": "<Name>"`. The header
  sent is `Mcp-Param-<Name>` with the argument's value.
- Annotation validity: non-empty; RFC 9110 token syntax (`1*tchar`); no
  CR/LF/control chars; case-insensitively unique within the schema; only on
  primitive-typed properties (`string`, `integer`, `boolean` — `number` is
  not permitted); only on properties *statically reachable* through chains
  of `properties` keys (never through `items`, composition keywords,
  `if`/`then`/`else`, or `$ref`). Any violation invalidates the whole tool
  definition.
- Tool rejection: streamable-HTTP clients MUST exclude tools with invalid
  annotations from `tools/list` results and SHOULD log a warning naming the
  tool and reason. Stdio clients MAY ignore annotations (we ignore them:
  no headers, no rejection).
- Value encoding: string as-is; integer as decimal (must be within ±(2^53−1));
  boolean lowercase. Values that are not header-safe use the existing
  `=?base64?…?=` sentinel (`encodeHeaderValue`). `null` or absent argument →
  header omitted. Non-primitive runtime value → header omitted (the server
  validates against the body anyway).
- HeaderMismatch recovery: if a `tools/call` fails with JSON-RPC `-32020`
  (raw or inside an HTTP 400 body), refresh the tool list once, recompute
  headers from the fresh schema, and retry the call once.

### Implementation

- `headers.go` (new): schema walking + validation + value extraction.
  - `extractHeaderParams(inputSchema json.RawMessage) ([]headerParam, error)`
    where `headerParam` is `{Path []string, Name string}`. Error = invalid
    tool definition (with reason).
  - `headerValuesForCall(params []headerParam, args map[string]any) map[string]string`
    applying the value-encoding rules.
- `protocol.go`: `jsonrpcRequest` gains `ExtraHeaders map[string]string
  `json:"-"`` (never serialized). `HTTPTransport.sendWithContext` sets them
  in modern mode only. `protocolSession.decorate` must not drop the field.
- `tools.go` `listAllTools`: when the transport speaks modern streamable
  HTTP (unwrap `*protocolSession` → `*HTTPTransport`), validate each tool's
  annotations and drop invalid tools with a stderr warning. Add a helper
  `isModernHTTP(transport Transport) bool`.
- `call.go`: `cmdCall` resolves the tool's cached schema (already loaded for
  flag coercion; load it also on the `--params` path, tolerating absence),
  computes header params, and passes them through `executeToolCall` (new
  `extraHeaders map[string]string` parameter threaded into each retry
  request). A `-32020` error triggers the one-shot refresh+retry described
  above (in `cmdCall`, since it owns schema access).

### Tests

- Extraction: top-level and nested (`properties` chain) annotations; each
  invalid case (empty, bad tchar, dup case-insensitive, `number` type,
  under `items`/`oneOf`/`$ref`) rejects the tool; valid tools unaffected.
- Encoding: integer/boolean/string conversion, base64 sentinel for
  non-ASCII, omission for null/absent/non-primitive.
- End-to-end modern HTTP mock asserting `Mcp-Param-Region: us-west1` on
  `tools/call` and validating header/body match.
- Rejection: `tools/list` over modern HTTP drops the invalid tool, keeps
  the valid one, warns on stderr; over stdio nothing is dropped.
- HeaderMismatch: first call → 400/-32020, client refreshes tools/list,
  retries with correct headers, succeeds; second -32020 is surfaced.

## 2. `subscriptions/listen` (`mcp listen`)

Spec: Subscriptions pattern. A long-lived request whose response stream
carries opted-in change notifications; replaces the legacy GET stream.

### CLI surface

`mcp listen <server|url> [--tools] [--prompts] [--resources]
[--resource <uri>]... [--json]`

- Builds `subscriptionsListenParams{notifications: {toolsListChanged,
  promptsListChanged, resourcesListChanged, resourceSubscriptions}}` from
  flags (at least one required).
- Prints one JSON line per event to stdout:
  `{"type":"acknowledged","notifications":{…}}`, then
  `{"type":"notification","method":"notifications/…","params":{…}}` per
  event, and `{"type":"closed"}` on graceful server close (exit 0).
- No timeout on the stream (`SetTimeout(0)` for the listen call).
- Ctrl-C: process exit closes the HTTP stream — that is the spec's
  cancellation signal. On stdio, also best-effort send
  `notifications/cancelled` with the listen request id.
- Legacy-era servers (negotiation fell back to initialize): fail fast with
  a clear "server does not support subscriptions/listen (pre-2026 protocol)"
  error. Do not emulate via the removed GET stream. A `-32601` response
  gets the same message.
- Daemon: `mcp listen` connects directly (never through the daemon socket —
  the daemon serializes one client per server, and a listen stream would
  starve other clients). Skip the daemon fast-path for this command.

### Transport plumbing

- HTTP: `SendStreaming` already delivers non-response SSE events through
  `onEvent` and returns when the final response arrives or the stream ends.
  Listen uses it as-is; events whose JSON parses as a notification are
  decoded and correlated via `_meta["io.modelcontextprotocol/subscriptionId"]`
  == the listen request id (log-and-skip mismatches). A returned response
  with empty result = graceful close. Stream EOF without response = abrupt
  disconnect → nonzero exit with message.
- stdio: `StdioTransport.readLoop` currently drops notifications. Add an
  optional notification sink: `SendStreaming` (only when onEvent != nil)
  registers a handler fed each notification line until the final response
  arrives; then unregisters. Guarded by the existing mutex.
- No changes to DaemonTransport.

### Tests

- httptest SSE server: asserts filter params and `_meta`; streams
  acknowledged → two notifications → graceful-close response; client
  prints 4 JSON lines and exits nil. Keep-alive comment lines (`:`)
  interleaved and ignored.
- Abrupt close (no response) → error.
- Filter flags → params mapping; no flags → usage error.
- Legacy server → actionable error, no GET fallback.
- stdio: fake transport delivering notifications through SendStreaming;
  subscriptionId correlation; unrelated-subscription messages skipped.

## 3. CIMD client registration (+ `application_type`, issuer binding)

Spec: Authorization §Client Registration. Priority order: pre-registered →
CIMD (if `client_id_metadata_document_supported`) → DCR (deprecated
fallback) → manual.

### Changes

- `authServerMetadata` gains
  `ClientIDMetadataDocumentSupported bool `json:"client_id_metadata_document_supported,omitempty"``
  and `Issuer` is already parsed.
- Client metadata URL source: `MCP_CLIENT_METADATA_URL` env var (a CLI
  cannot host a document itself; the user/deployment provides the HTTPS
  URL). Validation: https scheme, non-empty path.
- Selection logic in the auth flow (where `registerClient` is called today):
  1. Static `MCP_CLIENT_ID` (existing behavior, unchanged).
  2. If AS advertises CIMD support and `MCP_CLIENT_METADATA_URL` is set:
     use the URL as `client_id`, `token_endpoint_auth_method: "none"`
     (public client + PKCE, already implemented), skip registration
     entirely.
  3. Else DCR as today, now with `application_type: "native"` in the
     registration request (the CLI is a native app; loopback redirect).
- Issuer binding: `AuthTokens` and `PendingAuth` gain
  `Issuer string `json:"issuer,omitempty"``, recorded from the discovered
  AS metadata. Before reusing persisted credentials (`getAuthToken` refresh
  path and the auth idempotency probe), compare stored issuer to the
  freshly discovered one: on mismatch, do not reuse the client_id/secret or
  refresh token — log that the authorization server changed and force
  re-auth/re-registration. Tokens saved before the field existed (empty
  issuer) keep working (no comparison).

### Tests

- Metadata parsing of the new fields.
- CIMD path: AS advertises support + env URL set → authorize URL and token
  exchange carry the metadata URL as client_id; no POST to the
  registration endpoint.
- CIMD ignored when AS does not advertise support (DCR still used).
- Invalid metadata URL (http, no path) → clear error before any network.
- DCR request body includes `"application_type":"native"`.
- Issuer mismatch: stored tokens with issuer A, discovery now returns
  issuer B → credentials not reused, re-registration happens; empty stored
  issuer → legacy tolerance, credentials reused.
