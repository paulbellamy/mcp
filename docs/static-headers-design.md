# Design: arbitrary static request headers

Motivation: enterprise MCP servers often authenticate with a fixed API key
plus an org identifier carried in HTTP headers, not via OAuth. Devin's
enterprise MCP is the concrete case — it requires both:

```json
"headers": {
  "Authorization": "Bearer <API_KEY>",
  "X-Org-Id": "<YOUR_ORG_ID>"
}
```

Before this feature the CLI could only supply `Authorization` (OAuth token or
`MCP_AUTH_TOKEN`); there was no way to send `X-Org-Id` or any other header, so
such servers were unreachable. This adds arbitrary, per-server static headers.

Not to be confused with `headers.go`: those are the protocol's `Mcp-Param-*`
headers *derived from tool arguments* on a per-call basis. This feature is
*fixed* headers configured on the server and sent on every request.

## CLI surface

```
mcp add <name> <url> [--header "Name: Value"]...    # -H is an alias
```

- `--header`/`-H` is repeatable and curl-style (`"Name: Value"`; the value is
  everything after the first colon, so colons in the value are fine).
- Names must be valid HTTP tokens and are canonicalized; later duplicates
  override earlier ones.
- To change headers, re-run `mcp add` (an upsert) with the new flags.

### stdio servers

A stdio server has no HTTP layer, so `--header` is not an `mcp add` option for
it. But a stdio *command* may define its own header flag — that is just one of
its arguments: everything after `--stdio` is the child's argv, passed verbatim,
so `mcp add w --stdio some-cli --header "X: y"` gives `some-cli` the
`--header "X: y"` flag. `--header` placed *before* `--stdio` is rejected with a
message pointing at the correct placement (it would otherwise be silently
dropped, since there is no HTTP request to attach it to).

### ad-hoc URLs

Ad-hoc URL commands (`mcp tools <url>`, `call`, `resources`, `read`, `ping`,
`listen`) have no config entry, so a `--header` flag has nowhere to live — and
on `mcp call` it would collide with the `--<param>` tool-argument flags. Static
headers for ad-hoc URLs therefore come from the `MCP_HEADERS` environment
variable (one `Name: Value` per line), the counterpart to `MCP_AUTH_TOKEN`.
It is parsed in `resolveServer`'s URL branch through the same
`parseHeaderFlags`/`resolveHeaders` pipeline, so `${VAR}` expansion and
validation behave identically to configured headers, and every ad-hoc command
picks them up for free.

## Environment interpolation

A value may contain `${VAR}` references, expanded from the environment **at
request time**, not at `add` time. So secrets live in the environment and only
the reference (`Bearer ${DEVIN_API_KEY}`) is written to `servers.json`. An
unset referenced variable is a hard error surfaced when connecting, rather than
silently sending a broken header. Literal values (no `${...}`) are stored and
sent as-is; a bare `$` or `$NAME` without braces is left literal.

## Storage & precedence

- `ServerConfig.Headers map[string]string` (`json:"headers,omitempty"`),
  persisted in `servers.json` (mode 0600). Absent when no headers configured,
  so existing configs and output are unchanged.
- Applied in `HTTPTransport` before the protocol-critical headers
  (`Content-Type`, `Accept`, `MCP-Protocol-Version`, `Mcp-Method`,
  `Mcp-Name`, `Mcp-Param-*`), so a stray configured header can never clobber
  the transport framing.
- `Authorization`: a configured value stands only when there is **no** OAuth
  token; a present token wins. Enterprise static-key servers have no token, so
  the configured `Authorization` is used.
- Because headers live on `ServerConfig` and are resolved in `mcpConnectOpts`,
  every command that connects (`tools`, `call`, `resources`, `read`, `ping`,
  `listen`, discovery) sends them automatically. Stdio and daemon transports
  ignore them.

## Implementation

- `config.go`: `ServerConfig.Headers`.
- `static_headers.go` (new): `parseHeaderFlag`/`parseHeaderFlags` (flag →
  canonical map, reusing `validHeaderToken`), `resolveHeaders` (env expansion +
  field-value validation), `expandHeaderEnv`, `validateHeaderValue`,
  `splitEnvHeaders` (MCP_HEADERS lines → flags).
- `config.go` `resolveServer`: for an ad-hoc URL, parse `MCP_HEADERS` into the
  synthesized `ServerConfig.Headers`.
- `transport.go`: `HTTPTransport.headers`, `setHeaders`, `applyStaticHeaders`;
  applied in `sendWithContext`, `Notify`, and `Close` (DELETE).
- `tools.go` `mcpConnectOpts`: resolve `server.Headers` and set them on the
  HTTP transport; a resolution error fails the connect.
- `main.go` `cmdAdd`: parse `--header`/`-H` up to an optional `--stdio`
  boundary (tokens after `--stdio` are the child command, taken literally).

## Tests (`static_headers_test.go`)

- Flag parsing: canonicalization, colon-in-value, missing colon, empty value,
  bad token, control chars; last-wins dedup.
- Resolution: literal passthrough, `${VAR}` expansion, unset-var error, nil in
  → nil out; `$`/`$NAME` left literal.
- Transport: static headers on the wire; configured `Authorization` used with
  no token, OAuth token overrides it; headers on `Notify`.
- End-to-end `cmd add`: raw `${VAR}` persisted with canonical names, server
  observes the expanded values through the full connect path; `--stdio`
  rejection; unset-var surfaces as a connect error.
