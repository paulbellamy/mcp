# mcp

A CLI tool for discovering and calling tools on external [MCP](https://modelcontextprotocol.io/) (Model Context Protocol) servers.

## When to use this CLI

Most agent harnesses (Claude Code, Cursor, etc.) can connect to MCP servers
natively — every tool on every connected server is loaded into the prompt as a
typed function the model can call directly. That works well when you need a
small, fixed set of tools every turn. It works badly when the catalogue is
large, the server is one-off, or you want to compose tool output with the
shell.

This CLI is the second path. Reach for it when native loading is wrong for the
job.

**Decision rule (one sentence):** if the tool you need is already loaded as
`mcp__<server>__<tool>` in your tool list, call it natively; otherwise use
this CLI.

**Use this CLI when:**
- The server isn't loaded natively (no harness config, ad-hoc URL, dev sandbox).
- The catalogue is large but you only need one or two tools this session — pay
  for one schema, not all of them.
- You need to search across every connected server at once (`mcp tools --query`).
- You want to pipe tool output through `jq`, capture it in a variable, or
  script it.
- You need to see what each server costs in tokens (`mcp stats`).
- Your agent has no native MCP support and shell is the only channel.

**Use native MCP when:**
- The tool is already loaded and you'll call it many times this session — the
  schema-load cost amortizes.
- Args are deeply nested or typed — the model emits them directly, no JSON
  escaping.
- You're inside a tight loop where the extra `mcp tools` / `mcp schema`
  round-trips matter.

**What this CLI gives you that native loading doesn't:**
- **Lazy schema loading.** `mcp tools` returns name+description only. Schemas
  (often 10k+ tokens per server) are fetched one tool at a time, on demand,
  via `mcp schema`. Native loading pays the full schema tax every turn whether
  you call the tool or not.
- **Ad-hoc URLs.** `mcp tools https://api.example.com/mcp` works without
  registering the server. Same for `call`, `ping`. Token auth via
  `MCP_AUTH_TOKEN`.
- **Cross-server search.** `mcp tools --query foo` ranks tools by keyword
  across every connected server. Native has no equivalent.
- **Resource access.** `mcp resources` lists a server's readable resources and
  templates; `mcp read <uri>` fetches their contents. Useful for servers (like
  Notion) that expose data as resources rather than tools.
- **Token visibility.** `mcp stats` (and `--full`) tells you exactly what
  each server's schemas would cost if loaded natively.
- **Streaming.** `mcp call --stream` emits NDJSON progress events for
  long-running tools.
- **Shell composability.** Output is JSON on stdout, logs on stderr, exit
  codes for success/failure. Pipe, capture, chain at will.

**Tradeoffs to know:**
- More turns per call (list → schema → call) than a native one-shot invocation.
- Args are passed as a JSON string — escaping matters.
- Schema cache can drift; use `--refresh` if a server changed.
- Stdio servers have a cold-start cost on first call (use `mcp daemon` to keep
  them warm).

## Install

### Claude Code plugin

```bash
claude plugin marketplace add https://github.com/paulbellamy/mcp
claude plugin install mcp-cli
```

### Binary

```bash
curl -fsSL https://raw.githubusercontent.com/paulbellamy/mcp/master/scripts/install.sh | sh
```

### From source

```bash
go install github.com/paulbellamy/mcp@latest
```

## Usage

```bash
# List configured servers
mcp servers

# Add a server (HTTP or stdio)
mcp add <name> <url>
mcp add <name> --stdio <command> [args...]

# Discover tools (compact summaries — name + description only)
mcp tools [server] [--query "search term"] [--refresh]

# Include full inputSchema for every tool (large; usually not needed)
mcp tools --full

# Fetch the full schema for one tool on demand
mcp schema <server> <tool>

# List readable resources (and resource templates) — name + uri + description
mcp resources [server] [--query "search term"]

# Read a resource's contents by URI
mcp read <server> <uri>

# Estimate the token cost of cached schemas across servers
mcp stats [--full]

# Call a tool
mcp call <server> <tool> --params '{"key": "value"}'

# Scalar params can also be passed as individual flags (coerced via the cached
# schema). Array/object params can't be a bare string — pass inline JSON for the
# flag, or use --params:
mcp call <server> <tool> --query hello --limit 10
mcp call <server> <tool> --cmd '["bash","-lc","cd /app && npm run build"]'

# Stream progress for long-running tools
mcp call <server> <tool> --stream --params '{"key": "value"}'

# Override the per-call timeout (default: 2m plain / 5m streaming; 0 = no limit)
mcp call <server> <tool> --timeout 10m --params '{"key": "value"}'

# Authenticate
mcp auth <name> --callback-url <url>

# Use a server without adding it (pass URL directly)
mcp tools https://api.example.com/mcp
mcp call https://api.example.com/mcp <tool> --params '{"key": "value"}'
mcp resources https://api.example.com/mcp
mcp read https://api.example.com/mcp <uri>
mcp ping https://api.example.com/mcp

# Authenticate with a token for ad-hoc URLs
MCP_AUTH_TOKEN=<token> mcp call https://api.example.com/mcp <tool> --params '{}'

# Ping / remove
mcp ping <server>
mcp remove <name>
```

## License

MIT
