"""Task suites for the agent-UX bench. FROZEN.

Each task: id, prompt (formatted with {opt}, {shipping_url}, {billing_url}),
check(ctx) -> (pass: bool, reason: str). ctx has: answer (text after the
last 'ANSWER:'), final (full final message), calls (list of logged tool
calls across mock servers), sandbox (Path), mcp (callable running the
candidate binary inside the sandbox).
"""
import re

import profiles


def norm(s):
    return re.sub(r"[\s,`*_\"']+", " ", (s or "").lower()).strip()


def has_num(text, target, tol):
    for m in re.findall(r"-?\d[\d,]*\.?\d*", text or ""):
        try:
            if abs(float(m.replace(",", "")) - target) <= tol:
                return True
        except ValueError:
            pass
    return False


def calls_to(ctx, server, toolname):
    return [c for c in ctx["calls"] if c.get("server") == server and c.get("tool") == toolname]


# ------------------------------------------------------------ fixed set
def c_weather(ctx):
    return has_num(ctx["answer"], profiles.f2(profiles.WEATHER["oslo"]["hi"][2]), 0.15), "oslo day-3 high in F"


def c_fx(ctx):
    return has_num(ctx["answer"], profiles.convert(250, "EUR", "JPY"), 1.0), "250 EUR->JPY"


def c_create(ctx):
    ok = [c for c in calls_to(ctx, "tracker", "create_issue") if not c["isError"]]
    if len(ok) != 1:
        return False, f"{len(ok)} successful create_issue calls"
    a = ok[0]["arguments"]
    f = a.get("fields") or {}
    good = (str(a.get("project", "")).upper() == "ATLAS" and a.get("title", "").strip() == "Login button misaligned on Safari"
            and isinstance(a.get("labels"), list) and sorted(a["labels"]) == ["bug", "ui"]
            and isinstance(f, dict) and f.get("priority") == "high" and f.get("assignee") == "mkim")
    if not good:
        return False, f"wrong args {a}"
    return "ATLAS-482" in (ctx["answer"] or "").upper(), "issue key reported"


def c_dns(ctx):
    return "203.0.113.47" in (ctx["answer"] or ""), "api A record"


def c_docs(ctx):
    return "2026-11-18" in (ctx["answer"] or "") or "november 18" in norm(ctx["answer"]), "heron launch date"


def c_log(ctx):
    return "_zstd_frame_decode" in (ctx["answer"] or ""), "undefined symbol at end of log"


def c_tickets(ctx):
    nums = re.findall(r"\b\d+\b", ctx["answer"] or "")
    return nums[:1] == ["3"] or norm(ctx["answer"]) in ("3", "three"), "3 open tickets"


def c_ship(ctx):
    return "out for delivery" in norm(ctx["answer"]).replace("_", " ") or "out_for_delivery" in (ctx["answer"] or "").lower(), "order status"


def c_kv(ctx):
    if "tangerine-42" not in (ctx["answer"] or ""):
        return False, "value"
    out = ctx["mcp"](["servers"])
    return '"kv"' in out, "kv registered"


def c_status(ctx):
    ok = [c for c in calls_to(ctx, "tracker", "update_issue_status") if not c["isError"]]
    if not ok or ok[-1]["arguments"].get("key", "").upper() != "ATLAS-17" or ok[-1]["arguments"].get("status") != "in_review":
        return False, f"final transition {ok[-1]['arguments'] if ok else None}"
    return "in_review" in (ctx["answer"] or "").lower() or "in review" in norm(ctx["answer"]), "status reported"


def c_unreachable(ctx):
    names = {n.strip().strip("`.").lower() for n in re.split(r"[,\s]+", ctx["answer"] or "") if n.strip()}
    known = {"weather", "finance", "tracker", "cloud", "docs", "ci", "crm", "warehouse", "legacy-search"}
    return names & known == {"legacy-search"}, f"named {sorted(names & known)}"


def c_stats(ctx):
    names = {n.strip().strip("`.").lower() for n in re.split(r"[,\s]+", ctx["answer"] or "") if n.strip()}
    known = {"weather", "finance", "tracker", "cloud", "docs", "ci", "crm", "warehouse", "legacy-search"}
    return names & known == {"warehouse"}, f"named {sorted(names & known)}"


