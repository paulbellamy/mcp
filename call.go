package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// errToolFailed is returned when the tool reported an error.
// The JSON output has already been printed; main should exit 1 silently.
var errToolFailed = errors.New("tool returned error")

// defaultMaxOutput caps tool output to stay within LLM token budgets.
const defaultMaxOutput = 30_000

// cmdCall handles the `mcp call <server> <tool> [flags]` command.
// Tool parameters can be passed as individual --flags or via --params JSON.
func cmdCall(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: mcp call <server> <tool> [--<param> <value> ...] [--params '{...}'] [--stream] [--max-output N] [--truncate head|tail]")
	}

	serverName := args[0]
	adhoc := isURL(serverName)

	// `mcp call <server> --help` lists all tools for the server.
	if args[1] == "--help" || args[1] == "-h" {
		return cmdTools([]string{serverName})
	}

	toolName := args[1]
	if err := validateToolName(toolName); err != nil {
		return err
	}
	var paramsStr string
	stream := false
	showHelp := false
	maxOutput := defaultMaxOutput
	truncMode := "head"
	var timeout time.Duration
	timeoutSet := false
	dynamicFlags := make(map[string]string)

	// Parse remaining args: known flags first, then collect dynamic flags.
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--params", "-p":
			if i+1 >= len(args) {
				return fmt.Errorf("--params requires a value")
			}
			i++
			paramsStr = args[i]
		case "--stream":
			stream = true
		case "--max-output":
			if i+1 >= len(args) {
				return fmt.Errorf("--max-output requires a value")
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil {
				return fmt.Errorf("invalid --max-output value: %s", args[i])
			}
			maxOutput = n
		case "--truncate":
			if i+1 >= len(args) {
				return fmt.Errorf("--truncate requires a value (head or tail)")
			}
			i++
			truncMode = args[i]
			if truncMode != "head" && truncMode != "tail" {
				return fmt.Errorf("invalid --truncate value %q (want head or tail)", args[i])
			}
		case "--timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("--timeout requires a value (e.g. 30s, 5m, or 0 for none)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("invalid --timeout value %q: %w", args[i], err)
			}
			if d < 0 {
				return fmt.Errorf("--timeout must be >= 0")
			}
			timeout = d
			timeoutSet = true
		case "--help", "-h":
			showHelp = true
		default:
			if !strings.HasPrefix(args[i], "--") {
				return fmt.Errorf("unexpected argument %q (use --<param> for tool parameters)", args[i])
			}
			key := strings.TrimPrefix(args[i], "--")
			// Support --param=value syntax.
			if eqIdx := strings.IndexByte(key, '='); eqIdx >= 0 {
				dynamicFlags[key[:eqIdx]] = key[eqIdx+1:]
			} else if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				// Boolean flag (no value follows or next arg is also a flag)
				dynamicFlags[key] = "true"
			} else {
				i++
				dynamicFlags[key] = args[i]
			}
		}
	}

	// Handle --help: show tool description and available parameters.
	if showHelp {
		return showToolHelp(serverName, toolName)
	}

	// Reject combining --params with dynamic flags.
	if paramsStr != "" && len(dynamicFlags) > 0 {
		return fmt.Errorf("cannot combine --params with individual parameter flags")
	}

	// If no params from flag, try stdin
	if paramsStr == "" && len(dynamicFlags) == 0 {
		stat, _ := os.Stdin.Stat()
		if (stat.Mode() & os.ModeCharDevice) == 0 {
			const maxStdinSize = 10 << 20 // 10 MB — generous for piped JSON params
			limited := io.LimitReader(os.Stdin, maxStdinSize+1)
			data, err := io.ReadAll(limited)
			if err != nil {
				return fmt.Errorf("read stdin: %w", err)
			}
			if len(data) > maxStdinSize {
				return fmt.Errorf("stdin input exceeds %d bytes", maxStdinSize)
			}
			paramsStr = strings.TrimSpace(string(data))
		}
	}

	// Parse params from JSON or dynamic flags.
	params := make(map[string]any)
	if paramsStr != "" {
		if err := json.Unmarshal([]byte(paramsStr), &params); err != nil {
			return fmt.Errorf("invalid params JSON: %w", err)
		}
	} else if len(dynamicFlags) > 0 {
		if adhoc {
			// No cached schema for ad-hoc URLs — pass all values as strings.
			logStderr("warning: no cached schema; flags passed as strings (array/object params must use --params JSON)")
			for k, v := range dynamicFlags {
				params[k] = v
			}
		} else {
			schema, complexTypes, err := getToolSchema(serverName, toolName)
			if err != nil {
				logStderr("warning: no cached schema for %s/%s; flags passed as strings — array/object params must use --params JSON (run `mcp tools %s --refresh` to cache types)", serverName, toolName, serverName)
				for k, v := range dynamicFlags {
					params[k] = v
				}
			} else {
				coerced, err := coerceDynamicFlags(dynamicFlags, schema, complexTypes)
				if err != nil {
					return err
				}
				params = coerced
			}
		}
	}

	// Resolve server config and auth token.
	server, authToken, err := resolveServer(serverName)
	if err != nil {
		return err
	}

	// Connect
	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	// Apply user-specified timeout to the tool call (initialize used defaults).
	if timeoutSet {
		transport.SetTimeout(timeout)
	}

	// Mirror x-mcp-header-annotated arguments into Mcp-Param-* headers
	// (modern streamable HTTP only; requires a cached schema).
	var extraHeaders map[string]string
	if !adhoc && isModernHTTP(transport) {
		extraHeaders = computeExtraHeaders(serverName, toolName, params)
	}

	// Call tool
	output, err := executeToolCall(transport, toolName, params, stream, extraHeaders)
	if err != nil && !adhoc && isHeaderMismatch(err) {
		// HeaderMismatch (-32020): the server's tool definition no longer
		// matches our cached schema. Refresh the tool list, recompute the
		// headers, and retry exactly once.
		logStderr("warning: server reported a header mismatch; refreshing tool schema and retrying")
		if _, refreshErr := getToolsForServer(server, true); refreshErr != nil {
			logStderr("warning: tool list refresh failed: %v", refreshErr)
			return err
		}
		if isModernHTTP(transport) {
			extraHeaders = computeExtraHeaders(serverName, toolName, params)
		}
		output, err = executeToolCall(transport, toolName, params, stream, extraHeaders)
	}
	if err != nil {
		return err
	}

	// Truncate output to stay within token budgets.
	if maxOutput > 0 && len(output.Content) > maxOutput {
		savedPath := saveFullOutput(serverName, toolName, output.Content)
		output.Content = truncateContent(output.Content, maxOutput, truncMode, savedPath)
		output.Truncated = true
	}

	if err := outputJSON(output); err != nil {
		return err
	}

	// Signal tool error so main() can exit 1 after defers have run
	if output.IsError {
		return errToolFailed
	}

	return nil
}

