package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
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
		return fmt.Errorf("usage: mcp call <server> <tool> [--<param> <value> ...] [--params '{...}'] [--stream] [--max-output N]")
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
			logStderr("warning: no cached schema; flags will be passed as strings")
			for k, v := range dynamicFlags {
				params[k] = v
			}
		} else {
			schema, err := getToolSchema(serverName, toolName)
			if err != nil {
				logStderr("warning: no cached schema for %s/%s; flags will be passed as strings (run `mcp tools %s --refresh` to update)", serverName, toolName, serverName)
				for k, v := range dynamicFlags {
					params[k] = v
				}
			} else {
				coerced, err := coerceDynamicFlags(dynamicFlags, schema)
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

	// Call tool
	output, err := executeToolCall(transport, toolName, params, stream)
	if err != nil {
		return err
	}

	// Truncate output to stay within token budgets.
	if maxOutput > 0 && len(output.Content) > maxOutput {
		savedPath := saveFullOutput(serverName, toolName, output.Content)
		output.Content = output.Content[:maxOutput] + fmt.Sprintf("\n[output truncated at %d chars]", maxOutput)
		if savedPath != "" {
			output.Content += fmt.Sprintf("\n[full output saved to %s]", savedPath)
		}
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

// saveFullOutput writes the full output to a temp file and returns its path.
func saveFullOutput(serverName, toolName, content string) string {
	dir := filepath.Join(os.TempDir(), "mcp-results")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ""
	}
	name := fmt.Sprintf("%d-%s-%s.txt", time.Now().Unix(), sanitizePathComponent(serverName), sanitizePathComponent(toolName))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return ""
	}
	return path
}

// handleToolResponse converts a JSON-RPC response into a callOutput.
func handleToolResponse(resp jsonrpcResponse) (callOutput, error) {
	if resp.Error != nil {
		return callOutput{Content: resp.Error.Message, IsError: true}, nil
	}

	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return callOutput{}, fmt.Errorf("unmarshal tool result: %w", err)
	}

	return renderToolCallResult(result), nil
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

	params, skipped := parseInputSchema(found.InputSchema)
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

// executeToolCall sends a tools/call request and returns the output.
func executeToolCall(transport Transport, toolName string, params map[string]any, stream bool) (callOutput, error) {
	req := jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "tools/call",
		Params: toolCallParams{
			Name:      toolName,
			Arguments: params,
		},
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

	return handleToolResponse(resp)
}