FIXED = [
    {"id": "weather_units", "check": c_weather, "prompt":
     "According to the weather MCP server, what is the forecast high temperature for Oslo on the third day of its forecast, in degrees Fahrenheit?"},
    {"id": "find_fx", "check": c_fx, "prompt":
     "One of the configured MCP servers can convert currencies. Use it to convert 250 EUR to JPY and give the converted amount."},
    {"id": "create_nested", "check": c_create, "prompt":
     "In the tracker, create an issue in project ATLAS titled exactly \"Login button misaligned on Safari\" with the labels bug and ui, "
     "priority high, assigned to user mkim. Create it exactly once. Report the new issue's key."},
    {"id": "dns_chain", "check": c_dns, "prompt":
     "Using the cloud MCP server, find the IPv4 address that the `api` A record in the DNS zone example.org points to."},
    {"id": "docs_resource", "check": c_docs, "prompt":
     "The docs MCP server has a Q3 roadmap page. What is the public launch date of Project Heron according to it?"},
    {"id": "log_tail", "check": c_log, "prompt":
     "CI job 9917 failed at the link step. Using the ci MCP server, find which undefined symbol caused the final linker error."},
    {"id": "tickets_count", "check": c_tickets, "prompt":
     "How many OPEN support tickets does the customer with email dana.okafor@example.net have in the crm server? Answer with the number."},
    {"id": "adhoc_headers", "check": c_ship, "prompt":
     "There is an MCP server at {shipping_url} that is not in your configuration. It requires the HTTP header `X-Api-Key: k-5521` on every request. "
     "Use it to find the delivery status of order A1234."},
    {"id": "add_stdio", "check": c_kv, "prompt":
     "Register the stdio MCP server that is started with the command `{opt}/kvd --db {opt}/kv.json` under the name `kv`, "
     "then use it to look up the value stored under the key `launch-code`."},
    {"id": "enum_recover", "check": c_status, "prompt":
     "In the tracker, move issue ATLAS-17 into review, then report the status value the tracker now shows for it."},
    {"id": "unreachable", "check": c_unreachable, "prompt":
     "Which of the configured MCP servers are currently unreachable or failing to start? Give the answer as a comma-separated list of server names."},
    {"id": "token_cost", "check": c_stats, "prompt":
     "If every tool schema from all configured MCP servers were loaded natively into an agent's context, which single server would cost the most tokens? "
     "Answer with the server name."},
]


# ------------------------------------------------------------ holdout (never shown to the mutator)
def h_orbit(ctx):
    a = norm(ctx["answer"]) + " " + norm(ctx["final"])
    want = ["telemetry exporter drops spans under load", "retry budget ignored for grpc streams", "helm chart missing poddisruptionbudget"]
    return all(norm(w) in a for w in want) and "config reload leaks" not in a and "distroless" not in a, "3 newest ORBIT titles"


def h_vm(ctx):
    return "m7g.xlarge" in (ctx["answer"] or "").lower(), "web-03 eu-west type"


def h_vpn(ctx):
    return "vpn-gw2.corp.example.net" in (ctx["answer"] or "").lower(), "vpn hostname"


def h_invoice(ctx):
    return has_num(ctx["answer"], 1284.50, 0.01), "outstanding balance"


def h_stock(ctx):
    a = ctx["answer"] or ""
    return has_num(a, 1165, 0.5) and not has_num(a, 1375, 0.5), "available units"


HOLDOUT = [
    {"id": "h_orbit_recent", "check": h_orbit, "prompt":
     "List the titles of the three most recently CREATED issues in tracker project ORBIT, newest first."},
    {"id": "h_vm_type", "check": h_vm, "prompt":
     "What instance type is the compute instance web-03 in region eu-west, according to the cloud server?"},
    {"id": "h_vpn", "check": h_vpn, "prompt":
     "According to the engineering onboarding page on the docs server, what VPN hostname should new engineers connect to?"},
    {"id": "h_bearer", "check": h_invoice, "prompt":
     "The MCP server at {billing_url} is not configured locally and authenticates with the bearer token `tok-9931`. "
     "What is the outstanding balance on invoice INV-2207?"},
    {"id": "h_stock", "check": h_stock, "prompt":
     "How many units of SKU WX-4471 are AVAILABLE (not merely on hand) at warehouse AMS-2? Use the warehouse server."},
]
