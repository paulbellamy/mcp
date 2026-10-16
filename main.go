package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := ensureConfigDirs(); err != nil {
		fatal("config setup: %v", err)
	}

	args := os.Args[1:]
	if os.Getenv("MCP_VERBOSE") != "" {
		verbose = true
	}
	// Global flags go before the subcommand so they can never collide with
	// a tool parameter of the same name in `mcp call`.
	for len(args) > 0 && args[0] == "--verbose" {
		verbose = true
		args = args[1:]
	}
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	cmd := args[0]
	cmdArgs := args[1:]

	var err error
	switch cmd {
	case "list", "ls":
		err = cmdList(cmdArgs)
	case "servers":
		err = cmdServers(cmdArgs)
	case "add":
		err = cmdAdd(cmdArgs)
	case "remove":
		err = cmdRemove(cmdArgs)
	case "tools":
		err = cmdTools(cmdArgs)
	case "schema":
		err = cmdSchema(cmdArgs)
	case "stats":
		err = cmdStats(cmdArgs)
	case "resources":
		err = cmdResources(cmdArgs)
	case "read":
		err = cmdRead(cmdArgs)
	case "call":
		err = cmdCall(cmdArgs)
	case "auth":
		err = cmdAuth(cmdArgs)
	case "ping":
		err = cmdPing(cmdArgs)
	case "listen":
		err = cmdListen(cmdArgs)
	case "daemon":
		err = cmdDaemon(cmdArgs)
	case "enable":
		err = cmdSetEnabled(cmdArgs, true)
	case "disable":
		err = cmdSetEnabled(cmdArgs, false)
	case "auth-callback":
		err = cmdAuthCallback(cmdArgs)
	case "help", "--help", "-h":
		printUsage()
	case "--skill":
		printSkill()
	case "version", "--version", "-v":
		fmt.Println("mcp-cli " + Version)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		printUsage()
		os.Exit(1)
	}

	if err != nil {
		if errors.Is(err, errToolFailed) {
			os.Exit(1)
		}
		fatal("%v", err)
	}
}

func printUsage() {
	fmt.Fprintln(os.Stderr, `Usage: mcp [--verbose] <command> [args...]

Find and call tools on MCP servers. JSON on stdout, logs/warnings on stderr.
Exit 0 = success, 1 = error (including a tool result with isError).

Commands (usual flow: list -> schema -> call):
  list                           Each server and its tool names, one line each
  servers                        List configured servers (full config)
  tools [server|url] [--query q] List tools, compact (--refresh, --full)
  schema <server> <tool>         Show full input schema for one tool
  call <server|url> <tool> ...   Call a tool (flags below)
  resources [server|url]         List readable resources (--query q)
  read <server|url> <uri>        Read a resource by URI (--max-output N)
  add <name> <url> [-H "K: V"].. Add an HTTP server (${VAR} in header values)
  add <name> --stdio <cmd> ...   Add a stdio server
  remove|enable|disable <name>   Manage a configured server
  ping <server|url>              Check the server is reachable and authed
  auth <name>                    Authenticate (idempotent; see mcp auth --help)
  stats [--full]                 Show estimated token cost of tool schemas
  listen <server|url> [flags]    Stream change notifications (mcp listen --help)
  daemon [stop]                  Keep stdio servers warm (foreground)
  help, version

Call flags:
  --<param> <value>              Tool parameter, typed from the cached schema
                                 (arrays/objects as inline JSON)
  --params '{"key":"val"}'       All parameters as JSON (or pipe via stdin)
  --max-output N                 Truncate output to N chars (default 30000,
                                 0 = off); full output is saved to a file
  --truncate head|tail           Keep the head (default) or tail when truncating
  --stream                       Stream progress events as NDJSON
  --timeout <duration>           e.g. 30s, 5m; 0 = none (default 2m, 5m stream)
  --help                         Show tool description and parameters

Ad-hoc URL: pass it in place of <server> (schema needs a configured name; use
"mcp tools <url> --full --query <tool>"). Headers come from the environment:
  MCP_HEADERS                    "Name: Value" lines (${VAR} expanded)
  MCP_AUTH_TOKEN                 Bearer token (also used by mcp auth)

Global flags:
  --verbose                      Log progress to stderr (or set MCP_VERBOSE)
  --skill                        Print the agent skill file and exit

Are you an AI agent? The above covers the usual flow. For when to prefer
native MCP tools, output shapes, auth and truncation recovery: mcp --skill`)
}

// cmdServers handles the `mcp servers` command.
func cmdServers(args []string) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(os.Stderr, `Usage: mcp servers

List configured servers as JSON.`)
			return nil
		}
	}
	servers, err := loadServers()
	if err != nil {
		return err
	}
	return outputJSON(servers)
}

// cmdSetEnabled handles `mcp enable <name>` and `mcp disable <name>`.
func cmdSetEnabled(args []string, enabled bool) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			if enabled {
				fmt.Fprintln(os.Stderr, `Usage: mcp enable <name>

Enable a previously disabled server.`)
			} else {
				fmt.Fprintln(os.Stderr, `Usage: mcp disable <name>

Disable a server without removing it from the config.`)
			}
			return nil
		}
	}
	if len(args) < 1 {
		if enabled {
			return fmt.Errorf("usage: mcp enable <name>")
		}
		return fmt.Errorf("usage: mcp disable <name>")
	}
	name := args[0]
	if err := validateServerName(name); err != nil {
		return err
	}
	servers, err := loadServers()
	if err != nil {
		return err
	}
	found := false
	for i, s := range servers {
		if s.Name == name {
			servers[i].Enabled = &enabled
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("server %q not found", name)
	}
	if err := saveServers(servers); err != nil {
		return err
	}
	if enabled {
		logStderr("enabled server %q", name)
	} else {
		logStderr("disabled server %q", name)
	}
	return nil
}

