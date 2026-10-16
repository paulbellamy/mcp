# Autoresearch contract — mcp CLI agent UX

Goal: make the `mcp` CLI easier and more intuitive for AI agents to use
efficiently (fewer turns, less output to read, less doc to load) and
effectively (task success), without breaking existing users or scripts.

- **Target:** the CLI's agent-facing surface — non-test `*.go` in the repo
  root (help text, errors, flags/aliases, output shaping), and
  `skills/mcp-cli/SKILL.md` (embedded; what `mcp --skill`, the Claude Code
  plugin and the Pi package deliver). `README.md` may be kept in sync (not
  scored). New `*_test.go` files allowed; existing tests may change only where
  an intended output/help change makes an assertion stale.
- **Frozen:** `.autoresearch/bench/**` (mock MCP servers, task suites,
  checks, `ref_costs.json`, the scorer), `.autoresearch/tools/**`, `go.mod`
  (no new deps), `.github/`, `scripts/`, `extensions/`, `hooks/`,
  `.claude-plugin/`, `package.json`. Hashes in `state.json.frozen`.
  Mutators work in sparse checkouts that do not contain `.autoresearch/`.
- **Compatibility lock:** existing commands, flags, exit codes and stdout JSON
  shapes keep working. Additive only. Breaking ideas are logged, not applied.
- **Metric:** `agent_ux_score`, higher is better. A headless agent
  (`claude-haiku-5-5`, Bash tool only, hermetic HOME, no native MCP) runs each
  task against 8 configured stdio mock servers (+1 broken, +2 ad-hoc HTTP).
  Per run: `pass × (0.75 + 0.5·ref/(ref+cost))`, `ref` = frozen per-task
  baseline median USD cost. Pass is checked deterministically (answer and/or
  the mock servers' call logs). Metric = mean over runs. Baseline ≈ 1.000;
  a fail costs a whole run; ~5% lower agent cost ≈ +0.006.
- **Budget (per experiment, fixed):** 144 runs = 12 fixed tasks ×
  {cold: agent only told `mcp` exists; primed: `mcp --skill` text in its
  system prompt} × 6 reps; 300 s and $0.60 cap per run.
- **Validation set:** the 12 fixed tasks every cycle. Holdout: 5 tasks never
  shown to mutators, scored at baseline and at the end. Cross-model check
  (`claude-sonnet-5-5`) on the final best vs baseline.
- **Confidence margin:** baseline A/B = 1.0029 / 0.9983 (per-run sd 0.028,
  SE of a difference ≈ 0.0032). Stage 1: candidate must beat best by
  **> 0.006**; then a confirmation eval, and the pooled 288-run mean must
  still beat best's pooled mean by **> 0.0046**.
- **Hard gates:** `go build`, `go vet`, `go test ./...`, `golangci-lint run`,
  scope check (only target paths changed).
- **Search:** (μ+λ) generations of 3 candidates, each from the current best
  by a separate mutator subagent with a distinct operator; best confirmed
  candidate is kept; confirmed runners-up are rebased onto the new best and
  re-tested next generation.
- **Operators:** tighten · add_counterexample · restructure · remove_bloat ·
  accept_intent (accept what agents naturally type) · swap_mechanism ·
  fix_defect (a bug the evidence exposes).
- **Breakers:** max 24 candidate evals · no kept Δ in 9 consecutive
  candidates · plateau K=5 → next generation includes a from-scratch rewrite
  of SKILL.md / help from the failure history · bench spend ≤ $15.
- **Simplicity:** a hair of metric bought with tangled complexity is a
  discard; >150 changed non-test Go lines needs ≥ 2× margin.
