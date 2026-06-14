---
name: mcp-cli
description: "Discover and call MCP tools via the `mcp` CLI. Use when the needed tool is not exposed as a native `mcp__<server>__<tool>` in your tool list, when given an ad-hoc server URL, when you need to search across servers, or when context budget rules out loading a large catalogue natively."
---

# MCP CLI

Use the `mcp` CLI to discover and call tools on external MCP servers.
All commands run via bash. Output is JSON on stdout, logs on stderr.

## When to use this skill

**Decision rule:** if the tool you need is already loaded as
`mcp__<server>__<tool>` in your tool list, call it natively. Otherwise, use
this CLI.

**Available servers:**

!`mcp servers`

**Use this skill when:**
- The tool you need is not in your native tool list (server isn't loaded, or
  the user gave you an ad-hoc URL).
- The user pasted/named a server URL — call it with `mcp tools <url>` rather
  than asking them to register it.
- The native catalogue would be huge but you only need one or two tools — pay
  for one schema via `mcp schema`, not the entire catalogue's tax every turn.
- You need to search across every connected server (`mcp tools --query`).
- You want to pipe tool output through `jq`, capture it, or chain calls.
- You need token-cost visibility (`mcp stats`).

**Skip this skill when:**
- The tool is already loaded as `mcp__<server>__<tool>` and you'll call it
  more than once or twice this session — native is faster (no schema fetch,
  no JSON-string escaping).
- Args are deeply nested or strongly typed and JSON-escaping is brittle.

If both paths are open, prefer native for repeat calls and prefer this CLI
for one-shot, ad-hoc, or multi-server exploration.

## Lazy Schema Loading

Tool schemas are large — a single MCP server can produce tens of thousands of
tokens of JSON Schema. To keep prompts cheap, this CLI uses a two-phase pattern:

1. `mcp tools` returns **compact summaries** (name + description only).
2. `mcp schema <server> <tool>` returns the **full schema** for one tool, on demand.

When deciding which tool to call, list with `mcp tools` (cheap), then fetch the
schema for the chosen tool with `mcp schema` before constructing the call.

## Quick Reference

### List configured servers
```bash
mcp servers
```

### Add a server
```bash
# HTTP server
mcp add <name> <url>

# Stdio server (spawns a local process)
mcp add <name> --stdio <command> [args...]
```

### Discover tools (compact summaries)
```bash
# All tools across all servers — name + description only
mcp tools

# Filter by server
mcp tools <server>

# Search by keyword
mcp tools --query "search term"

# Force-refresh cached tool list
mcp tools --refresh

# Include full inputSchema for every tool (rarely needed; expensive)
mcp tools --full
```

### Fetch the full schema for a tool
```bash
# Returns name, description, and the full JSON Schema for one tool.
# Use this once you've decided which tool to call.
# Reads from the local cache; falls back to a live discover if no cache exists.
mcp schema <server> <tool>
```

### Inspect token cost
```bash
# Estimate how many tokens each server's schemas would consume.
# Shows the savings of compact summaries vs full schemas.
# Cache-only — run `mcp tools <server> --refresh` first if a server is empty.
mcp stats

# Per-tool breakdown
mcp stats --full
```

### Call a tool
```bash
# With inline params (waits for final result)
mcp call <server> <tool> --params '{"key": "value"}'

# With piped params
echo '{"key": "value"}' | mcp call <server> <tool>

# With individual flags (typed from the cached schema)
mcp call <server> <tool> --query "hello" --limit 10

# Stream progress for long-running tools (NDJSON to stdout)
mcp call <server> <tool> --stream --params '{"key": "value"}'

# Show parameters for a single tool (reads from cache; live-fetches if missing)
mcp call <server> <tool> --help
```

