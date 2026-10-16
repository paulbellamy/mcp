#!/usr/bin/env python3
"""Minimal MCP server (legacy initialize handshake) for the agent-UX bench.

stdio:  mockserver.py --profile NAME [--log DIR] [--db FILE]
http:   mockserver.py --profile NAME --http PORT [--log DIR]

Profiles live in profiles.py. Every tools/call is appended to
<log>/<profile>.jsonl so checks can verify side effects. FROZEN: part of
the scorer.
"""
import argparse
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import profiles  # noqa: E402

PAGE = 25


class Server:
    def __init__(self, name, log_dir=None, db=None):
        self.name = name
        self.log_dir = log_dir
        self.p = profiles.load(name, db=db, history=self.history())

    def history(self):
        """Prior successful tool calls to this profile (stdio servers are respawned per CLI call)."""
        if not self.log_dir:
            return []
        try:
            with open(os.path.join(self.log_dir, self.name + ".jsonl")) as f:
                recs = [json.loads(line) for line in f if line.strip()]
        except FileNotFoundError:
            return []
        return [r for r in recs if r.get("tool") and not r.get("isError")]

    def log(self, rec):
        if not self.log_dir:
            return
        os.makedirs(self.log_dir, exist_ok=True)
        with open(os.path.join(self.log_dir, self.name + ".jsonl"), "a") as f:
            f.write(json.dumps(rec) + "\n")

    def handle(self, msg):
        """Return a response dict, or None for notifications."""
        mid = msg.get("id")
        method = msg.get("method", "")
        params = msg.get("params") or {}
        if mid is None:
            return None
        try:
            result = self.dispatch(method, params)
        except RpcError as e:
            return {"jsonrpc": "2.0", "id": mid, "error": {"code": e.code, "message": e.msg}}
        return {"jsonrpc": "2.0", "id": mid, "result": result}

    def dispatch(self, method, params):
        p = self.p
        if method == "initialize":
            return {
                "protocolVersion": params.get("protocolVersion", "2025-06-18"),
                "capabilities": {"tools": {}, **({"resources": {}} if p.get("resources") or p.get("templates") else {})},
                "serverInfo": {"name": self.name, "version": "1.4.2"},
            }
        if method == "ping":
            return {}
        if method == "tools/list":
            tools = [{"name": t["name"], "description": t["description"], "inputSchema": t["inputSchema"]} for t in p["tools"]]
            start = int(params.get("cursor") or 0)
            page = tools[start:start + PAGE]
            out = {"tools": page}
            if start + PAGE < len(tools):
                out["nextCursor"] = str(start + PAGE)
            return out
        if method == "tools/call":
            name = params.get("name")
            args = params.get("arguments") or {}
            tool = next((t for t in p["tools"] if t["name"] == name), None)
            if tool is None:
                self.log({"tool": name, "arguments": args, "isError": True, "ts": time.time()})
                raise RpcError(-32602, f"Unknown tool: {name}")
            try:
                text = tool["handler"](args)
                is_err = False
            except profiles.ToolError as e:
                text, is_err = str(e), True
            self.log({"tool": name, "arguments": args, "isError": is_err, "ts": time.time()})
            if not isinstance(text, str):
                text = json.dumps(text, indent=2)
            return {"content": [{"type": "text", "text": text}], "isError": is_err}
        if method == "resources/list":
            return {"resources": [{k: r[k] for k in ("uri", "name", "description", "mimeType")} for r in p.get("resources", [])]}
        if method == "resources/templates/list":
            return {"resourceTemplates": p.get("templates", [])}
        if method == "resources/read":
            uri = params.get("uri", "")
            self.log({"resource": uri, "ts": time.time()})
            for r in p.get("resources", []):
                if r["uri"] == uri:
                    return {"contents": [{"uri": uri, "mimeType": r["mimeType"], "text": r["text"]}]}
            raise RpcError(-32002, f"Resource not found: {uri}")
        if method == "prompts/list":
            return {"prompts": []}
        raise RpcError(-32601, f"Method not found: {method}")


class RpcError(Exception):
    def __init__(self, code, msg):
        self.code, self.msg = code, msg


def serve_stdio(srv):
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        resp = srv.handle(msg)
        if resp is not None:
            sys.stdout.write(json.dumps(resp) + "\n")
            sys.stdout.flush()


def serve_http(srv, port):
    required = srv.p.get("required_headers", {})

    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _send(self, code, body, ctype="application/json"):
            data = body.encode() if isinstance(body, str) else body
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            self._send(405, json.dumps({"error": "method not allowed"}))

        def do_DELETE(self):
            self._send(200, "{}")

        def do_POST(self):
            for k, v in required.items():
                if self.headers.get(k) != v:
                    self._send(401, json.dumps({"error": f"unauthorized: missing or invalid {k} header"}))
                    return
            n = int(self.headers.get("Content-Length") or 0)
            try:
                msg = json.loads(self.rfile.read(n) or b"{}")
            except json.JSONDecodeError:
                self._send(400, json.dumps({"error": "bad json"}))
                return
            resp = srv.handle(msg)
            if resp is None:
                self.send_response(202)
                self.send_header("Content-Length", "0")
                self.end_headers()
                return
            self._send(200, json.dumps(resp))

    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--profile", required=True)
    ap.add_argument("--log")
    ap.add_argument("--db")
    ap.add_argument("--http", type=int)
    a = ap.parse_args()
    if a.profile == "broken":
        sys.stderr.write("legacy-search: fatal: search index unavailable (connection refused 10.4.0.9:9200)\n")
        sys.exit(1)
    srv = Server(a.profile, a.log, a.db)
    if a.http:
        serve_http(srv, a.http)
    else:
        serve_stdio(srv)


if __name__ == "__main__":
    main()
