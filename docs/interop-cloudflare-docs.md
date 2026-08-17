# Interop test plan: Cloudflare docs MCP (2026-07-28)

Target: `https://docs.mcp.cloudflare.com/mcp` — the first known public server
speaking protocol revision 2026-07-28 (`server/discover` reports
`supportedVersions: ["2026-07-28"]`, serverInfo `docs-ai-search`). No auth, so
every unauthenticated code path can be exercised against a real
implementation.

Two layers of testing:

- **Raw probes (curl)** validate the *server's* conformance and pin down the
  wire behavior our client must tolerate — including behavior the spec leaves
  open. These catch cases where our fixtures are stricter or laxer than
  reality.
- **CLI runs** validate *our* client end to end: negotiation, header
  emission, envelope handling, caching, the daemon path, and subscriptions.

Each test records: request sent, expected outcome per spec, actual outcome,
and verdict (PASS / DIVERGES / INFO). A spec divergence on the server side is
recorded, not "fixed"; a divergence on our side becomes a bug fix.

## A. Negotiation and `_meta` (raw probes)

| # | Probe | Expectation (spec) |
|---|-------|--------------------|
| A1 | `server/discover`, full modern `_meta` + headers | `supportedVersions` incl. 2026-07-28, `capabilities`, serverInfo in `_meta` |
| A2 | `protocolVersion: "2099-01-01"` (header + `_meta` consistent) | `-32022 UnsupportedProtocolVersion`, `data.supportedVersions` |
| A3 | Header `2026-07-28` but `_meta` says `2025-06-18` | `-32020 HeaderMismatch` |
| A4 | `Mcp-Method: tools/list` on a `server/discover` body | `-32020 HeaderMismatch` |
| A5 | Missing `MCP-Protocol-Version` header entirely | error (server SHOULD reject; exact code informative) |
| A6 | `_meta` missing `clientCapabilities` (required key) | `-32021 MissingRequiredClientCapability` (or documented leniency) |
| A7 | Stray `Mcp-Session-Id` header on a modern request | ignored or rejected — informative either way |
| A8 | Legacy `initialize` handshake against this server | rejected (only 2026-07-28 in supportedVersions) — confirms a modern-only server exists in the wild, i.e. our modern path is load-bearing |
| A9 | HTTP `GET` on the endpoint | 405 (the GET notification stream was removed) |

## B. Custom headers on name-carrying requests (raw probes)

| # | Probe | Expectation |
|---|-------|-------------|
| B1 | `tools/call` with `Mcp-Name` ≠ `params.name` | `-32020 HeaderMismatch` |
| B2 | `tools/call` name `検索` with `Mcp-Name: =?base64?5qSc57Si?=` | server decodes the sentinel: "unknown tool"-class error, NOT a header mismatch |
| B3 | `tools/call` with no `Mcp-Name` header at all | error or leniency — informative |

## C. Client end-to-end (built CLI, live server)

| # | Test | Verifies |
|---|------|----------|
| C1 | `mcp ping <url>` | modern liveness = `server/discover`, no initialize sent |
| C2 | `mcp tools <url>` twice; inspect cache file between runs | tools/list with `_meta` injection; envelope (`resultType`, `ttlMs`, `cacheScope`) parsed; **`ttlMs: 0` cache semantics** (see D1) |
| C3 | `mcp call <url> search_cloudflare_documentation --query …` | tools/call happy path: `Mcp-Method`/`Mcp-Name` headers accepted, `resultType: "complete"` handled |
| C4 | `mcp call` with an unknown tool name | server error surfaced as a structured CLI error, no fallback |
| C5 | `mcp call` missing the required `query` param | server-side validation error surfaced cleanly |
| C6 | `mcp listen <url> --tools` (timeboxed, SIGINT after ~10s) | subscriptions/listen against a server advertising `tools.listChanged`; acknowledged + clean close, or clean "unsupported" error |
| C7 | Same commands through the daemon (default connect path) | daemon-side negotiation for a modern child, request-ID remapping, decorated forwards — all against real latency |

## D. Findings to resolve against the spec

