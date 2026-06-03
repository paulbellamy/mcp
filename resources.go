package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func discoverResources(server *ServerConfig, authToken string) ([]resourceOutput, error) {
	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transport.Close() }()

	resources, err := listAllResources(transport, server.Name)
	if err != nil {
		return nil, err
	}

	// Templates are optional; their absence must not fail the whole listing.
	templates, err := listAllResourceTemplates(transport, server.Name)
	if err != nil {
		logStderr("warning: failed to list resource templates from %q: %v", server.Name, err)
	}

	return append(resources, templates...), nil
}

func listAllResources(transport Transport, serverName string) ([]resourceOutput, error) {
	var all []resourceOutput
	var cursor string
	const maxPages = 100

	for page := 0; page < maxPages; page++ {
		var params any
		if cursor != "" {
			params = resourcesListParams{Cursor: cursor}
		}

		resp, err := transport.Send(jsonrpcRequest{
			JSONRPC: jsonrpcVersion,
			ID:      nextID(),
			Method:  "resources/list",
			Params:  params,
		})
		if err != nil {
			return nil, fmt.Errorf("list resources: %w", err)
		}
		if resp.Error != nil {
			if resp.Error.Code == codeMethodNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("list resources: %s", resp.Error.Message)
		}

		var result resourcesListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("unmarshal resources: %w", err)
		}

		for _, r := range result.Resources {
			all = append(all, resourceOutput{
				Server:      serverName,
				URI:         r.URI,
				Name:        r.Name,
				Title:       r.Title,
				Description: r.Description,
				MimeType:    r.MimeType,
				Size:        r.Size,
			})
		}

		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}

	logStderr("warning: resources list truncated after %d pages", maxPages)
	return all, nil
}

func listAllResourceTemplates(transport Transport, serverName string) ([]resourceOutput, error) {
	var all []resourceOutput
	var cursor string
	const maxPages = 100

	for page := 0; page < maxPages; page++ {
		var params any
		if cursor != "" {
			params = resourcesListParams{Cursor: cursor}
		}

		resp, err := transport.Send(jsonrpcRequest{
			JSONRPC: jsonrpcVersion,
			ID:      nextID(),
			Method:  "resources/templates/list",
			Params:  params,
		})
		if err != nil {
			return nil, fmt.Errorf("list resource templates: %w", err)
		}
		if resp.Error != nil {
			if resp.Error.Code == codeMethodNotFound {
				return nil, nil
			}
			return nil, fmt.Errorf("list resource templates: %s", resp.Error.Message)
		}

		var result resourceTemplatesListResult
		if err := json.Unmarshal(resp.Result, &result); err != nil {
			return nil, fmt.Errorf("unmarshal resource templates: %w", err)
		}

		for _, r := range result.ResourceTemplates {
			all = append(all, resourceOutput{
				Server:      serverName,
				URITemplate: r.URITemplate,
				Name:        r.Name,
				Title:       r.Title,
				Description: r.Description,
				MimeType:    r.MimeType,
			})
		}

		if result.NextCursor == "" {
			return all, nil
		}
		cursor = result.NextCursor
	}

	logStderr("warning: resource templates list truncated after %d pages", maxPages)
	return all, nil
}

