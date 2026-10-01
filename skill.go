package main

import (
	_ "embed"
	"fmt"
)

// skillMD is the agent skill bundled at build time, so `mcp --skill` always
// prints the copy that matches this binary's release.
//
//go:embed skills/mcp-cli/SKILL.md
var skillMD string

// printSkill writes the bundled agent skill to stdout.
func printSkill() {
	fmt.Print(skillMD)
}