| # | Question | Why it matters |
|---|----------|----------------|
| D1 | Does `ttlMs: 0` mean "do not cache" or is `0` reserved/absent? | The live server sends `ttlMs: 0`. Our `toolsListResult.TTLMs` is a plain `int64`, so an explicit `0` is conflated with "no hint" and cached for the 10-minute default. If the spec says 0 = don't cache, that's a client bug (needs `*int64`). |
| D2 | Is `Mcp-Name` required or optional on tools/call? | Determines how strict our header emission must be and whether B3's result is a server divergence. |

## Results (2026-08-02, serverInfo docs-ai-search 0.4.10)

### A. Negotiation and `_meta`

| # | Actual | Verdict |
|---|--------|---------|
| A1 | `supportedVersions: ["2026-07-28"]`, tools+prompts capabilities, `resultType: "complete"`, `ttlMs: 0`, `cacheScope: "private"`, serverInfo in `_meta` | PASS |
| A2 | HTTP 400, `-32022`, `data: {"supported": [...], "requested": "2099-01-01"}` — field name is `supported`, which is what our `unsupportedVersionData` parses | PASS (both sides) |
| A3 | HTTP 400, `-32020` with a precise header-vs-body message | PASS |
| A4 | HTTP 400, `-32020` | PASS |
| A5 | Request accepted despite the missing `MCP-Protocol-Version` header | INFO: server is lenient; we always send the header, so no client impact |
| A6 | `-32602` "Invalid _meta envelope … clientCapabilities: missing" — not `-32021` | INFO: the server treats a malformed envelope as invalid params and reserves `-32021` for capability requirements. Our negotiation always sends a valid envelope, so this cannot steer era detection |
| A7 | Stray `Mcp-Session-Id` silently ignored | PASS |
| A8 | Legacy `initialize` still answered (2025-03-26, SSE-framed) | INFO: the server is dual-era; `supportedVersions` describes only the modern side |
| A9 | HTTP 405 on GET | PASS |

### B. Name headers

| # | Actual | Verdict |
|---|--------|---------|
| B1 | `-32020`, message names both values | PASS |
| B2 | `Tool 検索 not found` (-32602) — the `=?base64?5qSc57Si?=` sentinel decoded server-side, no header mismatch | PASS (validates our `encodeHeaderValue`) |
| B3 | `-32020` "the required Mcp-Name header is absent" | PASS — confirms `Mcp-Name` is mandatory on tools/call, matching `mcpNameForRequest` |

### C. Client end-to-end

| # | Actual | Verdict |
|---|--------|---------|
| C1 | `{"status":"ok"}` via server/discover | PASS |
| C2 | Tools listed; envelope parsed; **found bug D1** (see below) | PASS after fix |
| C3 | Live search call returns content, no error | PASS |
| C4 | Unknown tool → `isError` output, exit 1 | PASS |
| C5 | Missing required param → server validation error surfaced, exit 1 | PASS |
| C6 | `subscriptions/listen --tools` → `{"type":"acknowledged","notifications":{"toolsListChanged":true}}`, stream held open, clean SIGINT close | PASS |
| C7 | Not applicable: the daemon only fronts stdio servers; HTTP connects directly | N/A |

### D. Spec findings and fixes

- **D1 — confirmed bug, fixed.** Spec: an explicit `ttlMs: 0` means the
  result "SHOULD be considered immediately stale"; *absent* means clients
  fall back to their own heuristics; negative clamps to 0. Our
  `toolsListResult.TTLMs` was a plain `int64`, conflating explicit 0 with
  absent — demonstrated live by tampering the cache and watching `mcp tools`
  serve the tampered copy. Fix: `ttlMs` is now `*int64` end to end
  (`toolsListResult` → `listAllTools` → `saveCachedTools` → `ToolCache`);
  `loadCachedTools` treats an explicit `<= 0` hint as immediately stale.
  Stale-tolerant readers (tool help, `mcp schema`, stats, Mcp-Param header
  computation, flag typing) now use the stale loader explicitly, so
  ttlMs=0 servers keep typed flags and custom headers — a genuinely
  outdated header schema is recovered by the HeaderMismatch retry.
- **D2 — confirmed, no change needed.** `Mcp-Name` is required exactly on
  tools/call, prompts/get, and resources/read; the live server enforces
  this with `-32020` (B3) and our client already sends it on exactly those
  methods.
