package main

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Arbitrary static request headers on an HTTP MCP server. Unlike the
// Mcp-Param-* parameter headers derived from a tool's schema (headers.go),
// these are fixed per-server headers configured at `mcp add` time and sent on
// every request — the mechanism enterprise servers use for a static API key
// plus an org identifier (e.g. Devin: Authorization + X-Org-Id) that the
// OAuth flow does not provide.

// headerEnvRef matches a ${NAME} environment-variable reference in a header
// value. Anything not matching (a bare "$", "${", or "$NAME") is left
// literal, so ordinary values pass through untouched.
var headerEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// parseHeaderFlag parses a curl-style "Name: Value" header flag. The name
// must be a valid HTTP token and is canonicalized; the value is everything
// after the first colon, trimmed of surrounding whitespace. A ${VAR}
// reference in the value is preserved verbatim (resolved later, at request
// time) so secrets can stay in the environment rather than servers.json.
func parseHeaderFlag(s string) (name, value string, err error) {
	idx := strings.IndexByte(s, ':')
	if idx < 0 {
		return "", "", fmt.Errorf("invalid header %q: expected \"Name: Value\"", s)
	}
	name = strings.TrimSpace(s[:idx])
	value = strings.TrimSpace(s[idx+1:])
	if !validHeaderToken(name) {
		return "", "", fmt.Errorf("invalid header name %q: must be a valid HTTP token", name)
	}
	if value == "" {
		return "", "", fmt.Errorf("invalid header %q: value is empty", s)
	}
	if err := validateHeaderValue(value); err != nil {
		return "", "", fmt.Errorf("header %q: %w", name, err)
	}
	return http.CanonicalHeaderKey(name), value, nil
}

// parseHeaderFlags folds a list of raw "Name: Value" flags into a
// canonicalized map. Later occurrences of a header override earlier ones.
func parseHeaderFlags(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	headers := make(map[string]string, len(raw))
	for _, r := range raw {
		name, value, err := parseHeaderFlag(r)
		if err != nil {
			return nil, err
		}
		headers[name] = value
	}
	return headers, nil
}

// splitEnvHeaders splits the MCP_HEADERS env value into individual header
// flags, one "Name: Value" per line. Blank lines are ignored so the variable
// reads naturally in a shell heredoc or a .env file.
func splitEnvHeaders(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, line := range strings.Split(v, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// resolveHeaders expands ${VAR} references and validates each value, yielding
// the concrete headers to put on the wire. A nil/empty input returns a nil
// map (no headers). An unset referenced variable, or a value that is not a
// valid HTTP field value after expansion, is an error — surfaced at connect
// time rather than silently sending a broken header.
func resolveHeaders(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Resolve in a deterministic order so any error is stable across runs.
	names := make([]string, 0, len(raw))
	for k := range raw {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make(map[string]string, len(raw))
	for _, name := range names {
		v, err := expandHeaderEnv(raw[name])
		if err != nil {
			return nil, fmt.Errorf("header %q: %w", name, err)
		}
		if err := validateHeaderValue(v); err != nil {
			return nil, fmt.Errorf("header %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

// expandHeaderEnv replaces every ${VAR} in value with the environment
// variable's value, erroring if any referenced variable is unset.
func expandHeaderEnv(value string) (string, error) {
	var missing []string
	out := headerEnvRef.ReplaceAllStringFunc(value, func(m string) string {
		name := m[2 : len(m)-1] // strip ${ and }
		v, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("references unset environment variable(s): %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// validateHeaderValue rejects values that are not legal HTTP field values:
// control characters other than horizontal tab, and DEL. This mirrors the
// net/http field-value rules so a bad value fails with a clear message
// instead of an opaque transport error (or, worse, header injection).
func validateHeaderValue(v string) error {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b == 0x7f || (b < 0x20 && b != '\t') {
			return fmt.Errorf("value contains control characters")
		}
	}
	return nil
}
