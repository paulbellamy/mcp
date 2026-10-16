# Autoresearch summary — mcp CLI agent UX

**Stopped by breaker:** no kept Δ in 9 consecutive candidates (runs 5–13).
13 candidates of a 24 cap. Bench spend at list prices: ≈ $3.70 for the loop
(2 baseline, 13 candidate and 2 confirmation evals), plus $2.10 for the final
holdout and Sonnet checks. Mutator subagents are not included.

## Result

| | baseline (v0.13.0) | best | Δ |
|---|---|---|---|
| **agent_ux_score** (fixed suite, 2×144 runs) | 1.0006 | **1.0245** | +0.024 (≈4× the margin) |
| pass rate | 100% | 100% | — |
| mean agent cost / run | $0.00175 | $0.00144 | **−18%** |
| — cold (only told `mcp` exists) | $0.00195 | $0.00158 | −19% |
| — primed (skill in system prompt) | $0.00155 | $0.00130 | −17% |
| bash calls / run | 3.95 | 3.32 | −16% |
| output chars read / run | 8129 | 6537 | −20% |
| **holdout** (5 unseen tasks, 60 runs) | 1.0015 | **1.0202** | +0.019; cost −14%, calls 3.68→3.43 |
| **sonnet-5.5** cross-check (48 runs) | $0.0233, 3.06 calls | $0.0186, 2.65 calls | cost −20%, pass 100%→100% |

Kept changes (in `agent-ux-improvements`):

1. **run 2 — remove_bloat.** SKILL.md 9775 → 3240 chars. `mcp --help` goes from
   79 to 44 lines, so the `mcp --help | head -50` that agents run shows all of it.
   Pooled 1.0106.
2. **run 4 — accept_intent: `mcp list`.** One line per server with its tool
   names, from cache, with failing or disabled servers flagged. About 75% of
   cold agents opened with `mcp --help; mcp list`, and `list` did not exist.
   Pooled 1.0245.

Plus one bug the bench exposed. It is not ratchet-validated because the metric
cannot see it (it only affects a stderr warning):

- `listAllTools` printed `warning: tools list truncated after 100 pages` after
  every complete multi-page listing. The cursor was not cleared on the last page.

Per task, best vs baseline: every task got cheaper except `docs_resource`
(+8% cost, 3.0 → 3.9 calls). The holdout's resource task `h_vpn` regressed the
same way (4.1 → 4.75 calls). `mcp list` plus the shorter skill make a server
whose data is exposed only as resources look empty. Near-miss run 7 fixes this
(see below).

## Ledger

| run | operator | change | score | decision |
|---|---|---|---|---|
| 0 | — | baseline v0.13.0 | 1.0006 | — |
| 1 | accept_intent | ad-hoc URL parity: `schema <url>`, `-H` on URL commands (~208 Go lines) | 1.0029 | discard: within margin, needed 2× |
| 2 | remove_bloat | SKILL.md −67%, help fits 50 lines | 1.0106 | **keep** |
| 3 | swap_mechanism | compact JSON when piped | 0.9927 | discard |
| 4 | accept_intent | `mcp list` overview | 1.0245 | **keep** |
| 5 | swap_mechanism | truncation keeps head+tail, 20k default, better marker | 1.0285 | discard: +0.0040 |
| 6 | restructure | help leads with a worked example; errors name the next step | 1.0237 | discard |
| 7 | add_constraint | `mcp list` shows resource counts | 1.0301 | discard: +0.0056, **near-miss** |
| 8 | swap_mechanism | `mcp call --raw` | 1.0271 | discard: +0.0026 |
| 9 | add_counterexample | "mistakes that cost a turn" section | 1.0199 | discard: calls went *up* |
| 10 | stack | runs 5+8 | 1.0273 | discard: no additivity |
| 11 | tighten | stderr warning on unknown params, listing the valid ones | 1.0198 | discard (incl. 1 contamination fail) |
| 12 | regenerate | SKILL.md + help rewritten from scratch (−27%) | 1.0264 | discard |
| 13 | stack | runs 5+7+8 | 1.0299 | discard: equals run 7 alone |

Per-operator keep rate: accept_intent 1/2 · remove_bloat 1/1 · swap_mechanism
0/3 · add_constraint 0/1 · add_counterexample 0/1 · restructure 0/1 ·
tighten 0/1 · regenerate 0/1 · stack 0/2.

## What the loop learned

- **Accepting what agents already type works, and so does deleting text.**
  Adding text does not. Help restructures, worked examples, a common-mistakes
  section and a from-scratch rewrite were all neutral or negative. Once the
  skill was 3k chars, shortening it further stopped paying.
- **"Read the schema first" is bad advice for small models.** Guessing a flag
  and then correcting it costs the same as reading the schema first. A correct
  guess saves a turn. Run 9 made agents slower.
- **Output-format work hardly mattered.** Compact JSON (run 3), `--raw` (run 8)
  and head+tail truncation (run 5) left per-run cost within ±3%. Agents already
  bound what they read with `head`/`jq`, and their cost is dominated by turns,
  not bytes.
- **Near-miss worth a human look: run 7** (resource counts in `mcp list`). It
  replicated at about +0.0055 in two independent evals (runs 7 and 13), but
  never cleared the pre-registered 0.006 stage-1 bar, so it was not re-tested.
  It is the only change aimed at the one task class that regressed.
  Caveat: discovery sends up to two extra requests, and adds a one-time ~5 s
  wait for servers that never answer `resources/list`.

## Declined or breaking ideas surfaced by mutators (not applied)

- Make `--raw`-style unwrapped content the default when stdout is not a TTY.
  This changes the stdout JSON shape.
- Refuse unknown params when the schema says `additionalProperties: false`.
  This changes exit codes.
- Fill in `"server": ""` for ad-hoc URL listings; drop the per-entry `server`
  field from `mcp tools`.
- Treat `--header` on `mcp call <url>` as an HTTP header. This collides with
  tool params named `header`.

## Caveats on the measurement

- The agent under test is `claude-haiku-5-5`, Bash only, against mock servers.
  Pass rate was 100% from the start, so the score measures efficiency, not
  capability. Effectiveness gains on harder tasks or weaker models are
  unmeasured.
- The harness was changed twice before the baseline was frozen: the mock
  tracker was made stateful, and agents were given `--add-dir` for their own
  sandbox. Neither was changed after.
- The holdout tasks were written by the same orchestrator as the fixed set.
  Mutators never saw them, but they are not fully independent.