// sanitizePathComponent replaces characters unsafe for filenames.
var unsafePathChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func sanitizePathComponent(s string) string {
	return unsafePathChars.ReplaceAllString(s, "_")
}

// truncateContent shrinks content to maxOutput bytes, keeping either the head
// (the default — first N bytes) or the tail (last N bytes). The dropped portion
// is replaced with a marker noting how much was cut and, when available, where
// the full output was saved so the rest can be retrieved later.
func truncateContent(content string, maxOutput int, mode, savedPath string) string {
	marker := fmt.Sprintf("[output truncated at %d chars]", maxOutput)
	if savedPath != "" {
		marker += fmt.Sprintf("\n[full output saved to %s]", savedPath)
	}
	if mode == "tail" {
		// Marker first so the agent sees the beginning was dropped.
		return marker + "\n" + content[len(content)-maxOutput:]
	}
	return content[:maxOutput] + "\n" + marker
}

// saveFullOutput writes the full output to a file under the user's config
// directory and returns its path. The config dir is 0700 and inside the
// user's home, so other local users can't pre-plant symlinks the way
// they can in a shared /tmp. We also use O_EXCL + a random suffix so we
// never follow an existing symlink at the target path.
func saveFullOutput(serverName, toolName, content string) string {
	dir := filepath.Join(configDir(), "results")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	pattern := fmt.Sprintf("%d-%s-%s-*.txt", time.Now().Unix(), sanitizePathComponent(serverName), sanitizePathComponent(toolName))
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return ""
	}
	path := f.Name()
	if err := os.Chmod(path, 0600); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return ""
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return ""
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return ""
	}
	return path
}