// cmdResources fetches listings live rather than caching them like tools,
// because resources change far more often than tool schemas.
func cmdResources(args []string) error {
	var serverFilter, query string
	var jsonOutput bool

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--query", "-q":
			if i+1 >= len(args) {
				return fmt.Errorf("--query requires a value")
			}
			i++
			query = args[i]
		case "--json":
			jsonOutput = true
		case "--help", "-h":
			_, _ = fmt.Fprintln(os.Stderr, `Usage: mcp resources [server|url] [--query <q>] [--json]

List readable resources (and resource templates) across configured servers.
Fetched live from each server. Read a resource's contents with
"mcp read <server|url> <uri>".

Flags:
  --query <q>   Filter resources by uri/name/description
  --json        Output as JSON (default: human-readable)`)
			return nil
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s", args[i])
			}
			if serverFilter == "" {
				serverFilter = args[i]
			} else {
				return fmt.Errorf("unexpected argument: %s", args[i])
			}
		}
	}

	if serverFilter != "" && isURL(serverFilter) {
		server, authToken, err := resolveServer(serverFilter)
		if err != nil {
			return err
		}
		resources, err := discoverResources(server, authToken)
		if err != nil {
			return err
		}
		return outputResourcesList(resources, query, jsonOutput)
	}

	if serverFilter != "" {
		if err := validateServerName(serverFilter); err != nil {
			return err
		}
	}

	servers, err := loadServers()
	if err != nil {
		return err
	}

	if len(servers) == 0 && serverFilter == "" {
		return outputResourcesList(nil, query, jsonOutput)
	}

	var (
		all []resourceOutput
		mu  sync.Mutex
		wg  sync.WaitGroup
	)
	sem := make(chan struct{}, 5)
	for _, s := range servers {
		if serverFilter != "" && s.Name != serverFilter {
			continue
		}
		if !s.IsEnabled() {
			continue
		}
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			authToken, err := getAuthToken(s.Name)
			if err != nil {
				logStderr("warning: auth token load failed for %q: %v", s.Name, err)
			}
			resources, err := discoverResources(&s, authToken)
			if err != nil {
				logStderr("warning: failed to get resources from %q: %v", s.Name, err)
				return
			}
			mu.Lock()
			all = append(all, resources...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	return outputResourcesList(all, query, jsonOutput)
}

func (r resourceOutput) resourceID() string {
	if r.URITemplate != "" {
		return r.URITemplate
	}
	return r.URI
}

func outputResourcesList(resources []resourceOutput, query string, jsonOutput bool) error {
	sort.Slice(resources, func(i, j int) bool {
		if resources[i].Server != resources[j].Server {
			return resources[i].Server < resources[j].Server
		}
		return resources[i].resourceID() < resources[j].resourceID()
	})

	if query != "" {
		q := strings.ToLower(query)
		var filtered []resourceOutput
		for _, r := range resources {
			if strings.Contains(strings.ToLower(r.resourceID()), q) ||
				strings.Contains(strings.ToLower(r.Name), q) ||
				strings.Contains(strings.ToLower(r.Title), q) ||
				strings.Contains(strings.ToLower(r.Description), q) {
				filtered = append(filtered, r)
			}
		}
		resources = filtered
	}

	if resources == nil {
		resources = []resourceOutput{}
	}

	if jsonOutput || !isStdoutTTY() {
		return outputJSON(resources)
	}
	return printResourcesHuman(resources)
}

func printResourcesHuman(resources []resourceOutput) error {
	if len(resources) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "No resources found.")
		return nil
	}

	maxIDLen := 0
	for _, r := range resources {
		if l := len(r.resourceID()); l > maxIDLen {
			maxIDLen = l
		}
	}

	lastServer := ""
	for _, r := range resources {
		if r.Server != lastServer {
			if lastServer != "" {
				_, _ = fmt.Fprintln(os.Stdout)
			}
			count := 0
			for _, u := range resources {
				if u.Server == r.Server {
					count++
				}
			}
			noun := "resources"
			if count == 1 {
				noun = "resource"
			}
			_, _ = fmt.Fprintf(os.Stdout, "%s (%d %s)\n", r.Server, count, noun)
			lastServer = r.Server
		}

		var label string
		switch {
		case r.Title != "":
			label = r.Title
		case r.Name != "":
			label = r.Name
		}
		desc := r.Description
		if r.URITemplate != "" {
			if label != "" {
				label = "(template) " + label
			} else {
				label = "(template)"
			}
		}

		suffix := label
		if desc != "" {
			if suffix != "" {
				suffix += " — " + desc
			} else {
				suffix = desc
			}
		}

		if suffix == "" {
			_, _ = fmt.Fprintf(os.Stdout, "  %s\n", r.resourceID())
		} else {
			pad := strings.Repeat(" ", 2+maxIDLen+2)
			lines := strings.Split(suffix, "\n")
			_, _ = fmt.Fprintf(os.Stdout, "  %-*s  %s\n", maxIDLen, r.resourceID(), lines[0])
			for _, line := range lines[1:] {
				_, _ = fmt.Fprintf(os.Stdout, "%s%s\n", pad, line)
			}
		}
	}

	return nil
}

