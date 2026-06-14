package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

// toolParam represents a single parameter extracted from a tool's JSON Schema.
type toolParam struct {
	Name        string
	Type        string // "string", "number", "integer", "boolean"
	Description string
	Required    bool
	Default     any
	Enum        []string
}

// parseInputSchema extracts flat (scalar) parameters from a JSON Schema
// inputSchema, plus a map of the array/object-typed properties (name -> type).
// Scalar params can be passed as `--flag value`; complex ones can't, so they're
// returned separately for callers to route to `--params` JSON (and for
// coercion to reject a bare string instead of silently mangling it). Params are
// sorted by name.
func parseInputSchema(raw json.RawMessage) ([]toolParam, map[string]string) {
	if len(raw) == 0 {
		return nil, nil
	}

	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                  `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, nil
	}

	if schema.Properties == nil {
		return nil, nil
	}

	requiredSet := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	var params []toolParam
	complexTypes := map[string]string{}
	for name, prop := range schema.Properties {
		typ, _ := prop["type"].(string)
		if typ == "object" || typ == "array" {
			complexTypes[name] = typ
			continue
		}
		if typ == "" {
			typ = "string"
		}

		p := toolParam{
			Name:     name,
			Type:     typ,
			Required: requiredSet[name],
		}

		if desc, ok := prop["description"].(string); ok {
			p.Description = desc
		}
		if def, ok := prop["default"]; ok {
			p.Default = def
		}
		if enumRaw, ok := prop["enum"].([]any); ok {
			for _, e := range enumRaw {
				if s, ok := e.(string); ok {
					p.Enum = append(p.Enum, s)
				}
			}
		}

		params = append(params, p)
	}

	sort.Slice(params, func(i, j int) bool {
		return params[i].Name < params[j].Name
	})

	return params, complexTypes
}

// coerceDynamicFlags converts string flag values to proper Go types based on
// schema. complexTypes maps array/object param names to their type; values for
// those must be inline JSON (a bare string can't represent them — see
// coerceComplexFlag). Unknown flags (not in schema) are passed through as
// strings.
func coerceDynamicFlags(flags map[string]string, params []toolParam, complexTypes map[string]string) (map[string]any, error) {
	paramTypes := make(map[string]string, len(params))
	for _, p := range params {
		paramTypes[p.Name] = p.Type
	}

	result := make(map[string]any, len(flags))
	for k, v := range flags {
		if ct, ok := complexTypes[k]; ok {
			parsed, err := coerceComplexFlag(k, ct, v)
			if err != nil {
				return nil, err
			}
			result[k] = parsed
			continue
		}
		typ := paramTypes[k]
		switch typ {
		case "number":
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, fmt.Errorf("flag --%s: expected number, got %q", k, v)
			}
			result[k] = f
		case "integer":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fmt.Errorf("flag --%s: expected integer, got %q", k, v)
			}
			result[k] = n
		case "boolean":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("flag --%s: expected boolean, got %q", k, v)
			}
			result[k] = b
		default:
			result[k] = v
		}
	}
	return result, nil
}

// coerceComplexFlag parses the value of a --flag whose schema type is array or
// object. A bare string can't carry these: e.g. sprites `exec` takes its cmd as
// an argv array and whitespace-splits a string into tokens, shredding any
// quoted shell line (`bash -c "a b"` -> [bash, -c, "a, b"]). So require inline
// JSON of the matching kind, and otherwise point the caller at --params rather
// than silently sending a string the server will mangle.
func coerceComplexFlag(name, typ, value string) (any, error) {
	example := `{"k":"v"}`
	if typ == "array" {
		example = `["a","b"]`
	}
	var parsed any
	if err := json.Unmarshal([]byte(value), &parsed); err != nil {
		return nil, fmt.Errorf("flag --%s is %s-typed and needs JSON, not a bare string; pass --%s '%s' or use --params '{...}'", name, typ, name, example)
	}
	switch typ {
	case "array":
		if _, ok := parsed.([]any); !ok {
			return nil, fmt.Errorf("flag --%s expects a JSON array, e.g. --%s '%s'", name, name, example)
		}
	case "object":
		if _, ok := parsed.(map[string]any); !ok {
			return nil, fmt.Errorf("flag --%s expects a JSON object, e.g. --%s '%s'", name, name, example)
		}
	}
	return parsed, nil
}

// getToolSchema looks up a tool's schema from the cache and parses it into its
// scalar params and array/object-typed params (name -> type). Returns an error
// if the tool is not cached.
func getToolSchema(serverName, toolName string) ([]toolParam, map[string]string, error) {
	cached, err := loadCachedTools(serverName)
	if err != nil || cached == nil {
		return nil, nil, fmt.Errorf("no cached schema for %s/%s", serverName, toolName)
	}
	for _, t := range cached {
		if t.Name == toolName {
			params, complexTypes := parseInputSchema(t.InputSchema)
			return params, complexTypes, nil
		}
	}
	return nil, nil, fmt.Errorf("tool %q not found in cache for server %q", toolName, serverName)
}

// cmdSchema handles the `mcp schema <server> <tool>` command.
// Outputs the full schema for a single tool — the on-demand half of
// the lazy schema loading pattern.
func cmdSchema(args []string) error {
	for _, arg := range args {
		if arg == "--help" || arg == "-h" {
			_, _ = fmt.Fprintln(os.Stderr, `Usage: mcp schema <server> <tool>

Print the full JSON schema for a specific tool. Reads from the local cache;
falls back to a live discover if no cache exists. Run
"mcp tools <server> --refresh" to update.`)
			return nil
		}
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: mcp schema <server> <tool>")
	}

	serverName := args[0]
	toolName := args[1]

	if err := validateServerName(serverName); err != nil {
		return err
	}
	if err := validateToolName(toolName); err != nil {
		return err
	}

	if len(args) > 2 {
		extra := args[2]
		if strings.HasPrefix(extra, "-") {
			return fmt.Errorf("unknown flag: %s", extra)
		}
		return fmt.Errorf("unexpected argument: %s", extra)
	}

	tools, err := loadCachedToolsStale(serverName)
	if err != nil {
		return fmt.Errorf("read cache: %w", err)
	}
	if tools == nil {
		server, err := getServerConfig(serverName)
		if err != nil {
			return err
		}
		tools, err = getToolsForServer(server, false)
		if err != nil {
			return fmt.Errorf("discover tools: %w", err)
		}
	}

	for i := range tools {
		if tools[i].Name == toolName {
			return outputJSON(tools[i])
		}
	}
	return fmt.Errorf("tool %q not found on server %q", toolName, serverName)
}