// maxInputRequiredRetries bounds the multi round-trip request (MRTR) retry
// loop so a server that answers every attempt with input_required cannot
// spin the CLI forever.
const maxInputRequiredRetries = 5

// inputRequiredError renders an input_required result the CLI cannot
// fulfill: the server asked for interactive input (elicitation, sampling, or
// roots) that this client does not declare support for.
func inputRequiredError(requests map[string]inputRequest) callOutput {
	var wants []string
	for id, r := range requests {
		wants = append(wants, fmt.Sprintf("%s (%s)", r.Method, id))
	}
	sort.Strings(wants)
	return callOutput{
		Content: "server requires interactive input this CLI does not support: " + strings.Join(wants, ", "),
		IsError: true,
	}
}

// renderToolCallResult converts a toolCallResult into a callOutput.
func renderToolCallResult(result toolCallResult) callOutput {
	var parts []string
	for _, block := range result.Content {
		switch block.Type {
		case "text":
			parts = append(parts, block.Text)
		case "image":
			parts = append(parts, fmt.Sprintf("[image: %s]", block.MimeType))
		default:
			data, _ := json.Marshal(block)
			parts = append(parts, string(data))
		}
	}
	return callOutput{
		Content: strings.Join(parts, "\n"),
		IsError: result.IsError,
	}
}

// showToolHelp prints help for a specific tool, including its description and parameters.
func showToolHelp(serverName, toolName string) error {
	// Try stale cache first to avoid connecting to the server.
	tools, _ := loadCachedToolsStale(serverName)
	if tools == nil {
		server, err := getServerConfig(serverName)
		if err != nil {
			return err
		}
		tools, err = getToolsForServer(server, false)
		if err != nil {
			return fmt.Errorf("cannot discover tools: %w", err)
		}
	}

	var found *toolOutput
	for _, t := range tools {
		if t.Name == toolName {
			found = &t
			break
		}
	}
	if found == nil {
		return fmt.Errorf("tool %q not found on server %q", toolName, serverName)
	}

	desc := found.Description
	if desc == "" {
		desc = "(no description)"
	}
	fmt.Fprintf(os.Stderr, "%s — %s\n", toolName, desc)
	fmt.Fprintf(os.Stderr, "  server: %s\n", serverName)

	params, complexTypes := parseInputSchema(found.InputSchema)
	skipped := len(complexTypes)
	if len(params) == 0 {
		if skipped > 0 {
			fmt.Fprintf(os.Stderr, "\nNo flag parameters (%d complex parameter(s) must be passed via --params JSON).\n", skipped)
		} else {
			fmt.Fprintln(os.Stderr, "\nNo parameters.")
		}
		return nil
	}

	fmt.Fprintln(os.Stderr, "\nParameters:")

	// Calculate max flag width for alignment.
	maxWidth := 0
	for _, p := range params {
		w := len(p.Name)
		if p.Type != "boolean" {
			w += len(p.Type) + 3 // " <type>"
		}
		if w > maxWidth {
			maxWidth = w
		}
	}

	for _, p := range params {
		flag := "--" + p.Name
		if p.Type != "boolean" {
			flag += " <" + p.Type + ">"
		}

		var annotations []string
		if p.Required {
			annotations = append(annotations, "required")
		}
		if p.Default != nil {
			annotations = append(annotations, fmt.Sprintf("default: %v", p.Default))
		}
		if len(p.Enum) > 0 {
			annotations = append(annotations, fmt.Sprintf("one of: %s", strings.Join(p.Enum, ", ")))
		}

		line := fmt.Sprintf("  %-*s", maxWidth+4, flag)
		if p.Description != "" {
			line += "  " + p.Description
		}
		if len(annotations) > 0 {
			line += " (" + strings.Join(annotations, ", ") + ")"
		}
		fmt.Fprintln(os.Stderr, line)
	}

	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "\n  (%d complex parameter(s) must be passed via --params JSON)\n", skipped)
	}

	return nil
}