func cmdRead(args []string) error {
	for _, a := range args {
		if a == "--help" || a == "-h" {
			_, _ = fmt.Fprintln(os.Stderr, `Usage: mcp read <server|url> <uri> [--max-output N] [--timeout <dur>]

Read the contents of a resource by URI. Discover available URIs with
"mcp resources <server|url>".

Flags:
  --max-output N         Truncate output to N chars (default 30000)
  --timeout <duration>   Per-call timeout (e.g. 30s, 5m; 0 = no limit)`)
			return nil
		}
	}

	if len(args) < 2 {
		return fmt.Errorf("usage: mcp read <server|url> <uri> [--max-output N] [--timeout <dur>]")
	}

	serverName := args[0]
	uri := args[1]
	if err := validateResourceURI(uri); err != nil {
		return err
	}

	maxOutput := defaultMaxOutput
	var timeout time.Duration
	timeoutSet := false

	for i := 2; i < len(args); i++ {
		switch args[i] {
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
		default:
			return fmt.Errorf("unexpected argument %q", args[i])
		}
	}

	server, authToken, err := resolveServer(serverName)
	if err != nil {
		return err
	}

	transport, err := mcpConnect(server, authToken)
	if err != nil {
		return err
	}
	defer func() { _ = transport.Close() }()

	if timeoutSet {
		transport.SetTimeout(timeout)
	}

	output, err := readResource(transport, uri)
	if err != nil {
		return err
	}

	truncateReadOutput(&output, maxOutput, serverName, uri)

	return outputJSON(output)
}

func readResource(transport Transport, uri string) (readOutput, error) {
	resp, err := transport.Send(jsonrpcRequest{
		JSONRPC: jsonrpcVersion,
		ID:      nextID(),
		Method:  "resources/read",
		Params:  resourceReadParams{URI: uri},
	})
	if err != nil {
		return readOutput{}, fmt.Errorf("read resource: %w", err)
	}
	if resp.Error != nil {
		return readOutput{}, fmt.Errorf("read resource: %s", resp.Error.Message)
	}

	var result resourceReadResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return readOutput{}, fmt.Errorf("unmarshal resource contents: %w", err)
	}

	out := readOutput{Contents: make([]readContent, 0, len(result.Contents))}
	for _, c := range result.Contents {
		out.Contents = append(out.Contents, readContent(c))
	}
	return out, nil
}

func truncateReadOutput(out *readOutput, maxOutput int, serverName, uri string) {
	if maxOutput <= 0 {
		return
	}

	total := 0
	for _, c := range out.Contents {
		total += len(c.Text) + len(c.Blob)
	}
	if total <= maxOutput {
		return
	}

	full, _ := json.Marshal(out.Contents)
	savedPath := saveFullOutput(serverName, uri, string(full))

	remaining := maxOutput
	for i := range out.Contents {
		c := &out.Contents[i]
		c.Blob = truncateField(c.Blob, &remaining)
		c.Text = truncateField(c.Text, &remaining)
	}

	out.Truncated = true
	note := fmt.Sprintf("\n[output truncated at %d chars]", maxOutput)
	if savedPath != "" {
		note += fmt.Sprintf("\n[full output saved to %s]", savedPath)
	}
	if n := len(out.Contents); n > 0 {
		out.Contents[n-1].Text += note
	}
}

func truncateField(s string, remaining *int) string {
	if s == "" {
		return s
	}
	if *remaining <= 0 {
		return ""
	}
	if len(s) > *remaining {
		s = s[:*remaining]
		*remaining = 0
		return s
	}
	*remaining -= len(s)
	return s
}
