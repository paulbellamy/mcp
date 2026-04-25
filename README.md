# mcp

A CLI tool for discovering and calling tools on external [MCP](https://modelcontextprotocol.io/) (Model Context Protocol) servers.

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

# Estimate the token cost of cached schemas across servers
mcp stats [--full]

# Call a tool
mcp call <server> <tool> --params '{"key": "value"}'

# Stream progress for long-running tools
mcp call <server> <tool> --stream --params '{"key": "value"}'

# Authenticate
mcp auth <name> --callback-url <url>

# Use a server without adding it (pass URL directly)
mcp tools https://api.example.com/mcp
mcp call https://api.example.com/mcp <tool> --params '{"key": "value"}'
mcp ping https://api.example.com/mcp

# Authenticate with a token for ad-hoc URLs
MCP_AUTH_TOKEN=<token> mcp call https://api.example.com/mcp <tool> --params '{}'

# Ping / remove
mcp ping <server>
mcp remove <name>
```

## License

MIT
