package main

import (
	"strings"
	"testing"
)

// TestUsageFitsOneScreen keeps the top-level help readable in one call:
// agents commonly run `mcp --help 2>&1 | head -50`, so everything — including
// the ad-hoc header hint and the skill pointer at the end — must fit.
func TestUsageFitsOneScreen(t *testing.T) {
	out := captureStderr(t, printUsage)
	if n := strings.Count(out, "\n"); n >= 50 {
		t.Errorf("usage is %d lines; keep it under 50 so `head -50` shows all of it", n)
	}
	for _, want := range []string{"MCP_HEADERS", "mcp --skill"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage missing %q", want)
		}
	}
}

// TestSkillBudget guards against SKILL.md regrowing: primed agents carry it
// in their context on every turn.
func TestSkillBudget(t *testing.T) {
	const budget = 4000
	if n := len(skillMD); n > budget {
		t.Errorf("SKILL.md is %d chars; keep it under %d", n, budget)
	}
}