const addUsage = `Usage: mcp add <name> <url> [--header "Name: Value"]...
       mcp add <name> --stdio <command> [args...]

Add a server to the config. HTTP servers are given a URL; stdio servers are
launched via a local command. Tools are discovered immediately when reachable.

Options (HTTP only):
  --header, -H "Name: Value"   Static header sent on every request (repeatable).
                               Values may reference env vars as ${VAR}, expanded
                               at request time so secrets stay out of the config.`

// cmdAdd handles the `mcp add` command.
func cmdAdd(args []string) error {
	// Everything after --stdio is the child command, taken literally, so its
	// own -H/-h is never mistaken for a flag of `mcp add`.
	var positionals, headerFlags, stdioCmd []string
	stdio := false
	for i := 0; i < len(args); {
		a := args[i]
		switch {
		case a == "--help" || a == "-h":
			fmt.Fprintln(os.Stderr, addUsage)
			return nil
		case a == "--stdio":
			stdio = true
			stdioCmd = args[i+1:]
			i = len(args)
		case a == "--header" || a == "-H":
			if i+1 >= len(args) {
				return fmt.Errorf("--header requires a \"Name: Value\" argument")
			}
			headerFlags = append(headerFlags, args[i+1])
			i += 2
		case strings.HasPrefix(a, "--header="):
			headerFlags = append(headerFlags, strings.TrimPrefix(a, "--header="))
			i++
		case strings.HasPrefix(a, "-H="):
			headerFlags = append(headerFlags, strings.TrimPrefix(a, "-H="))
			i++
		default:
			positionals = append(positionals, a)
			i++
		}
	}

	if len(positionals) < 1 {
		return fmt.Errorf("usage: mcp add <name> <url>  or  mcp add <name> --stdio <command> [args...]")
	}
	name := positionals[0]
	if err := validateServerName(name); err != nil {
		return err
	}

	if stdio {
		if len(headerFlags) > 0 {
			return fmt.Errorf("--header applies to HTTP servers only; a stdio command's own flags (including its own --header) go after --stdio and are passed to it verbatim")
		}
		if len(stdioCmd) < 1 {
			return fmt.Errorf("usage: mcp add <name> --stdio <command> [args...]")
		}
		return addServer(ServerConfig{
			Name:      name,
			Transport: "stdio",
			Command:   stdioCmd[0],
			Args:      stdioCmd[1:],
		}, "")
	}

	// HTTP mode
	if len(positionals) < 2 {
		return fmt.Errorf("usage: mcp add <name> <url> [--header \"Name: Value\" ...]")
	}
	serverURL := positionals[1]
	if err := validateEndpointURL(serverURL, "MCP server"); err != nil {
		return err
	}
	headers, err := parseHeaderFlags(headerFlags)
	if err != nil {
		return err
	}
	authToken, err := getAuthToken(name)
	if err != nil {
		logStderr("warning: auth token load failed: %v", err)
	}
	return addServer(ServerConfig{
		Name:      name,
		Transport: "streamable-http",
		URL:       serverURL,
		Headers:   headers,
	}, authToken)
}

func addServer(server ServerConfig, authToken string) error {
	if err := addServerConfig(server); err != nil {
		return err
	}

	logStderr("connecting to %s...", server.Name)
	tools, ttlMs, err := discoverTools(&server, authToken)
	if err != nil {
		logStderr("warning: could not discover tools: %v", err)
		if server.Transport == "streamable-http" {
			logStderr("hint: run `mcp auth %s` to authenticate, then `mcp tools %s --refresh`", server.Name, server.Name)
		}
		logStderr("added server %q (%s)", server.Name, server.Transport)
		return nil
	}

	if err := saveCachedTools(server.Name, tools, ttlMs); err != nil {
		logStderr("warning: cache write failed: %v", err)
	}
	logStderr("added server %q (%s) — %d tools discovered", server.Name, server.Transport, len(tools))
	return nil
}

// cmdRemove handles the `mcp remove` command.
func cmdRemove(args []string) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(os.Stderr, `Usage: mcp remove <name>

Remove a configured server and its cached tools.`)
			return nil
		}
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: mcp remove <name>")
	}
	name := args[0]
	if err := validateServerName(name); err != nil {
		return err
	}
	if err := removeServerConfig(name); err != nil {
		return err
	}
	logStderr("removed server %q", name)
	return nil
}

// cmdPing handles the `mcp ping <server>` command.
func cmdPing(args []string) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			fmt.Fprintln(os.Stderr, `Usage: mcp ping <server|url>

Liveness check: connect to a server and send a ping. Prints {"status":"ok"}
on success.`)
			return nil
		}
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: mcp ping <server|url>")
	}

	server, authToken, err := resolveServer(args[0])
	if err != nil {
		return err
	}

	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	if err := mcpPing(transport); err != nil {
		return err
	}

	return outputJSON(map[string]string{"status": "ok"})
}

// outputJSON writes a value as JSON to stdout.
func outputJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// logStderr writes a formatted message to stderr.
func logStderr(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
}

// verbose enables progress logging; set by --verbose or MCP_VERBOSE.
var verbose bool

// logVerbose writes a progress message to stderr only when verbose logging
// is enabled. Use it for routine chatter (refreshes, retries, skipped lines)
// that clients merging stderr into a JSON stream must not see by default;
// genuine warnings still go through logStderr unconditionally.
func logVerbose(format string, args ...any) {
	if verbose {
		logStderr(format, args...)
	}
}

// fatal writes an error to stderr and exits.
func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
