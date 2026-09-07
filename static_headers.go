package main

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Fixed per-server request headers, distinct from the schema-derived
// Mcp-Param-* headers in headers.go. Configured at `mcp add` time (an
// enterprise static API key + org id the OAuth flow does not provide) and
// sent on every request.

var headerEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// A ${VAR} in the value is left unexpanded (resolved at request time) so a
// secret stays in the environment rather than servers.json.
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

// One "Name: Value" per line so MCP_HEADERS reads naturally in a heredoc.
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

// An unset ${VAR} is an error here (connect time) rather than a silently
// broken header on the wire.
func resolveHeaders(raw map[string]string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Deterministic order so the error on a bad set is stable across runs.
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

func expandHeaderEnv(value string) (string, error) {
	var missing []string
	out := headerEnvRef.ReplaceAllStringFunc(value, func(m string) string {
		name := m[2 : len(m)-1]
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

// Mirrors net/http's field-value rule so a bad (or injected) value fails
// with a clear message, not an opaque transport error. Tab is the one
// control character HTTP permits.
func validateHeaderValue(v string) error {
	for i := 0; i < len(v); i++ {
		b := v[i]
		if b == 0x7f || (b < 0x20 && b != '\t') {
			return fmt.Errorf("value contains control characters")
		}
	}
	return nil
}
