#!/usr/bin/env python3
"""Agent-UX bench for the mcp CLI. FROZEN (the scorer).

Runs a headless Claude agent (Bash only) against hermetic mock MCP servers,
once per (task, condition, rep), and scores how well it uses the candidate
`mcp` binary.

  bench.py run --binary PATH --out DIR [--suite fixed|holdout] [--reps 2]
               [--conds cold,primed] [--workers 8] [--model M] [--tasks a,b]
  bench.py rescore --out DIR        # recompute summary from stored runs

Conditions:
  cold    agent is only told that `mcp` is installed
  primed  the binary's own `mcp --skill` text is appended to the system prompt
          (how the Claude Code plugin / Pi package deliver it)

Per-run score = pass * (0.75 + 0.5 * ref/(ref + cost)), ref = the frozen
per-task baseline median cost in ref_costs.json (1.0 at baseline cost,
→1.25 as cost → 0, 0 on fail). Metric = mean over runs.
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import shutil
import socket
import statistics
import subprocess
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import tasks as T  # noqa: E402

WORKSPACE = ["weather", "finance", "tracker", "cloud", "docs", "ci", "crm", "warehouse"]
KEEP_ENV = ["PATH", "HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "NODE_EXTRA_CA_CERTS",
            "SSL_CERT_FILE", "CLAUDE_SESSION_INGRESS_TOKEN_FILE", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY",
            "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "LANG", "TERM"]
CONTAM = re.compile(r"mockserver|profiles\.py|tasks\.py|\.autoresearch|bench\.py")
PREAMBLE = ("You have shell access. The `mcp` command-line tool is installed for working with MCP (Model Context Protocol) "
            "servers, and several servers are already configured for you.\n\nTask: {task}\n\n"
            "Work autonomously; there is no user to answer questions. When you are done, end your final message with a "
            "single line of the form:\nANSWER: <your answer>")


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def start_http(profile):
    port = free_port()
    proc = subprocess.Popen([sys.executable, str(HERE / "mockserver.py"), "--profile", profile, "--http", str(port)],
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    for _ in range(50):
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.2).close()
            break
        except OSError:
            time.sleep(0.1)
    return proc, f"http://127.0.0.1:{port}/mcp"


def sh(path, body):
    path.write_text("#!/bin/sh\n" + body + "\n")
    path.chmod(0o755)


def setup_sandbox(rd, binary):
    for d in ("home", "work", "opt", "logs", "bin"):
        (rd / d).mkdir(parents=True, exist_ok=True)
    os.symlink(binary, rd / "bin" / "mcp")
    ms = f"{sys.executable} {HERE / 'mockserver.py'}"
    for name in WORKSPACE:
        sh(rd / "opt" / f"{name}-mcp", f'exec {ms} --profile {name} --log {rd / "logs"} "$@"')
    sh(rd / "opt" / "legacy-search-mcp", f'exec {ms} --profile broken "$@"')
    sh(rd / "opt" / "kvd", f'exec {ms} --profile kv --log {rd / "logs"} "$@"')
    (rd / "opt" / "kv.json").write_text(json.dumps({"launch-code": "tangerine-42", "motd": "hello", "region": "eu-west"}))
    env = {"HOME": str(rd / "home"), "PATH": f"{rd / 'bin'}:{os.environ['PATH']}"}
    for name in WORKSPACE + ["legacy-search"]:
        subprocess.run([str(rd / "bin" / "mcp"), "add", name, "--stdio", str(rd / "opt" / f"{name}-mcp")],
                       env=env, capture_output=True, timeout=30)
    # The broken server must not leave a cache behind; everything else may.
    return env


def agent_env(rd):
    env = {k: os.environ[k] for k in KEEP_ENV if k in os.environ}
    env["HOME"] = str(rd / "home")
    env["PATH"] = f"{rd / 'bin'}:{os.environ['PATH']}"
    return env


def parse_stream(text):
    cmds, outputs, errors, final, res = [], [], 0, "", {}
    for line in text.splitlines():
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        t = ev.get("type")
        if t == "assistant":
            for b in ev.get("message", {}).get("content", []):
                if b.get("type") == "tool_use":
                    cmds.append(b.get("input", {}).get("command", json.dumps(b.get("input"))))
        elif t == "user":
            c = ev.get("message", {}).get("content", [])
            for b in c if isinstance(c, list) else []:
                if b.get("type") == "tool_result":
                    body = b.get("content")
                    if isinstance(body, list):
                        body = "\n".join(x.get("text", "") for x in body if isinstance(x, dict))
                    outputs.append(body or "")
                    errors += bool(b.get("is_error"))
        elif t == "result":
            res = ev
            final = ev.get("result") or ""
    return cmds, outputs, errors, final, res


def extract_answer(final):
    i = final.rfind("ANSWER:")
    return final[i + 7:].strip() if i >= 0 else ""


def read_calls(rd):
    calls = []
    for f in (rd / "logs").glob("*.jsonl"):
        for line in f.read_text().splitlines():
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                continue
            rec["server"] = f.stem
            calls.append(rec)
    calls.sort(key=lambda c: c.get("ts", 0))
    return calls


def run_one(task, cond, rep, out, binary, model, urls, skill_text, timeout):
    rd = out / "runs" / f"{task['id']}__{cond}__{rep}"
    if rd.exists():
        shutil.rmtree(rd)
    rd.mkdir(parents=True)
    setup_sandbox(rd, binary)
    prompt = PREAMBLE.format(task=task["prompt"].format(opt=rd / "opt", **urls))
    cmd = ["claude", "-p", "--model", model, "--output-format", "stream-json", "--verbose", "--safe-mode", "--strict-mcp-config",
           "--tools", "Bash", "--allowedTools", "Bash", "--permission-mode", "dontAsk", "--no-session-persistence",
           "--max-budget-usd", "0.60", "--add-dir", str(rd)]
    if cond == "primed":
        cmd += ["--append-system-prompt", skill_text]
    t0 = time.time()
    try:
        p = subprocess.run(cmd, input=prompt, cwd=rd / "work", env=agent_env(rd), capture_output=True, text=True, timeout=timeout)
        stream, timed_out = p.stdout, False
    except subprocess.TimeoutExpired as e:
        stream, timed_out = (e.stdout or b"").decode() if isinstance(e.stdout, bytes) else (e.stdout or ""), True
    wall = time.time() - t0
    (rd / "transcript.jsonl").write_text(stream)
    cmds, outputs, errs, final, res = parse_stream(stream)
    answer = extract_answer(final)
    ctx = {"answer": answer, "final": final, "calls": read_calls(rd), "sandbox": rd,
           "mcp": lambda a: subprocess.run([str(rd / "bin" / "mcp"), *a], env={"HOME": str(rd / "home"), "PATH": os.environ["PATH"]},
                                           capture_output=True, text=True, timeout=30).stdout}
    contaminated = any(CONTAM.search(c) for c in cmds)
    try:
        ok, why = task["check"](ctx)
    except Exception as e:  # a crashing check is a fail, never a pass
        ok, why = False, f"check error: {e!r}"
    if contaminated:
        ok, why = False, "contaminated: inspected bench internals"
    if timed_out:
        ok, why = False, "timeout"
    rec = {"task": task["id"], "cond": cond, "rep": rep, "pass": bool(ok), "why": why, "answer": answer[:300],
           "cost": res.get("total_cost_usd", 0.0) or 0.0, "turns": res.get("num_turns", 0), "bash_calls": len(cmds),
           "error_results": errs, "output_chars": sum(len(o) for o in outputs), "wall_s": round(wall, 1),
           "usage": res.get("usage", {}), "cmds": cmds, "outputs_head": [o[:400] for o in outputs], "final_tail": final[-600:]}
    (rd / "result.json").write_text(json.dumps(rec, indent=1))
    return rec


def load_ref():
    f = HERE / "ref_costs.json"
    return json.loads(f.read_text()) if f.exists() else {}


def run_score(r, ref):
    if not r["pass"]:
        return 0.0
    c = ref.get(r["task"])
    if not c:
        return 1.0
    return 0.75 + 0.5 * c / (c + max(r["cost"], 1e-6))


def summarize(out):
    recs = [json.loads(p.read_text()) for p in sorted((out / "runs").glob("*/result.json"))]
    ref = load_ref()
    for r in recs:
        r["score"] = run_score(r, ref)
    by = {}
    for r in recs:
        by.setdefault(r["task"], []).append(r)
    per_task = {k: {"pass_rate": sum(x["pass"] for x in v) / len(v), "score": statistics.mean(x["score"] for x in v),
                    "median_cost": statistics.median(x["cost"] for x in v), "mean_bash_calls": statistics.mean(x["bash_calls"] for x in v),
                    "n": len(v)} for k, v in sorted(by.items())}
    per_cond = {}
    for r in recs:
        per_cond.setdefault(r["cond"], []).append(r)
    s = {"n": len(recs), "score": statistics.mean(r["score"] for r in recs) if recs else 0.0,
         "score_sd": statistics.stdev(r["score"] for r in recs) if len(recs) > 1 else 0.0,
         "pass_rate": statistics.mean(r["pass"] for r in recs) if recs else 0.0,
         "mean_cost": statistics.mean(r["cost"] for r in recs) if recs else 0.0,
         "total_cost": sum(r["cost"] for r in recs),
         "mean_bash_calls": statistics.mean(r["bash_calls"] for r in recs) if recs else 0.0,
         "mean_output_chars": statistics.mean(r["output_chars"] for r in recs) if recs else 0.0,
         "per_cond": {k: {"score": statistics.mean(x["score"] for x in v), "pass_rate": statistics.mean(x["pass"] for x in v),
                          "mean_cost": statistics.mean(x["cost"] for x in v)} for k, v in per_cond.items()},
         "per_task": per_task, "items": {k: round(v["score"], 4) for k, v in per_task.items()}, "ref_costs": bool(ref)}
    (out / "summary.json").write_text(json.dumps(s, indent=1))
    write_digest(out, recs, per_task)
    return s


def write_digest(out, recs, per_task):
    """Mutator-facing digest: what the agents actually typed and what went wrong."""
    lines = ["# Bench digest", ""]
    for tid, st in per_task.items():
        lines.append(f"## {tid}: pass {st['pass_rate']:.2f}, score {st['score']:.3f}, median cost ${st['median_cost']:.4f}, "
                     f"mean bash calls {st['mean_bash_calls']:.1f}")
        for r in sorted((x for x in recs if x["task"] == tid), key=lambda x: (x["cond"], x["rep"])):
            lines.append(f"### {r['cond']} rep{r['rep']}: {'PASS' if r['pass'] else 'FAIL'} ({r['why']}); ${r['cost']:.4f}, "
                         f"{r['bash_calls']} calls, {r['error_results']} errored, {r['output_chars']} output chars")
            for i, c in enumerate(r["cmds"]):
                o = r["outputs_head"][i] if i < len(r["outputs_head"]) else ""
                lines.append(f"- `{c[:300]}`")
                lines.append(f"  → {o[:240]!r}")
            lines.append(f"- ANSWER: {r['answer'][:200]!r}")
        lines.append("")
    (out / "digest.md").write_text("\n".join(lines))


def cmd_run(a):
    out = Path(a.out).resolve()
    out.mkdir(parents=True, exist_ok=True)
    binary = str(Path(a.binary).resolve())
    suite = T.FIXED if a.suite == "fixed" else T.HOLDOUT
    if a.tasks:
        want = set(a.tasks.split(","))
        suite = [t for t in suite if t["id"] in want]
    skill_text = subprocess.run([binary, "--skill"], capture_output=True, text=True, env={"HOME": str(out), "PATH": os.environ["PATH"]}).stdout
    procs, urls = [], {}
    for prof in ("shipping", "billing"):
        p, u = start_http(prof)
        procs.append(p)
        urls[f"{prof}_url"] = u
    jobs = [(t, c, r) for r in range(a.reps) for t in suite for c in a.conds.split(",")]
    try:
        with cf.ThreadPoolExecutor(a.workers) as ex:
            futs = [ex.submit(run_one, t, c, r, out, binary, a.model, urls, skill_text, a.timeout) for t, c, r in jobs]
            for f in cf.as_completed(futs):
                r = f.result()
                print(f"{r['task']:16s} {r['cond']:6s} {r['rep']} {'PASS' if r['pass'] else 'FAIL'} ${r['cost']:.4f} "
                      f"{r['bash_calls']:2d} calls {r['wall_s']:5.1f}s  {r['why'][:60]}", flush=True)
    finally:
        for p in procs:
            p.kill()
    s = summarize(out)
    print(json.dumps({k: s[k] for k in ("n", "score", "score_sd", "pass_rate", "mean_cost", "total_cost", "mean_bash_calls", "per_cond")}, indent=1))


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    r = sub.add_parser("run")
    r.add_argument("--binary", required=True)
    r.add_argument("--out", required=True)
    r.add_argument("--suite", default="fixed", choices=["fixed", "holdout"])
    r.add_argument("--reps", type=int, default=2)
    r.add_argument("--conds", default="cold,primed")
    r.add_argument("--workers", type=int, default=8)
    r.add_argument("--model", default="claude-haiku-5-5")
    r.add_argument("--tasks")
    r.add_argument("--timeout", type=int, default=300)
    rs = sub.add_parser("rescore")
    rs.add_argument("--out", required=True)
    a = ap.parse_args()
    if a.cmd == "run":
        cmd_run(a)
    else:
        print(json.dumps({k: v for k, v in summarize(Path(a.out).resolve()).items() if k != "per_task"}, indent=1))


if __name__ == "__main__":
    main()
