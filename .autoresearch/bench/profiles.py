"""Server profiles (tool catalogues + data) for the agent-UX bench. FROZEN."""
import json
import random


class ToolError(Exception):
    pass


def S(props, required=()):
    return {"type": "object", "properties": props, "required": list(required)}


def _type_ok(v, t):
    return {
        "string": isinstance(v, str),
        "integer": isinstance(v, int) and not isinstance(v, bool),
        "number": isinstance(v, (int, float)) and not isinstance(v, bool),
        "boolean": isinstance(v, bool),
        "array": isinstance(v, list),
        "object": isinstance(v, dict),
    }.get(t, True)


def validate(schema, args, path=""):
    for r in schema.get("required", []):
        if r not in args:
            raise ToolError(f"Invalid arguments: '{path}{r}' is required")
    for k, v in args.items():
        ps = schema.get("properties", {}).get(k)
        if ps is None:
            if schema.get("additionalProperties") is False:
                raise ToolError(f"Invalid arguments: unknown property '{path}{k}'")
            continue
        t = ps.get("type")
        if t and not _type_ok(v, t):
            raise ToolError(f"Invalid arguments: '{path}{k}' must be of type {t}, got {type(v).__name__}")
        if "enum" in ps and v not in ps["enum"]:
            raise ToolError(f"Invalid arguments: '{path}{k}' must be one of {ps['enum']}, got {v!r}")
        if t == "integer":
            if "minimum" in ps and v < ps["minimum"] or "maximum" in ps and v > ps["maximum"]:
                raise ToolError(f"Invalid arguments: '{path}{k}' out of range [{ps.get('minimum')}, {ps.get('maximum')}]")
        if t == "array" and "items" in ps:
            for i, item in enumerate(v):
                it = ps["items"].get("type")
                if it and not _type_ok(item, it):
                    raise ToolError(f"Invalid arguments: '{path}{k}[{i}]' must be of type {it}")
        if t == "object" and "properties" in ps:
            validate(ps, v, path + k + ".")


def tool(name, description, schema, handler=None):
    def h(args):
        validate(schema, args)
        if handler is None:
            return {"ok": True}
        return handler(args)
    return {"name": name, "description": description, "inputSchema": schema, "handler": h}


# ---------------------------------------------------------------- weather
WEATHER = {
    "lisbon": {"now": (18.2, "partly cloudy", 14), "hi": [19.4, 21.0, 20.3, 18.8, 17.9, 19.1, 20.6], "lo": [13.1, 14.0, 13.6, 12.9, 12.2, 12.8, 13.5]},
    "oslo": {"now": (5.3, "light rain", 22), "hi": [8.4, 6.1, 9.7, 7.2, 5.5, 6.8, 7.9], "lo": [2.1, 0.4, 3.3, 1.8, -0.6, 0.9, 1.2]},
    "nairobi": {"now": (23.9, "sunny", 9), "hi": [26.2, 25.8, 27.1, 26.6, 25.0, 24.7, 26.0], "lo": [13.8, 14.2, 14.9, 13.5, 13.1, 13.9, 14.4]},
    "denver": {"now": (11.6, "clear", 17), "hi": [16.3, 12.8, 9.4, 14.1, 18.2, 17.5, 15.0], "lo": [1.2, -1.4, -3.0, 0.8, 3.3, 2.7, 1.9]},
    "osaka": {"now": (21.4, "cloudy", 11), "hi": [23.0, 22.4, 24.1, 21.9, 20.8, 22.6, 23.3], "lo": [16.2, 15.8, 17.0, 15.1, 14.6, 15.9, 16.4]},
}
CONDS = ["sunny", "partly cloudy", "cloudy", "light rain", "showers", "clear", "windy"]
DATES = ["2026-10-17", "2026-10-18", "2026-10-19", "2026-10-20", "2026-10-21", "2026-10-22", "2026-10-23"]


def f2(c):
    return round(c * 9 / 5 + 32, 1)


def _city(args):
    c = WEATHER.get(args["city"].strip().lower().split(",")[0])
    if not c:
        raise ToolError(f"Unknown city: {args['city']!r}. Known cities: {', '.join(x.title() for x in WEATHER)}")
    return c


