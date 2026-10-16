#!/usr/bin/env python3
"""decide.py RUN OPERATOR BRANCH LABEL [CONFIRM_LABEL] [--reason TEXT] [--complexity TEXT] [--force-discard]

Applies the contract's two-stage rule and records the verdict:
  stage 1 (LABEL only): discard unless score > best + confidence_margin,
                        else print NEEDS_CONFIRM and record nothing.
  stage 2 (+CONFIRM):   keep iff pooled mean over both evals beats best's
                        pooled mean by > confirm_margin_pooled.
Appends to results.jsonl, updates state.json, pins a git note on BRANCH.
Merging a kept branch into the best branch is left to the orchestrator.
"""
import argparse
import json
import statistics
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SP = Path("/tmp/claude-0/-home-user/d00c192f-9989-5be5-80ac-d670b8d21ee6/scratchpad")


def load_eval(label):
    d = SP / "evals" / label
    s = json.loads((d / "summary.json").read_text())
    scores = [json.loads(p.read_text()) for p in (d / "runs").glob("*/result.json")]
    return s, scores


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("run", type=int)
    ap.add_argument("operator")
    ap.add_argument("branch")
    ap.add_argument("label")
    ap.add_argument("confirm", nargs="?")
    ap.add_argument("--reason", default="")
    ap.add_argument("--complexity", default="")
    ap.add_argument("--force-discard", action="store_true", help="simplicity criterion or gate failure")
    a = ap.parse_args()
    st = json.loads((ROOT / "state.json").read_text())
    m = st["metric"]
    best = st["best"]["score"]
    s1, _ = load_eval(a.label)
    evals = [round(s1["score"], 4)]
    spent = s1["total_cost"]
    if a.confirm:
        s2, _ = load_eval(a.confirm)
        evals.append(round(s2["score"], 4))
        spent += s2["total_cost"]
        score = (s1["score"] * s1["n"] + s2["score"] * s2["n"]) / (s1["n"] + s2["n"])
        keep = score - best > m["confirm_margin_pooled"] and not a.force_discard
        reason = a.reason or (f"confirmed: pooled {score:.4f} vs best {best:.4f}" if keep else f"not confirmed: pooled {score:.4f} vs best {best:.4f}")
    else:
        score = s1["score"]
        if score - best > m["confidence_margin"] and not a.force_discard:
            print(f"NEEDS_CONFIRM stage1 {score:.4f} vs best {best:.4f} (+{score - best:.4f})")
            return
        keep = False
        reason = a.reason or (f"within confidence margin ({score - best:+.4f})" if score >= best else f"regressed ({score - best:+.4f})")
    items = {k: round(statistics.mean(v for v in [s1["items"].get(k)] + ([s2["items"].get(k)] if a.confirm else []) if v is not None), 4)
             for k in s1["items"]}
    rec = {"run": a.run, "operator": a.operator, "branch": a.branch, "score": round(score, 4), "evals": evals,
           "best_score": round(best, 4), "decision": "keep" if keep else "discard", "reason": reason,
           "complexity_delta": a.complexity, "pass_rate": round(s1["pass_rate"], 4), "mean_cost": round(s1["mean_cost"], 6),
           "mean_bash_calls": round(s1["mean_bash_calls"], 3), "items": items, "tokens": None}
    with open(ROOT / "results.jsonl", "a") as f:
        f.write(json.dumps(rec) + "\n")
    st["runs"] = max(st["runs"], a.run)
    st["bench_usd_spent"] = round(st["bench_usd_spent"] + spent, 4)
    if keep:
        st["best"] = {"run": a.run, "score": round(score, 4), "evals": evals, "branch": a.branch,
                      "commit": subprocess.check_output(["git", "-C", str(ROOT.parent), "rev-parse", a.branch]).decode().strip()}
        st["stale_streak"] = 0
    else:
        st["stale_streak"] += 1
    (ROOT / "state.json").write_text(json.dumps(st, indent=1))
    note = f"run={a.run} op={a.operator} score={score:.4f} best={best:.4f} decision={rec['decision']}"
    subprocess.run(["git", "-C", str(ROOT.parent), "notes", "add", "-f", "-m", note, a.branch], check=False)
    print(json.dumps({k: rec[k] for k in ("run", "operator", "score", "evals", "best_score", "decision", "reason")}))
    b = st["breakers"]
    trip = [n for n, hit in (("max_runs", st["runs"] >= b["max_runs"]), ("no_delta_in_k", st["stale_streak"] >= b["no_delta_in_k"]),
                             ("max_bench_usd", st["bench_usd_spent"] >= b["max_bench_usd"])) if hit]
    print(f"stale_streak={st['stale_streak']}/{b['no_delta_in_k']} runs={st['runs']}/{b['max_runs']} bench_usd={st['bench_usd_spent']}"
          + (f" BREAKER TRIPPED: {trip}" if trip else ""))


if __name__ == "__main__":
    main()