### Read resources
Some servers (e.g. Notion) expose data as **resources** — addressable by URI —
rather than (or in addition to) tools. List them, then read one by URI.
```bash
# List readable resources and resource templates across servers
mcp resources

# Filter by server
mcp resources <server>

# Search by keyword (uri/name/description)
mcp resources <server> --query "search term"

# Read a resource's contents by URI (output: {"contents":[{"uri","mimeType","text"|"blob"}]})
mcp read <server> <uri>

# Truncate large resources (default 30000 chars; full output saved to a file)
mcp read <server> <uri> --max-output 5000
```
Resource listings are fetched live (not cached). Templates appear with a
`uriTemplate` (e.g. `notion://page/{id}`) — substitute the parameters to form a
concrete URI before reading.

### Authenticate
```bash
# OAuth (outputs auth URL for user)
mcp auth <name> --callback-url <your-callback-url>

# Manual token
MCP_AUTH_TOKEN=<bearer-token> mcp auth <name>
```
`mcp auth` is idempotent: if the server is already connected it's a no-op that
returns `{"status":"complete"}` immediately, without starting a new OAuth flow
or handing back a fresh `auth_url`. So it's safe to run without checking first —
you'll only get back an `auth_url` (with `"status":"pending"`) when the server
actually needs authenticating.

### Check if a server is connected
Use `mcp ping` to test whether a server is reachable **and** your stored
credentials are accepted — it opens a real session with the saved token, so a
missing or invalid token makes it fail.
```bash
mcp ping <server>
```
- Connected: prints `{"status":"ok"}` and exits 0.
- Not connected (unreachable, or unauthenticated/expired auth): exits 1 with the
  error on stderr — run `mcp auth <server>` to authenticate.

### Remove a server
```bash
mcp remove <name>
```

## Workflow

If the tool you need is already exposed as `mcp__<server>__<tool>` in your
tool list, call it natively and skip the rest of this skill.

1. Check what servers are available: `mcp servers`
2. If the user asks to connect a new MCP server: `mcp add <name> <url>`
3. If auth is needed: `mcp auth <name> --callback-url <your-callback-url>` — it's
   idempotent (no-op if already connected), so it's safe to run. Only present an
   `auth_url` to the user when the output has `"status":"pending"`; a
   `"status":"complete"` means you're already connected. To check connection
   status without authenticating, use `mcp ping <name>`.
4. Discover tools (compact): `mcp tools <server>` — pick the tool you need
5. Fetch the schema for that tool: `mcp schema <server> <tool>`
6. Call it: `mcp call <server> <tool> --params '{...}'`

Skip step 5 only when:
- The tool has no parameters, or
- You already know the schema from a previous turn, or
- You're using `mcp call ... --help`, which prints parameters from the cache.

## Piping & Chaining

```bash
# Get tool names only
mcp tools <server> | jq '.[].name'

# Find tools matching a keyword and grab the first one's schema
TOOL=$(mcp tools <server> --query "search" | jq -r '.[0].name')
mcp schema <server> "$TOOL"

# Call with result from another command
echo '{"query": "..."}' | mcp call <server> search

# Chain tool results
mcp call <server> list --params '{}' | jq '.content' | ...
```

## Notes

- `MCP_CALLBACK_URL` is pre-configured in `~/.bashrc` — no need to pass `--callback-url` manually.
- `mcp tools` JSON output omits `inputSchema` by default. Use `mcp schema` to fetch one, or `mcp tools --full` for everything (expensive).
- Tool call results are JSON: `{"content": "...", "isError": false}`
- Resource reads are JSON: `{"contents": [{"uri": "...", "mimeType": "...", "text": "..."}]}` (binary resources use `blob` with base64 instead of `text`)
- Output is truncated to 30000 chars by default to protect the context window; when this happens you get `"truncated": true`, an `[output truncated at N chars]` marker, and an `[full output saved to <path>]` line so you can read the rest. Tune with `--max-output N` (0 disables). For `mcp call`, keep the end instead of the start with `--truncate tail` (default is `head`).
- Exit code 0 = success, 1 = error
- Logs and progress go to stderr, data to stdout
- Token refresh is automatic — if a token is expired, the CLI refreshes it before the call
- Default behavior waits for the final result. Use `--stream` for long-running tools — streams NDJSON progress events to stdout
- For streaming, each line is a JSON object: `{"type":"progress","data":"..."}` or `{"type":"result","content":"...","isError":false}`
