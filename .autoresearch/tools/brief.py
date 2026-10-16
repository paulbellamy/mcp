#!/usr/bin/env python3
"""Mutator-facing brief from a bench eval dir (NOT part of the scorer).

brief.py EVAL_DIR [--suite fixed]  -> markdown on stdout:
per-task table, a friction catalogue (commands whose output signalled a CLI
error, grouped by error text), and the worst transcripts in compact form.
"""
import json
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

ERR = re.compile(r"^(error: .*|unknown command: .*|warning: .*|invalid .*|Usage: mcp.*)$", re.M)


def parse(tr):
    cmds, outs = [], []
    for line in tr.read_text().splitlines():
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        if ev.get("type") == "assistant":
            for b in ev["message"].get("content", []):
                if b.get("type") == "tool_use":
                    cmds.append(b["input"].get("command", ""))
        elif ev.get("type") == "user":
            c = ev["message"].get("content", [])
            for b in c if isinstance(c, list) else []:
                if b.get("type") == "tool_result":
                    body = b.get("content")
                    if isinstance(body, list):
                        body = "\n".join(x.get("text", "") for x in body if isinstance(x, dict))
                    outs.append((body or "", bool(b.get("is_error"))))
    return cmds, outs


def norm_err(e):
    e = re.sub(r"https?://\S+", "<url>", e)
    e = re.sub(r"/tmp/\S+", "<path>", e)
    e = re.sub(r'"[^"]{20,}"', '"…"', e)
    return e[:160]


def main():
    d = Path(sys.argv[1])
    s = json.loads((d / "summary.json").read_text())
    print(f"# Bench brief — score {s['score']:.4f}, pass {s['pass_rate']:.3f}, mean bash calls {s['mean_bash_calls']:.2f}, "
          f"mean output chars {s['mean_output_chars']:.0f}, n={s['n']}\n")
    print("| task | pass | score | median $ | bash calls |\n|---|---|---|---|---|")
    for k, v in s["per_task"].items():
        print(f"| {k} | {v['pass_rate']:.2f} | {v['score']:.3f} | {v['median_cost']:.4f} | {v['mean_bash_calls']:.1f} |")
    print(f"\nper condition: " + ", ".join(f"{k}: score {v['score']:.3f} pass {v['pass_rate']:.2f}" for k, v in s["per_cond"].items()))
    fric = Counter()
    examples = defaultdict(list)
    runs = []
    for rd in sorted((d / "runs").iterdir()):
        if not (rd / "result.json").exists():
            continue
        r = json.loads((rd / "result.json").read_text())
        cmds, outs = parse(rd / "transcript.jsonl")
        runs.append((r, cmds, outs))
        for c, (o, _) in zip(cmds, outs):
            for m in ERR.findall(o):
                if m.startswith("Usage: mcp") and re.search(r"(--help|-h\b|\bhelp\b)", c):
                    continue  # the agent asked for usage; not friction
                k = norm_err(m)
                fric[k] += 1
                if len(examples[k]) < 3:
                    examples[k].append(c[:220])
    print("\n## Friction catalogue (CLI error/warning lines seen by agents, with example commands)\n")
    for k, n in fric.most_common(40):
        print(f"- **{n}×** `{k}`")
        for e in examples[k]:
            print(f"    - `{e}`")
    print("\n## Failed runs\n")
    for r, cmds, outs in runs:
        if r["pass"]:
            continue
        print(f"### {r['task']} {r['cond']} rep{r['rep']}: {r['why']}")
        for c, (o, _) in zip(cmds, outs):
            print(f"- `{c[:260]}`\n  → {o[:300]!r}")
        print(f"- ANSWER: {r['answer'][:200]!r}\n")
    print("\n## Most expensive passing run per task\n")
    best = {}
    for r, cmds, outs in runs:
        if r["pass"] and (r["task"] not in best or r["cost"] > best[r["task"]][0]["cost"]):
            best[r["task"]] = (r, cmds, outs)
    for t, (r, cmds, outs) in sorted(best.items()):
        print(f"### {t} {r['cond']} rep{r['rep']}: ${r['cost']:.4f}, {len(cmds)} calls, {r['output_chars']} output chars")
        for c, (o, _) in zip(cmds, outs):
            print(f"- `{c[:260]}`\n  → {o[:200]!r}")
        print()


if __name__ == "__main__":
    import signal
    signal.signal(signal.SIGPIPE, signal.SIG_DFL)
    main()
