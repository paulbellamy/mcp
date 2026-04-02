---
name: mcp-cli
description: "Connect to and use external MCP tool servers via the `mcp` CLI."
---

# MCP CLI

Use the `mcp` CLI to discover and call tools on external MCP servers.
All commands run via bash. Output is JSON on stdout, logs on stderr.

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

### Discover tools
```bash
# List all tools (JSON array)
mcp tools

# Filter by server
mcp tools <server>

# Search by keyword
mcp tools --query "search term"

# Force-refresh cached tool list
mcp tools --refresh
```

### Call a tool
```bash
# With inline params (waits for final result)
mcp call <server> <tool> --params '{"key": "value"}'

# With piped params
echo '{"key": "value"}' | mcp call <server> <tool>

# Stream progress for long-running tools (NDJSON to stdout)
mcp call <server> <tool> --stream --params '{"key": "value"}'
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

1. First check what servers are available: `mcp servers`
2. If the user asks to connect a new MCP server: `mcp add <name> <url>`
3. If auth is needed: `mcp auth <name> --callback-url <your-callback-url>` — present the `auth_url` from the JSON output to the user
4. Discover tools: `mcp tools <server>` — review available tools
5. Call tools as needed: `mcp call <server> <tool> --params '{...}'`

## Piping & Chaining

```bash
# Get tool names only
mcp tools server | jq '.[].name'

# Call with result from another command
echo '{"query": "..."}' | mcp call server search

# Chain tool results
mcp call server list --params '{}' | jq '.content' | ...
```

## Notes

- `MCP_CALLBACK_URL` is pre-configured in `~/.bashrc` — no need to pass `--callback-url` manually.
- Tool call results are JSON: `{"content": "...", "isError": false}`
- Exit code 0 = success, 1 = error
- Logs and progress go to stderr, data to stdout
- Token refresh is automatic — if a token is expired, the CLI refreshes it before the call
- Default behavior waits for the final result. Use `--stream` for long-running tools — streams NDJSON progress events to stdout
- For streaming, each line is a JSON object: `{"type":"progress","data":"..."}` or `{"type":"result","content":"...","isError":false}`
