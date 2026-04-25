---
name: mcp-cli
description: "Connect to and use external MCP tool servers via the `mcp` CLI."
---

# MCP CLI

Use the `mcp` CLI to discover and call tools on external MCP servers.
All commands run via bash. Output is JSON on stdout, logs on stderr.

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
mcp schema <server> <tool>
```

### Inspect token cost
```bash
# Estimate how many tokens each server's schemas would consume.
# Shows the savings of compact summaries vs full schemas.
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

# Show parameters for a single tool (reads from cache)
mcp call <server> <tool> --help
```

### Authenticate
```bash
# OAuth (outputs auth URL for user)
mcp auth <name> --callback-url <your-callback-url>

# Manual token
MCP_AUTH_TOKEN=<bearer-token> mcp auth <name>
```

### Ping a server
```bash
mcp ping <server>
```

### Remove a server
```bash
mcp remove <name>
```

## Workflow

1. Check what servers are available: `mcp servers`
2. If the user asks to connect a new MCP server: `mcp add <name> <url>`
3. If auth is needed: `mcp auth <name> --callback-url <your-callback-url>` — present the `auth_url` from the JSON output to the user
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
- Exit code 0 = success, 1 = error
- Logs and progress go to stderr, data to stdout
- Token refresh is automatic — if a token is expired, the CLI refreshes it before the call
- Default behavior waits for the final result. Use `--stream` for long-running tools — streams NDJSON progress events to stdout
- For streaming, each line is a JSON object: `{"type":"progress","data":"..."}` or `{"type":"result","content":"...","isError":false}`