def weather():
    units = {"type": "string", "enum": ["metric", "imperial"], "default": "metric", "description": "metric = °C, km/h; imperial = °F, mph"}
    city = {"type": "string", "description": "City name, e.g. 'Lisbon'"}

    def current(a):
        c, imp = _city(a), a.get("units") == "imperial"
        t, cond, wind = c["now"]
        return {"city": a["city"].title(), "temperature": f2(t) if imp else t, "conditions": cond,
                "wind": round(wind * 0.621, 1) if imp else wind, "units": "imperial" if imp else "metric"}

    def forecast(a):
        c, imp = _city(a), a.get("units") == "imperial"
        n = a.get("days", 3)
        days = []
        for i in range(n):
            hi, lo = c["hi"][i], c["lo"][i]
            days.append({"date": DATES[i], "high": f2(hi) if imp else hi, "low": f2(lo) if imp else lo,
                         "conditions": CONDS[(i * 3 + len(a["city"])) % len(CONDS)]})
        return {"city": a["city"].title(), "units": "imperial" if imp else "metric", "days": days}

    return {"tools": [
        tool("get_current_conditions", "Current observed weather conditions for a city.", S({"city": city, "units": units}, ["city"]), current),
        tool("get_forecast", "Daily forecast (high/low/conditions) for a city, starting tomorrow.",
             S({"city": city, "days": {"type": "integer", "minimum": 1, "maximum": 7, "default": 3, "description": "Number of days"}, "units": units}, ["city"]), forecast),
        tool("list_stations", "List weather stations in a country.", S({"country": {"type": "string", "description": "ISO 3166 alpha-2 code"}}, ["country"]),
             lambda a: {"stations": [f"{a['country'].upper()}-{i:03d}" for i in range(1, 6)]}),
        tool("get_alerts", "Active severe-weather alerts for a region.", S({"region": {"type": "string"}}, ["region"]), lambda a: {"alerts": []}),
    ]}


# ---------------------------------------------------------------- finance
RATES = {"USD": 1.0, "EUR": 0.9213, "JPY": 150.42, "GBP": 0.7861, "CHF": 0.8810, "SEK": 10.47, "CAD": 1.3702}


def convert(amount, frm, to):
    return round(amount / RATES[frm] * RATES[to], 2)


def finance():
    def conv(a):
        f, t = a["from"].upper(), a["to"].upper()
        for c in (f, t):
            if c not in RATES:
                raise ToolError(f"Unsupported currency {c!r}")
        return {"amount": a["amount"], "from": f, "to": t, "rate": round(RATES[t] / RATES[f], 6), "result": convert(a["amount"], f, t), "as_of": "2026-10-16"}

    cur = {"type": "string", "description": "ISO 4217 code"}
    return {"tools": [
        tool("convert_currency", "Convert an amount between currencies at today's reference rate.",
             S({"amount": {"type": "number"}, "from": cur, "to": cur}, ["amount", "from", "to"]), conv),
        tool("get_exchange_rates", "Reference exchange rates for a base currency.", S({"base": cur}, ["base"]),
             lambda a: {"base": a["base"].upper(), "rates": {k: round(v / RATES.get(a["base"].upper(), 1), 6) for k, v in RATES.items()}}),
        tool("stock_quote", "Latest quote for a ticker symbol.", S({"symbol": {"type": "string"}}, ["symbol"]),
             lambda a: {"symbol": a["symbol"].upper(), "price": 187.31, "change_pct": -0.42}),
        tool("market_hours", "Trading hours for an exchange.", S({"exchange": {"type": "string"}}, ["exchange"]),
             lambda a: {"exchange": a["exchange"].upper(), "open": "09:30", "close": "16:00", "tz": "America/New_York"}),
    ]}


# ---------------------------------------------------------------- tracker
ISSUES = [
    ("ATLAS-12", "ATLAS", "Password reset email has broken link", "done", "2026-08-02", "2026-09-01"),
    ("ATLAS-15", "ATLAS", "Profile avatar upload fails over 2MB", "todo", "2026-08-19", "2026-10-02"),
    ("ATLAS-17", "ATLAS", "Dark mode toggle resets on reload", "in_progress", "2026-09-03", "2026-10-15"),
    ("ATLAS-21", "ATLAS", "Session timeout too aggressive on mobile", "in_review", "2026-09-22", "2026-10-12"),
    ("ORBIT-88", "ORBIT", "Helm chart missing PodDisruptionBudget", "todo", "2026-10-09", "2026-10-09"),
    ("ORBIT-86", "ORBIT", "Retry budget ignored for gRPC streams", "in_progress", "2026-10-11", "2026-10-13"),
    ("ORBIT-91", "ORBIT", "Telemetry exporter drops spans under load", "todo", "2026-10-14", "2026-10-14"),
    ("ORBIT-80", "ORBIT", "Config reload leaks file descriptors", "done", "2026-09-28", "2026-10-15"),
    ("ORBIT-77", "ORBIT", "Upgrade base image to distroless", "in_review", "2026-09-20", "2026-10-16"),
    ("HELIX-5", "HELIX", "Nightly ETL misses late-arriving rows", "todo", "2026-10-01", "2026-10-03"),
]
STATUSES = ["todo", "in_progress", "in_review", "done"]


