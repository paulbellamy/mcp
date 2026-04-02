package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
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

// parseInputSchema extracts flat parameters from a JSON Schema inputSchema.
// Complex types (object, array) are skipped. Returns params sorted by name
// and the count of skipped complex properties.
func parseInputSchema(raw json.RawMessage) ([]toolParam, int) {
	if len(raw) == 0 {
		return nil, 0
	}

	var schema struct {
		Type       string                            `json:"type"`
		Properties map[string]map[string]any `json:"properties"`
		Required   []string                          `json:"required"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, 0
	}

	if schema.Properties == nil {
		return nil, 0
	}

	requiredSet := make(map[string]bool, len(schema.Required))
	for _, r := range schema.Required {
		requiredSet[r] = true
	}

	var params []toolParam
	var skipped int
	for name, prop := range schema.Properties {
		typ, _ := prop["type"].(string)
		if typ == "object" || typ == "array" {
			skipped++
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

	return params, skipped
}

// coerceDynamicFlags converts string flag values to proper Go types based on schema.
// Unknown flags (not in schema) are passed through as strings.
func coerceDynamicFlags(flags map[string]string, params []toolParam) (map[string]any, error) {
	paramTypes := make(map[string]string, len(params))
	for _, p := range params {
		paramTypes[p.Name] = p.Type
	}

	result := make(map[string]any, len(flags))
	for k, v := range flags {
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

// getToolSchema looks up a tool's schema from the cache and parses it.
// Returns an error if the tool is not cached.
func getToolSchema(serverName, toolName string) ([]toolParam, error) {
	cached, err := loadCachedTools(serverName)
	if err != nil || cached == nil {
		return nil, fmt.Errorf("no cached schema for %s/%s", serverName, toolName)
	}
	for _, t := range cached {
		if t.Name == toolName {
			params, _ := parseInputSchema(t.InputSchema)
			return params, nil
		}
	}
	return nil, fmt.Errorf("tool %q not found in cache for server %q", toolName, serverName)
}