// executeToolCall sends a tools/call request and returns the output. Under
// the 2026-07-28 MRTR pattern a server may answer with an input_required
// result; when it carries only opaque requestState the call is retried (as a
// fresh request echoing the state) until it completes or the retry cap hits.
// extraHeaders carries the Mcp-Param-* custom parameter headers (nil when
// none apply); they ride on every attempt, including MRTR retries.
func executeToolCall(transport Transport, toolName string, params map[string]any, stream bool, extraHeaders map[string]string) (callOutput, error) {
	// Always send an arguments object, even when empty. A nil map would
	// marshal to null; an empty map marshals to {} which servers expect.
	if params == nil {
		params = map[string]any{}
	}

	callParams := toolCallParams{
		Name:      toolName,
		Arguments: params,
	}

	for attempt := 0; attempt < maxInputRequiredRetries; attempt++ {
		req := jsonrpcRequest{
			JSONRPC:      jsonrpcVersion,
			ID:           nextID(),
			Method:       "tools/call",
			Params:       callParams,
			ExtraHeaders: extraHeaders,
		}

		var resp jsonrpcResponse
		var err error

		if stream {
			resp, err = transport.SendStreaming(req, func(evt streamEvent) {
				data, _ := json.Marshal(evt)
				_, _ = fmt.Fprintln(os.Stdout, string(data))
			})
		} else {
			resp, err = transport.Send(req)
		}

		if err != nil {
			return callOutput{}, fmt.Errorf("call tool: %w", err)
		}
		if resp.Error != nil {
			// Surface HeaderMismatch as a Go error so cmdCall can refresh
			// the cached schema and retry; every other JSON-RPC error
			// renders as tool output.
			if resp.Error.Code == codeHeaderMismatch {
				return callOutput{}, fmt.Errorf("call tool: %w", resp.Error)
			}
			return callOutput{Content: resp.Error.Message, IsError: true}, nil
		}

		var result toolCallResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return callOutput{}, fmt.Errorf("unmarshal tool result: %w", err)
		}

		switch result.ResultType {
		// Absent means "complete" for servers on earlier protocol revisions.
		case "", resultTypeComplete:
			return renderToolCallResult(result), nil
		case resultTypeInputRequired:
			if len(result.InputRequests) > 0 {
				return inputRequiredError(result.InputRequests), nil
			}
			if result.RequestState == "" {
				return callOutput{}, fmt.Errorf("call tool: input_required result carries neither inputRequests nor requestState")
			}
			// Retry the original request echoing the opaque state verbatim.
			callParams.RequestState = result.RequestState
		default:
			return callOutput{}, fmt.Errorf("call tool: unsupported result type %q", result.ResultType)
		}
	}

	return callOutput{}, fmt.Errorf("call tool: server still required input after %d attempts", maxInputRequiredRetries)
}

// isHeaderMismatch reports whether err carries the spec's HeaderMismatch
// error (-32020), either as a raw JSON-RPC error or inside an HTTP 400 body.
func isHeaderMismatch(err error) bool {
	var rpcErr *jsonrpcError
	if errors.As(err, &rpcErr) {
		return rpcErr.Code == codeHeaderMismatch
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.status == http.StatusBadRequest {
		modernErr := parseModernError(statusErr.body)
		return modernErr != nil && modernErr.Code == codeHeaderMismatch
	}
	return false
}

// computeExtraHeaders derives the Mcp-Param-* headers for a call from the
// tool's cached inputSchema. No cached schema, or an invalid one (possible
// with a cache written before annotation validation existed), yields no
// headers — the server validates the arguments against the body regardless.
func computeExtraHeaders(serverName, toolName string, args map[string]any) map[string]string {
	cached, err := loadCachedTools(serverName)
	if err != nil || cached == nil {
		return nil
	}
	for _, t := range cached {
		if t.Name != toolName {
			continue
		}
		headerParams, err := extractHeaderParams(t.InputSchema)
		if err != nil {
			logStderr("warning: tool %q has an invalid x-mcp-header annotation: %v", toolName, err)
			return nil
		}
		return headerValuesForCall(headerParams, args)
	}
	return nil
}
