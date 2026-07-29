package main

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Custom parameter headers (2026-07-28 streamable HTTP transport): tool
// inputSchema properties annotated with "x-mcp-header": "<Name>" are mirrored
// into Mcp-Param-<Name> request headers on tools/call. Tools whose
// annotations violate the spec must be rejected as a whole.

// headerParam is one valid x-mcp-header annotation: the chain of properties
// keys leading to the annotated property, and the header name suffix.
type headerParam struct {
	Path []string
	Name string
}

// mcpParamPrefix prefixes every custom parameter header name.
const mcpParamPrefix = "Mcp-Param-"

// maxHeaderInt bounds integer header values per spec (±(2^53−1)) so the
// decimal form round-trips exactly through IEEE 754 doubles server-side.
const maxHeaderInt = int64(1)<<53 - 1

// headerTcharPunct is the RFC 9110 tchar set minus ALPHA / DIGIT.
const headerTcharPunct = "!#$%&'*+-.^_`|~"

// validHeaderToken reports whether s is a non-empty RFC 9110 token
// (1*tchar). tchar excludes CR/LF, control characters, and separators.
func validHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		default:
			if strings.IndexByte(headerTcharPunct, b) < 0 {
				return false
			}
		}
	}
	return true
}

// headerDataKeywords hold instance data, not subschemas; an "x-mcp-header"
// key inside them is a value, not an annotation.
var headerDataKeywords = map[string]bool{
	"default":  true,
	"examples": true,
	"enum":     true,
	"const":    true,
}

// extractHeaderParams walks a tool's inputSchema for x-mcp-header
// annotations and returns the valid ones sorted by header name. A non-nil
// error means the whole tool definition is invalid per the custom-headers
// spec and must be rejected.
func extractHeaderParams(inputSchema json.RawMessage) ([]headerParam, error) {
	if len(inputSchema) == 0 {
		return nil, nil
	}
	var schema map[string]any
	if err := json.Unmarshal(inputSchema, &schema); err != nil {
		// Not an object schema — nothing to extract, matching the tolerant
		// schema parsing elsewhere in the CLI.
		return nil, nil
	}

	var params []headerParam
	seen := make(map[string]string) // lower-cased header name -> property
	if err := walkHeaderSchema(schema, nil, true, seen, &params); err != nil {
		return nil, err
	}
	sort.Slice(params, func(i, j int) bool { return params[i].Name < params[j].Name })
	return params, nil
}

// walkHeaderSchema validates and collects annotations on one schema node.
// onChain is true only when the node was reached from the root exclusively
// through properties keys — the static reachability the spec requires.
// Annotations found anywhere else (items, composition keywords,
// if/then/else, $ref targets, ...) invalidate the tool.
func walkHeaderSchema(node map[string]any, path []string, onChain bool, seen map[string]string, out *[]headerParam) error {
	if raw, ok := node["x-mcp-header"]; ok {
		if !onChain || len(path) == 0 {
			return fmt.Errorf("x-mcp-header annotation is only allowed on properties statically reachable through properties keys")
		}
		prop := strings.Join(path, ".")
		name, ok := raw.(string)
		if !ok {
			return fmt.Errorf("property %q: x-mcp-header must be a string", prop)
		}
		if !validHeaderToken(name) {
			return fmt.Errorf("property %q: x-mcp-header %q is not a valid RFC 9110 token", prop, name)
		}
		typ, _ := node["type"].(string)
		switch typ {
		case "string", "integer", "boolean":
		default:
			return fmt.Errorf("property %q: x-mcp-header requires type string, integer, or boolean (got %q)", prop, typ)
		}
		lower := strings.ToLower(name)
		if prev, dup := seen[lower]; dup {
			return fmt.Errorf("property %q: x-mcp-header %q duplicates the header on property %q (names are case-insensitive)", prop, name, prev)
		}
		seen[lower] = prop
		*out = append(*out, headerParam{Path: append([]string(nil), path...), Name: name})
	}

	for key, val := range node {
		if key == "x-mcp-header" || headerDataKeywords[key] {
			continue
		}
		// A properties map's keys are property NAMES, never schema keywords,
		// so its children must always be walked as schema nodes — on-chain
		// children extend the path, off-chain children stay unreachable.
		// Passing the map itself to walkHeaderSchema would misread property
		// names like "enum" (silently skipped) or "x-mcp-header" (falsely
		// treated as an annotation).
		if key == "properties" {
			props, ok := val.(map[string]any)
			if !ok {
				continue
			}
			for propName, propVal := range props {
				child, ok := propVal.(map[string]any)
				if !ok {
					continue
				}
				var childPath []string
				if onChain {
					// Copy the path: append on a shared backing array would
					// alias sibling walks.
					childPath = append(append([]string(nil), path...), propName)
				}
				if err := walkHeaderSchema(child, childPath, onChain, seen, out); err != nil {
					return err
				}
			}
			continue
		}
		// Every other keyword breaks static reachability.
		if err := walkHeaderContent(val, seen, out); err != nil {
			return err
		}
	}
	return nil
}

// walkHeaderContent scans arbitrary schema content that is not statically
// reachable through properties chains; any x-mcp-header found inside it is
// an invalid annotation.
func walkHeaderContent(v any, seen map[string]string, out *[]headerParam) error {
	switch vv := v.(type) {
	case map[string]any:
		return walkHeaderSchema(vv, nil, false, seen, out)
	case []any:
		for _, item := range vv {
			if err := walkHeaderContent(item, seen, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// headerValuesForCall derives the Mcp-Param-* headers for one tools/call.
// Values that cannot be encoded per the spec (absent, null, non-primitive,
// integers beyond ±(2^53−1)) omit the header — the server validates the
// arguments against the body regardless.
func headerValuesForCall(params []headerParam, args map[string]any) map[string]string {
	var headers map[string]string
	for _, p := range params {
		v, ok := lookupArg(args, p.Path)
		if !ok {
			continue
		}
		s, ok := encodeHeaderParamValue(v)
		if !ok {
			continue
		}
		if headers == nil {
			headers = make(map[string]string)
		}
		headers[mcpParamPrefix+p.Name] = encodeHeaderValue(s)
	}
	return headers
}

// lookupArg resolves a properties-chain path against the call arguments.
func lookupArg(args map[string]any, path []string) (any, bool) {
	var cur any = args
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// encodeHeaderParamValue renders a primitive argument value in its
// spec-defined header form: string as-is, integer as decimal, boolean
// lowercase. ok is false for values with no valid header form.
func encodeHeaderParamValue(v any) (string, bool) {
	switch vv := v.(type) {
	case string:
		return vv, true
	case bool:
		return strconv.FormatBool(vv), true
	case int:
		return encodeHeaderInt(int64(vv))
	case int64:
		return encodeHeaderInt(vv)
	case float64:
		// JSON numbers decode as float64; only integral values within the
		// safe range have an exact decimal form.
		if vv != math.Trunc(vv) || vv > float64(maxHeaderInt) || vv < -float64(maxHeaderInt) {
			return "", false
		}
		return strconv.FormatInt(int64(vv), 10), true
	}
	return "", false
}

func encodeHeaderInt(n int64) (string, bool) {
	if n > maxHeaderInt || n < -maxHeaderInt {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}
