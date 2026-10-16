# Mutator brief (one experiment)

You are proposing ONE experiment in an autoresearch loop whose goal is to make
the `mcp` CLI (a Go CLI for discovering and calling tools on MCP servers)
easier and more intuitive for AI agents to use efficiently and effectively.

Your checkout: `{worktree}` (branch `{branch}`, cut from the current best).
Mutation operator for this run: **{operator}** — {operator_desc}
Focus hint from the orchestrator: {focus}

## Evidence

`{brief}` is a digest of how headless agents (a small, fast model with only a
Bash tool) actually used the current best binary on a fixed task suite: which
commands they typed, which CLI errors/warnings they hit (the "friction
catalogue"), failed runs, and the most expensive passing runs. Read it first.
Two conditions are measured: `cold` (agent only knows `mcp` is installed and
must discover usage via `--help` / `mcp --skill`) and `primed` (the text of
`mcp --skill`, i.e. skills/mcp-cli/SKILL.md, is in the agent's system prompt).
The score rewards task success first, then lower total agent cost (fewer
turns, less output to read, smaller always-loaded skill text).

Ledger of prior experiments (what was tried, kept or discarded and why):
{ledger}

## Rules (violating any one discards the experiment)

1. Editable: non-test `*.go` files in the repo root, `skills/mcp-cli/SKILL.md`
   (embedded in the binary; printed by `mcp --skill`), `README.md` (keep in
   sync with behaviour; not scored). You may add new `*_test.go` files, and
   update an existing test only where your intended output/help change makes
   an assertion stale. Nothing else: not go.mod/go.sum (no new deps), not
   `.github/`, `scripts/`, `extensions/`, `hooks/`, `.claude-plugin/`.
2. Do NOT look for, read, or reference the benchmark harness. It is not in
   your checkout; do not go searching for it elsewhere on disk.
3. Generalize. Never mention the specific servers, tools, data or task
   wording you see in the brief (weather, Oslo, tracker, ATLAS, zones,
   invoices, etc.) in code or docs. Fix the *class* of friction so it helps
   with any MCP server. The change is also scored on held-out tasks you have
   not seen; overfitting loses.
4. Backward compatible: existing commands, flags, exit codes and stdout JSON
   shapes keep working for scripts. Additive changes (new aliases, new
   flags, new fields, better errors/help, accepting what agents naturally
   type) are fine. If you believe a breaking change is the right call, do
   not make it — describe it in your final report instead.
5. ONE coherent change, kept small and simple. A hair of improvement bought
   with tangled complexity is a discard. Prefer deleting/shortening over
   adding. Docs that grow cost tokens on every primed run.
6. Before finishing, all of these must pass in your checkout:
   `go build ./... && go vet ./... && go test ./... && golangci-lint run ./...`
   Add focused unit tests for new behaviour.
7. Commit your change on `{branch}` with a clear message (no co-author or
   model lines needed). Do not push. Do not touch other branches.

## Report

Reply with: the operator, a 2–4 sentence description of the change and the
friction it targets (cite brief evidence), `git diff --stat`, and any
breaking-change ideas you declined to implement.