def _issue(t):
    return {"key": t[0], "project": t[1], "title": t[2], "status": t[3], "created": t[4], "updated": t[5]}


def tracker(history=()):
    issues = [list(t) for t in ISSUES]
    for h in history:
        a = h.get("arguments", {})
        if h["tool"] == "update_issue_status":
            for t in issues:
                if t[0] == str(a.get("key", "")).upper():
                    t[3], t[5] = a.get("status"), "2026-10-16"
        elif h["tool"] == "create_issue":
            pk = str(a.get("project", "")).upper()
            n = {"ATLAS": 482, "ORBIT": 92, "HELIX": 6}.get(pk)
            if n and not any(t[0] == f"{pk}-{n}" for t in issues):
                issues.append([f"{pk}-{n}", pk, a.get("title", ""), "todo", "2026-10-16", "2026-10-16"])
    proj = {"type": "string", "description": "Project key, e.g. ATLAS"}
    key = {"type": "string", "description": "Issue key, e.g. ATLAS-12"}

    def get(a):
        for t in issues:
            if t[0] == a["key"].upper():
                return _issue(t)
        raise ToolError(f"Issue not found: {a['key']}")

    def lst(a):
        rows = [_issue(t) for t in issues if t[1] == a["project"].upper()]
        if not rows:
            raise ToolError(f"Unknown project {a['project']!r}")
        s = a.get("sort", "updated_desc")
        field, rev = ("created" if s.startswith("created") else "updated"), s.endswith("desc")
        rows.sort(key=lambda r: r[field], reverse=rev)
        return {"issues": rows[: a.get("limit", 20)]}

    def search(a):
        q = a.get("query", "").lower()
        rows = [_issue(t) for t in issues if (not a.get("project") or t[1] == a["project"].upper()) and q in t[2].lower()
                and (not a.get("status") or t[3] == a["status"])]
        return {"issues": rows[: a.get("limit", 20)]}

    def create(a):
        p = a["project"].upper()
        if p not in {"ATLAS", "ORBIT", "HELIX"}:
            raise ToolError(f"Unknown project {a['project']!r}")
        n = {"ATLAS": 482, "ORBIT": 92, "HELIX": 6}[p]
        return {"key": f"{p}-{n}", "url": f"https://tracker.example.com/browse/{p}-{n}", "title": a["title"],
                "labels": a.get("labels", []), "fields": a.get("fields", {}), "status": "todo"}

    def status(a):
        i = get(a)
        return {**i, "status": a["status"], "previous_status": i["status"], "updated": "2026-10-16"}

    fields = {"type": "object", "description": "Additional issue fields", "properties": {
        "priority": {"type": "string", "enum": ["low", "medium", "high", "urgent"]},
        "assignee": {"type": "string", "description": "Username of the assignee"},
        "due_date": {"type": "string", "description": "YYYY-MM-DD"}}}
    return {"tools": [
        tool("list_projects", "List projects visible to you.", S({}), lambda a: {"projects": [{"key": "ATLAS", "name": "Atlas Web"}, {"key": "ORBIT", "name": "Orbit Platform"}, {"key": "HELIX", "name": "Helix Data"}]}),
        tool("list_issues", "List issues in a project.", S({"project": proj, "sort": {"type": "string", "enum": ["updated_desc", "created_desc", "created_asc"], "default": "updated_desc"}, "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20}}, ["project"]), lst),
        tool("search_issues", "Full-text search over issue titles.", S({"query": {"type": "string"}, "project": proj, "status": {"type": "string", "enum": STATUSES}, "limit": {"type": "integer", "minimum": 1, "maximum": 50}}, ["query"]), search),
        tool("get_issue", "Fetch one issue by key.", S({"key": key}, ["key"]), get),
        tool("create_issue", "Create a new issue.", S({"project": proj, "title": {"type": "string"}, "description": {"type": "string"},
                                                       "labels": {"type": "array", "items": {"type": "string"}}, "fields": fields}, ["project", "title"]), create),
        tool("update_issue_status", "Transition an issue to a new workflow status.", S({"key": key, "status": {"type": "string", "enum": STATUSES}}, ["key", "status"]), status),
        tool("add_comment", "Add a comment to an issue.", S({"key": key, "body": {"type": "string"}}, ["key", "body"]), lambda a: {"ok": True, "comment_id": 99121}),
    ]}


# ---------------------------------------------------------------- cloud
ZONES = [{"id": "z-7f3a91", "name": "example.org"}, {"id": "z-11c0d2", "name": "example.com"}, {"id": "z-5be777", "name": "internal.example.org"}]
RECORDS = {
    "z-7f3a91": [{"name": "api", "type": "A", "value": "203.0.113.47", "ttl": 300}, {"name": "api", "type": "AAAA", "value": "2001:db8::2f", "ttl": 300},
                 {"name": "www", "type": "CNAME", "value": "edge.example.net", "ttl": 3600}, {"name": "@", "type": "MX", "value": "10 mx1.example.org", "ttl": 3600}],
    "z-11c0d2": [{"name": "api", "type": "A", "value": "198.51.100.12", "ttl": 300}, {"name": "www", "type": "A", "value": "198.51.100.13", "ttl": 300}],
    "z-5be777": [{"name": "api", "type": "A", "value": "10.20.0.15", "ttl": 60}],
}
INSTANCES = [
    {"name": "web-01", "region": "eu-west", "type": "m7g.large", "state": "running"},
    {"name": "web-03", "region": "eu-west", "type": "m7g.xlarge", "state": "running"},
    {"name": "web-03", "region": "us-east", "type": "c6i.large", "state": "stopped"},
    {"name": "db-01", "region": "eu-west", "type": "r7g.2xlarge", "state": "running"},
]


def cloud():
    region = {"type": "string", "enum": ["us-east", "us-west", "eu-west", "ap-south"], "description": "Region"}
    zid = {"type": "string", "description": "Hosted zone ID (e.g. z-1a2b3c)"}

    def zone_check(a):
        if not any(z["id"] == a["zone_id"] for z in ZONES):
            raise ToolError(f"Zone not found: {a['zone_id']!r} (expected a hosted zone ID like z-1a2b3c)")
        return a["zone_id"]

    def inst_get(a):
        for i in INSTANCES:
            if i["name"] == a["name"] and i["region"] == a["region"]:
                return i
        raise ToolError(f"Instance {a['name']!r} not found in {a['region']}")

    special = {
        "dns_zone_list": ("List hosted DNS zones.", S({}), lambda a: {"zones": ZONES}),
        "dns_zone_get": ("Get a hosted DNS zone.", S({"zone_id": zid}, ["zone_id"]), lambda a: next(z for z in ZONES if z["id"] == zone_check(a))),
        "dns_record_list": ("List DNS records in a hosted zone.", S({"zone_id": zid, "type": {"type": "string", "description": "Filter by record type"}}, ["zone_id"]),
                            lambda a: {"records": [r for r in RECORDS[zone_check(a)] if not a.get("type") or r["type"] == a["type"].upper()]}),
        "compute_instance_list": ("List compute instances in a region.", S({"region": region}, ["region"]),
                                  lambda a: {"instances": [{"name": i["name"], "state": i["state"]} for i in INSTANCES if i["region"] == a["region"]]}),
        "compute_instance_get": ("Get details for a compute instance.", S({"name": {"type": "string"}, "region": region}, ["name", "region"]), inst_get),
    }
    resources = {
        "compute": ["instance", "image", "disk", "snapshot"], "storage": ["bucket", "object", "lifecycle_rule"],
        "dns": ["zone", "record"], "iam": ["user", "role", "policy", "access_key"], "network": ["vpc", "subnet", "firewall_rule", "load_balancer"],
        "db": ["cluster", "backup"], "k8s": ["cluster", "nodepool"], "queue": ["topic", "subscription"], "monitoring": ["alert_policy", "dashboard"],
        "billing": ["budget"],
    }
    verbs = [("list", "List {r}s"), ("get", "Get a {r}"), ("create", "Create a {r}"), ("delete", "Delete a {r}")]
    tools = []
    for svc, rs in resources.items():
        for r in rs:
            for v, d in verbs:
                name = f"{svc}_{r}_{v}"
                if name in special:
                    desc, sch, h = special[name]
                    tools.append(tool(name, desc, sch, h))
                    continue
                if svc == "billing" and v != "list":
                    continue
                desc = d.format(r=r.replace("_", " ")) + f" ({svc})."
                props = {"region": region}
                req = []
                if v in ("get", "delete"):
                    props["id"] = {"type": "string", "description": f"{r} ID"}
                    req = ["id"]
                if v == "create":
                    props["name"] = {"type": "string"}
                    props["labels"] = {"type": "object", "description": "Key/value labels"}
                    req = ["name"]
                if v == "list":
                    props["page_size"] = {"type": "integer", "minimum": 1, "maximum": 500}
                    props["page_token"] = {"type": "string"}
                tools.append(tool(name, desc, S(props, req), (lambda a: {"items": [], "next_page_token": None}) if v == "list" else None))
    return {"tools": tools}


# ---------------------------------------------------------------- docs (resources only)
DOCS = {
    "q3-roadmap": ("Q3 Roadmap", "# Q3 Roadmap\n\n## Project Heron\nSelf-serve onboarding flow. Public launch: **2026-11-18**. Owner: Priya N.\n\n"
                   "## Project Kestrel\nUsage-based billing. Beta: 2026-12-02. Owner: Tom W.\n\n## Project Wren\nMobile offline mode. Discovery phase; no date.\n"),
    "onboarding": ("Engineering Onboarding", "# Engineering Onboarding\n\n1. Request laptop via IT portal.\n2. Connect to the VPN: hostname `vpn-gw2.corp.example.net`, "
                   "profile `eng-default`.\n3. Join #eng-help.\n4. Clone the monorepo and run `make bootstrap`.\n"),
    "incident-2026-09-14": ("Incident review: 2026-09-14 checkout outage", "# Incident review\n\nDuration 47 minutes. Root cause: expired TLS cert on payments-gw.\n"),
    "style-guide": ("Writing style guide", "# Style guide\n\nUse sentence case for headings. Prefer active voice.\n"),
}


def docs():
    return {
        "tools": [tool("get_space_info", "Information about this documentation space.", S({}), lambda a: {"space": "Engineering", "pages": len(DOCS), "updated": "2026-10-15"})],
        "resources": [{"uri": f"docs://pages/{k}", "name": v[0], "description": f"Docs page: {v[0]}", "mimeType": "text/markdown", "text": v[1]} for k, v in DOCS.items()],
        "templates": [{"uriTemplate": "docs://pages/{slug}", "name": "Docs page by slug", "mimeType": "text/markdown"}],
    }


# ---------------------------------------------------------------- ci
def build_log(job):
    rnd = random.Random(job)
    lines = [f"[ci] job {job} started on runner rx-{rnd.randint(10, 99)}", "[ci] checkout ok (a9f3c21)", "configure: checking for zlib... yes",
             "configure: checking for zstd... yes (1.5.6)"]
    for i in range(30):
        lines.append(f"[{i // 60:02d}:{i % 60:02d}] CC   src/core/mod_{i:03d}.c")
    lines.append("warning: ld: undefined symbol: _crc32_legacy (weak, ignored)")
    mods = ["core", "net", "io", "codec", "archive", "util", "crypto"]
    for i in range(30, 2300):
        m = mods[rnd.randrange(len(mods))]
        lines.append(f"[{i // 60:02d}:{i % 60:02d}] CC   src/{m}/{m}_{rnd.randint(0, 999):03d}.c -O2 -fPIC -Wall")
    lines += ["[38:20] LD   build/bin/packd", "ld: error: undefined symbol: _zstd_frame_decode", ">>> referenced by archive_read.o:(archive_read_next)",
              "ERROR: linker failed with exit code 1", "[ci] job failed after 14m02s"]
    return "\n".join(lines)


def ci():
    jid = {"type": "integer", "description": "Job ID"}

    def log(a):
        if a["job_id"] not in (9917, 9916, 9915):
            raise ToolError(f"Job {a['job_id']} not found")
        return build_log(a["job_id"]) if a["job_id"] == 9917 else "[ci] job ok\n[ci] job finished after 9m12s"

    return {"tools": [
        tool("list_jobs", "List recent CI jobs.", S({"pipeline": {"type": "string"}, "limit": {"type": "integer"}}),
             lambda a: {"jobs": [{"id": 9917, "pipeline": "packd", "status": "failed"}, {"id": 9916, "pipeline": "packd", "status": "passed"}, {"id": 9915, "pipeline": "web", "status": "passed"}]}),
        tool("get_job", "Get a CI job's metadata.", S({"job_id": jid}, ["job_id"]), lambda a: {"id": a["job_id"], "status": "failed" if a["job_id"] == 9917 else "passed", "duration_s": 842}),
        tool("get_job_log", "Fetch the full raw log of a CI job.", S({"job_id": jid}, ["job_id"]), log),
        tool("retry_job", "Retry a CI job.", S({"job_id": jid}, ["job_id"])),
    ]}


# ---------------------------------------------------------------- crm
TICKETS = [("T-5101", "open", "Invoice PDF missing VAT line"), ("T-5102", "closed", "Password reset"), ("T-5104", "pending", "Seat count mismatch"),
           ("T-5107", "open", "SSO login loop on Firefox"), ("T-5110", "closed", "Export to CSV slow"), ("T-5111", "closed", "Change billing email"),
           ("T-5115", "open", "Webhook retries not firing"), ("T-5118", "pending", "Data residency question"), ("T-5120", "closed", "Add teammate")]


def crm():
    def find(a):
        if not a.get("email") and not a.get("name"):
            raise ToolError("Provide email or name")
        if (a.get("email") or "").lower() == "dana.okafor@example.net" or "okafor" in (a.get("name") or "").lower():
            return {"customers": [{"id": "cus_88213", "name": "Dana Okafor", "email": "dana.okafor@example.net", "plan": "business"}]}
        return {"customers": []}

    def tickets(a):
        if a["customer_id"] != "cus_88213":
            raise ToolError(f"Customer not found: {a['customer_id']!r} (expected an ID like cus_12345)")
        st = a.get("status", "all")
        rows = [{"id": t[0], "status": t[1], "subject": t[2]} for t in TICKETS if st == "all" or t[1] == st]
        size, page = a.get("page_size", 5), a.get("page", 1)
        out = {"tickets": rows[(page - 1) * size: page * size], "page": page, "total": len(rows)}
        if page * size < len(rows):
            out["next_page"] = page + 1
        return out

    return {"tools": [
        tool("find_customer", "Look up customers by email or name.", S({"email": {"type": "string"}, "name": {"type": "string"}}), find),
        tool("get_customer", "Get a customer by ID.", S({"customer_id": {"type": "string"}}, ["customer_id"]), lambda a: {"id": a["customer_id"]}),
        tool("list_tickets", "List support tickets for a customer.", S({"customer_id": {"type": "string"},
                                                                        "status": {"type": "string", "enum": ["open", "pending", "closed", "all"], "default": "all"},
                                                                        "page": {"type": "integer", "minimum": 1, "default": 1},
                                                                        "page_size": {"type": "integer", "minimum": 1, "maximum": 50, "default": 5}}, ["customer_id"]), tickets),
        tool("create_ticket", "Open a support ticket.", S({"customer_id": {"type": "string"}, "subject": {"type": "string"}}, ["customer_id", "subject"])),
    ]}


# ---------------------------------------------------------------- warehouse (few tools, enormous schemas)
SKUS = sorted({f"WX-{n:04d}" for n in random.Random(7).sample(range(1000, 9999), 640)} | {"WX-4471"})
WAREHOUSES = [f"{c}-{i}" for c in ("AMS", "FRA", "LHR", "JFK", "ORD", "SIN", "NRT", "SYD", "GRU", "JNB", "YYZ", "MAD") for i in range(1, 11)]
LONG = ("Identifier as issued by the master data service. Values are validated against the current catalogue snapshot, which is refreshed "
        "nightly at 02:00 UTC; identifiers retired in the last 30 days remain accepted for reads but are rejected for writes. ")


def warehouse():
    sku = {"type": "string", "enum": SKUS, "description": "Stock keeping unit. " + LONG}
    wh = {"type": "string", "enum": WAREHOUSES, "description": "Warehouse code. " + LONG}

    def q(a):
        if a["sku"] == "WX-4471" and a["warehouse"] == "AMS-2":
            return {"sku": "WX-4471", "warehouse": "AMS-2", "on_hand": 1375, "reserved": 210, "available": 1165}
        return {"sku": a["sku"], "warehouse": a["warehouse"], "on_hand": 0, "reserved": 0, "available": 0}

    def p(extra=None):
        d = {"sku": sku, "warehouse": wh}
        d.update(extra or {})
        return d
    qty = {"type": "integer", "description": "Quantity in base units. " + LONG}
    return {"tools": [
        tool("inventory_query", "Query stock levels for a SKU at a warehouse.", S(p(), ["sku", "warehouse"]), q),
        tool("inventory_adjust", "Adjust stock for a SKU at a warehouse.", S(p({"delta": qty, "reason": {"type": "string", "description": LONG}}), ["sku", "warehouse", "delta"])),
        tool("transfer_create", "Create a stock transfer between warehouses.", S(p({"to_warehouse": wh, "quantity": qty}), ["sku", "warehouse", "to_warehouse", "quantity"])),
        tool("shipment_create", "Create an outbound shipment.", S(p({"quantity": qty, "address": {"type": "string", "description": LONG}}), ["sku", "warehouse", "quantity"])),
        tool("sku_lookup", "Look up product data for a SKU.", S({"sku": sku}, ["sku"]), lambda a: {"sku": a["sku"], "name": "Widget"}),
        tool("cycle_count_schedule", "Schedule a cycle count.", S(p({"date": {"type": "string", "description": LONG}}), ["sku", "warehouse"])),
    ]}


# ---------------------------------------------------------------- ad-hoc HTTP servers
def shipping():
    def st(a):
        if a["order_id"].lstrip("#").upper() == "A1234":
            return {"order_id": "A1234", "status": "out_for_delivery", "carrier": "PostNL", "eta": "2026-10-17", "last_scan": "Rotterdam hub"}
        raise ToolError(f"Order not found: {a['order_id']}")
    return {"required_headers": {"X-Api-Key": "k-5521"}, "tools": [
        tool("get_order_status", "Delivery status for an order.", S({"order_id": {"type": "string"}}, ["order_id"]), st),
        tool("list_carriers", "Carriers we ship with.", S({}), lambda a: {"carriers": ["PostNL", "DHL", "UPS"]}),
    ]}


def billing():
    def inv(a):
        if a["invoice_id"].upper() == "INV-2207":
            return {"invoice_id": "INV-2207", "customer": "Northwind BV", "total": "3,120.00 EUR", "paid": "1,835.50 EUR", "outstanding": "1,284.50 EUR", "due": "2026-10-31"}
        raise ToolError(f"Invoice not found: {a['invoice_id']}")
    return {"required_headers": {"Authorization": "Bearer tok-9931"}, "tools": [
        tool("get_invoice", "Fetch an invoice with payment status.", S({"invoice_id": {"type": "string"}}, ["invoice_id"]), inv),
        tool("list_invoices", "List invoices.", S({"status": {"type": "string", "enum": ["open", "paid", "overdue"]}}), lambda a: {"invoices": ["INV-2207", "INV-2210"]}),
    ]}


# ---------------------------------------------------------------- kv (stdio, added by the agent)
def kv(db):
    def load():
        with open(db) as f:
            return json.load(f)

    def get(a):
        d = load()
        if a["key"] not in d:
            raise ToolError(f"Key not found: {a['key']!r}")
        return {"key": a["key"], "value": d[a["key"]]}
    return {"tools": [
        tool("kv_get", "Get the value for a key.", S({"key": {"type": "string"}}, ["key"]), get),
        tool("kv_list", "List keys (optionally by prefix).", S({"prefix": {"type": "string"}}), lambda a: {"keys": sorted(k for k in load() if k.startswith(a.get("prefix", "")))}),
        tool("kv_set", "Set a key.", S({"key": {"type": "string"}, "value": {"type": "string"}}, ["key", "value"])),
    ]}


PROFILES = {"weather": weather, "finance": finance, "tracker": tracker, "cloud": cloud, "docs": docs, "ci": ci, "crm": crm,
            "warehouse": warehouse, "shipping": shipping, "billing": billing}


def load(name, db=None, history=()):
    if name == "kv":
        return kv(db)
    if name == "tracker":
        return tracker(history)
    return PROFILES[name]()
