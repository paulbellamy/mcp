---
name: mcp-cli
description: "Discover and call MCP tools via the `mcp` CLI. Use when the needed tool is not exposed as a native `mcp__<server>__<tool>` in your tool list, when given an ad-hoc server URL, when you need to search across servers, or when context budget rules out loading a large catalogue natively."
---

# MCP CLI

If the tool you need is already loaded natively as `mcp__<server>__<tool>`,
call it natively. Otherwise use `mcp` from bash: servers that aren't loaded,
ad-hoc URLs, cross-server search, one-off calls, piping into other commands.

Data is JSON on stdout; logs and warnings go to stderr (one broken server only
warns, the rest still list). Exit 0 = success, 1 = error or tool `isError`.

## Workflow

```bash
mcp list                            # one line per server: tool names or error
mcp tools [server] [--query word]   # [{"server","name","description"}]
mcp schema <server> <tool>          # full inputSchema for one tool
mcp call <server> <tool> --key value --limit 10   # flags typed from schema
mcp call <server> <tool> --params '{"ids":[1,2],"opts":{"a":true}}'
```
- Skip `schema` when the tool has no params or you already know them.
- Array/object params: inline JSON as the flag value (`--tags '["a","b"]'`),
  or put everything in `--params` (or pipe JSON on stdin). Don't mix the two.
- Result: `{"content":"<text>","isError":false}`; `content` is a string,
  often JSON-encoded (`jq -r .content` unwraps it).
- `mcp tools --refresh` re-fetches cached lists; `--full` adds every schema
  (expensive). `mcp call <server> <tool> --help` lists params from the cache.

## Ad-hoc URLs

Pass the URL in place of the server name (`tools`, `call`, `ping`,
`resources`, `read`). Headers come from `MCP_HEADERS`, one `Name: Value` per
line (these commands have no `-H`); `MCP_AUTH_TOKEN` sends a bearer token.
```bash
export MCP_HEADERS="X-Api-Key: $KEY"
mcp tools https://host/mcp
mcp tools https://host/mcp --full --query <tool>  # schema needs a name, so this
mcp call https://host/mcp <tool> --params '{...}' # flags are sent as strings
```

## Configure servers

```bash
mcp add <name> <url> [-H "Name: Value"]...  # ${VAR} expands per request
mcp add <name> --stdio <command> [args...]  # rest goes to the command as-is
mcp remove <name>                           # also enable/disable <name>
mcp ping <name|url>     # {"status":"ok"}; exit 1 if unreachable or unauthed
mcp auth <name>         # idempotent; show auth_url only if "status":"pending"
MCP_AUTH_TOKEN=<token> mcp auth <name>      # save a bearer token, no OAuth
```
`--callback-url` defaults to `$MCP_CALLBACK_URL`.

## Large output

Output over 30000 chars is cut: the JSON gets `"truncated":true` and a
`[full output saved to <path>]` line. Read that file (grep/sed/tail) instead
of re-calling. `--max-output N` sets the cap (0 = off); on `call`,
`--truncate tail` keeps the end instead of the start.

## Resources

```bash
mcp resources [server|url] [--query word]  # templates: fill {x} in uriTemplate
mcp read <server|url> <uri>  # {"contents":[{"uri","mimeType","text"|"blob"}]}
```

## Other

`--stream` prints `{"type":"progress","data":...}` lines before the result;
`--timeout 5m` (default 2m); `mcp stats [--full]` estimates schema token cost.
