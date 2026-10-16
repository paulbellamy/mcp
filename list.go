package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// listNameLimit caps the tool names shown per server so one very large
// server can't flood the overview; the rest stay reachable via --query.
const listNameLimit = 150

const listUsage = `Usage: mcp list [--refresh]

One line per configured server with its tool names, or why it failed:
  <server> (<n> tools): <tool>, <tool>, ...
  <server>: error: <reason>
  <server>: disabled
Reads the tool cache, querying servers whose cache is missing or stale
(--refresh queries every server). Next: mcp schema <server> <tool>.
Descriptions: mcp tools <server>.`

// cmdList handles `mcp list`: a compact overview of every configured server
// and its tool names, so one read answers "what do I have".
func cmdList(args []string) error {
	refresh := false
	for _, a := range args {
		switch a {
		case "--help", "-h":
			fmt.Fprintln(os.Stderr, listUsage)
			return nil
		case "--refresh":
			refresh = true
		default:
			return fmt.Errorf("unexpected argument %q: mcp list covers every server; for one server's tools with descriptions use: mcp tools <server>", a)
		}
	}

	servers, err := loadServers()
	if err != nil {
		return err
	}
	if len(servers) == 0 {
		logStderr("No servers configured. Add one: mcp add <name> <url>  or  mcp add <name> --stdio <command>")
		return nil
	}

	lines := make([]string, len(servers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)
	for i, s := range servers {
		if !s.IsEnabled() {
			lines[i] = s.Name + ": disabled"
			continue
		}
		wg.Add(1)
		go func(i int, s ServerConfig) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			tools, err := getToolsForServer(&s, refresh)
			lines[i] = formatListLine(s.Name, tools, err)
		}(i, s)
	}
	wg.Wait()

	for _, l := range lines {
		fmt.Println(l)
	}
	return nil
}

// formatListLine renders one server's overview line.
func formatListLine(server string, tools []toolOutput, err error) string {
	if err != nil {
		return server + ": error: " + strings.Join(strings.Fields(err.Error()), " ")
	}
	names := make([]string, len(tools))
	for i, t := range tools {
		names[i] = t.Name
	}
	sort.Strings(names)

	noun := "tools"
	if len(names) == 1 {
		noun = "tool"
	}
	line := fmt.Sprintf("%s (%d %s)", server, len(names), noun)
	if len(names) == 0 {
		return line
	}
	shown := names
	if len(shown) > listNameLimit {
		shown = shown[:listNameLimit]
	}
	line += ": " + strings.Join(shown, ", ")
	if more := len(names) - len(shown); more > 0 {
		line += fmt.Sprintf(", ... +%d more (mcp tools %s --query <word>)", more, server)
	}
	return line
}
