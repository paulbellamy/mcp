package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// estimateTokens approximates token count for a JSON-marshaled value.
// Uses ~4 chars per token, which is a conservative approximation.
func estimateTokens(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(data) / 4
}

// summaryFor returns the compact payload for a tool that `mcp tools` emits
// in its JSON output (server + name + description, no schema).
func summaryFor(t toolOutput) map[string]string {
	return map[string]string{
		"server":      t.Server,
		"name":        t.Name,
		"description": t.Description,
	}
}

// cmdStats handles the `mcp stats` command.
// Reports estimated token cost of full schemas vs compact summaries per server.
// Reads from the local cache only — does not connect to servers.
func cmdStats(args []string) error {
	var full bool
	for _, arg := range args {
		switch arg {
		case "--full":
			full = true
		case "--help", "-h":
			_, _ = fmt.Fprintln(os.Stderr, `Usage: mcp stats [--full]

Estimates the token cost of tool schemas across configured servers.
Reads from the local cache only — run "mcp tools <server> --refresh" first
if a server has never been queried. Token counts are approximate
(~4 chars per token).

Flags:
  --full    Include per-tool breakdown.`)
			return nil
		default:
			return fmt.Errorf("unknown flag: %s", arg)
		}
	}

	servers, err := loadServers()
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		_, _ = fmt.Fprintln(os.Stderr, "No servers configured.")
		return nil
	}

	type serverStats struct {
		name          string
		toolCount     int
		schemaTokens  int
		summaryTokens int
		tools         []toolOutput
	}

	var stats []serverStats
	totalTools := 0
	totalSchemaTokens := 0
	totalSummaryTokens := 0

	for _, s := range servers {
		if !s.IsEnabled() {
			continue
		}
		tools, err := loadCachedToolsStale(s.Name)
		if err != nil {
			logStderr("warning: cache read failed for %q: %v", s.Name, err)
			continue
		}
		if tools == nil {
			logStderr("warning: no cached tools for %q (run `mcp tools %s --refresh`)", s.Name, s.Name)
			continue
		}

		ss := serverStats{name: s.Name, toolCount: len(tools), tools: tools}
		for _, t := range tools {
			ss.schemaTokens += estimateTokens(t.InputSchema)
			ss.summaryTokens += estimateTokens(summaryFor(t))
		}

		totalTools += ss.toolCount
		totalSchemaTokens += ss.schemaTokens
		totalSummaryTokens += ss.summaryTokens
		stats = append(stats, ss)
	}

	sort.Slice(stats, func(i, j int) bool {
		return stats[i].name < stats[j].name
	})

	if full {
		for _, ss := range stats {
			_, _ = fmt.Fprintf(os.Stdout, "\n%s (%d tools, %d schema tokens)\n\n", ss.name, ss.toolCount, ss.schemaTokens)
			maxNameLen := 0
			for _, t := range ss.tools {
				if len(t.Name) > maxNameLen {
					maxNameLen = len(t.Name)
				}
			}
			sortedTools := make([]toolOutput, len(ss.tools))
			copy(sortedTools, ss.tools)
			sort.Slice(sortedTools, func(i, j int) bool {
				return sortedTools[i].Name < sortedTools[j].Name
			})
			for _, t := range sortedTools {
				tokens := estimateTokens(t.InputSchema)
				desc := t.Description
				if idx := strings.Index(desc, "\n"); idx >= 0 {
					desc = desc[:idx]
				}
				if len(desc) > 60 {
					desc = desc[:57] + "..."
				}
				_, _ = fmt.Fprintf(os.Stdout, "  %-*s  %5d tokens    %q\n", maxNameLen, t.Name, tokens, desc)
			}
		}
		_, _ = fmt.Fprintln(os.Stdout)
	}

	// Server column widens to fit the longest name (min 15 for header).
	nameWidth := len("Server")
	for _, ss := range stats {
		if len(ss.name) > nameWidth {
			nameWidth = len(ss.name)
		}
	}
	if nameWidth < 15 {
		nameWidth = 15
	}

	rowFmt := fmt.Sprintf("%%-%ds %%5s  %%14s  %%14s  %%7s\n", nameWidth)
	dataFmt := fmt.Sprintf("%%-%ds %%5d  %%14d  %%14d  %%6d%%%%\n", nameWidth)
	ruleWidth := nameWidth + 1 + 5 + 2 + 14 + 2 + 14 + 2 + 8

	_, _ = fmt.Fprintf(os.Stdout, rowFmt, "Server", "Tools", "Schema Tokens", "Summary Tokens", "Savings")
	_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("-", ruleWidth))
	for _, ss := range stats {
		savings := 0
		if ss.schemaTokens > 0 {
			savings = 100 - (ss.summaryTokens * 100 / ss.schemaTokens)
		}
		_, _ = fmt.Fprintf(os.Stdout, dataFmt, ss.name, ss.toolCount, ss.schemaTokens, ss.summaryTokens, savings)
	}
	_, _ = fmt.Fprintln(os.Stdout, strings.Repeat("-", ruleWidth))
	totalSavings := 0
	if totalSchemaTokens > 0 {
		totalSavings = 100 - (totalSummaryTokens * 100 / totalSchemaTokens)
	}
	_, _ = fmt.Fprintf(os.Stdout, dataFmt, "Total", totalTools, totalSchemaTokens, totalSummaryTokens, totalSavings)

	return nil
}
