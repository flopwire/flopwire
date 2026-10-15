#!/usr/bin/env python3
"""Small synthetic retrieval pilot. See notes/competitive-benchmark/README.md.

Uses only the standard library. All product state lives below --root.
Langfuse must be provisioned separately; --langfuse-env reads its local
headless initialization file without writing credentials into the report.
"""

import argparse
import base64
import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import platform
import random
import re
import sqlite3
import statistics
import subprocess
import time
import urllib.parse
import urllib.request
import uuid


def uid(value):
    return str(uuid.uuid5(uuid.NAMESPACE_URL, "flopwire-competitive-pilot-v1/" + value))


def dump(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + "\n")


def env_for(home):
    # Explicit allow-list prevents inheriting live product credentials/config.
    env = {k: os.environ[k] for k in ("PATH", "TMPDIR", "LANG", "TERM") if k in os.environ}
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               XDG_DATA_HOME=str(home / ".local/share"),
               XDG_CACHE_HOME=str(home / ".cache"), XDG_STATE_HOME=str(home / ".state"),
               CLAUDE_CONFIG_DIR=str(home / ".claude"), CODEX_HOME=str(home / ".codex"),
               FLOPWIRE_CONFIG=str(home / ".config/flopwire/config.json"),
               FLOPWIRE_CLOUD="off", FLOPWIRE_OPENCODE_DB="-", OPENCODE_DB="-")
    return env


def invoke(cmd, env, stdin=None, timeout=60):
    started = time.perf_counter()
    p = subprocess.run(cmd, env=env, input=stdin, text=True, capture_output=True,
                       timeout=timeout)
    elapsed = (time.perf_counter() - started) * 1000
    if p.returncode:
        raise RuntimeError(f"{Path(cmd[0]).name} {cmd[1:]}: exit {p.returncode}: "
                           + p.stderr[-1500:])
    return p.stdout, elapsed


def native_rows(session, messages):
    sid, harness = session["id"], session["harness"]
    cwd = "/workspace/pilot-" + session["repo"]
    if harness == "codex":
        rows = [{"timestamp": messages[0]["at"], "type": "session_meta", "payload": {
            "id": sid, "session_id": sid, "timestamp": messages[0]["at"],
            "cwd": cwd, "originator": "codex-cli", "cli_version": "0.154.0",
            "source": "cli", "git": {"branch": "main",
                "repository_url": "https://example.invalid/pilot/" + session["repo"] + ".git"}}}]
        for m in messages:
            if m["kind"] == "tool_result":
                rows.extend([
                    {"timestamp": m["at"], "type": "response_item", "payload": {
                        "type": "function_call", "name": "exec_command",
                        "call_id": m["id"], "arguments": '{"cmd":"echo fixture"}'}},
                    {"timestamp": m["at"], "type": "response_item", "payload": {
                        "type": "function_call_output", "call_id": m["id"], "output": m["text"]}}])
            else:
                rows.append({"timestamp": m["at"], "type": "response_item", "payload": {
                    "type": "message", "id": m["id"], "role": m["kind"],
                    "content": [{"type": "input_text" if m["kind"] == "user" else "output_text",
                                 "text": m["text"]}]}})
        return rows
    rows, parent = [], None
    for m in messages:
        if m["kind"] == "tool_result":
            call_id = uid(m["id"] + "/call")
            rows.append({"type": "assistant", "uuid": call_id, "parentUuid": parent,
                "sessionId": sid, "cwd": cwd, "timestamp": m["at"], "isSidechain": False,
                "message": {"id": call_id, "role": "assistant", "model": "fixture-model",
                    "content": [{"type": "tool_use", "id": m["id"], "name": "Bash",
                                 "input": {"command": "echo fixture"}}]}})
            parent = call_id
            role = "user"
            content = [{"type": "tool_result", "tool_use_id": m["id"], "content": m["text"]}]
        else:
            role, content = m["kind"], m["text"]
        rows.append({"type": role, "uuid": m["id"], "parentUuid": parent,
            "sessionId": sid, "cwd": cwd, "timestamp": m["at"], "isSidechain": False,
            "gitBranch": "main", "version": "2.1.0", "userType": "external",
            "message": {"id": m["id"], "role": role, "content": content,
                        **({"model": "fixture-model"} if role == "assistant" else {})}})
        parent = m["id"]
    return rows


def corpus(root):
    queries = [
        {"name": "identifier", "query": "atlasquokka", "kind": "literal"},
        {"name": "path", "query": "internal/zircon/jaxflux.go", "kind": "literal"},
        {"name": "tool-output", "query": "jinxedtoolsentinel", "kind": "literal"},
        {"name": "unicode", "query": "cafépiñata", "kind": "literal"},
        {"name": "phrase", "query": "retry jitter", "kind": "literal"},
        {"name": "regex", "query": "ZQ-[0-9]{4}", "kind": "regex"},
    ]
    sessions, hashes = [], {}
    for i in range(48):
        harness = "claude" if i < 24 else "codex"
        s = {"id": uid(f"session/{i}"), "harness": harness,
             "repo": "alpha" if i % 2 == 0 else "beta"}
        messages = []
        for j in range(20):
            kind = "tool_result" if j == 13 else "user" if j % 2 == 0 else "assistant"
            text = (f"Session {i}, turn {j}. Parser buffer worker ledger index. " * 40).strip()
            if i % 24 < 6 and j == (13 if i % 24 == 2 else 7):
                q = queries[i % 24]
                text += "\n" + ("ZQ-1042" if q["kind"] == "regex" else q["query"])
                if i % 24 == 4:
                    text += ": bounded randomness prevents synchronized client retries."
            messages.append({"id": uid(f"message/{i}/{j}"), "session_id": s["id"],
                "harness": harness, "kind": kind, "ordinal": j, "text": text,
                "at": f"2026-10-13T12:{i:02d}:{j:02d}.000Z"})
        s["messages"] = messages
        home = root / "corpus"
        if harness == "claude":
            path = home / ".claude/projects" / ("-workspace-pilot-" + s["repo"]) / (s["id"] + ".jsonl")
        else:
            path = home / ".codex/sessions/2026/10/13" / ("rollout-2026-10-13T12-00-00-" + s["id"] + ".jsonl")
        path.parent.mkdir(parents=True, exist_ok=True)
        data = "".join(json.dumps(r, ensure_ascii=False) + "\n" for r in native_rows(s, messages))
        path.write_text(data)
        s["source"] = str(path.relative_to(root))
        hashes[s["source"]] = hashlib.sha256(data.encode()).hexdigest()
        sessions.append(s)
    for q in queries:
        pattern = re.compile(q["query"] if q["kind"] == "regex" else re.escape(q["query"]))
        q["expected_sessions"] = sorted({s["id"] for s in sessions
            if any(pattern.search(m["text"]) for m in s["messages"])})
        assert len(q["expected_sessions"]) == 2
    manifest = {"version": 1, "sessions": sessions, "queries": queries, "sha256": hashes,
                "messages": 960, "bytes": sum((root / p).stat().st_size for p in hashes)}
    dump(root / "manifest.json", manifest)
    return manifest


def session_ids(value):
    """Read attribution fields, never UUIDs mentioned in snippets or prompts."""
    found = set()
    if isinstance(value, dict):
        for k, v in value.items():
            if k in ("session", "session_id", "sessionId") and isinstance(v, str):
                # AgentsView namespaces Codex IDs; strip only a known format prefix.
                found.add(v.removeprefix("codex:"))
            elif isinstance(v, (dict, list)):
                found.update(session_ids(v))
    elif isinstance(value, list):
        for item in value:
            found.update(session_ids(item))
    return found


def correctness(expected, actual):
    expected, actual = set(expected), set(actual)
    correct = expected & actual
    return {"recall": len(correct) / len(expected),
            "precision": len(correct) / len(actual) if actual else 0,
            "missing": sorted(expected - actual), "extra": sorted(actual - expected),
            "pass": expected == actual}


def summary(samples):
    values = sorted(samples)
    return {"n": len(values), "p50_ms": statistics.median(values),
            "p95_ms": values[math.ceil(len(values) * .95) - 1]}


def validate_archive(path, table, id_column, manifest, expected_rows):
    with sqlite3.connect("file:" + str(path) + "?mode=ro", uri=True) as db:
        sessions = {row[0].removeprefix("codex:") for row in db.execute(f"SELECT {id_column} FROM {table}")}
        rows = db.execute("SELECT count(*) FROM messages").fetchone()[0]
    expected = {s["id"] for s in manifest["sessions"]}
    if sessions != expected or rows != expected_rows:
        raise RuntimeError(f"Incomplete {table} archive: {len(sessions)} sessions, {rows} message rows")
    return {"sessions": len(sessions), "message_rows": rows}


class Langfuse:
    def __init__(self, env_file, url):
        values = dict(line.split("=", 1) for line in env_file.read_text().splitlines()
                      if line and not line.startswith("#"))
        keys = values["LANGFUSE_INIT_PROJECT_PUBLIC_KEY"] + ":" + values["LANGFUSE_INIT_PROJECT_SECRET_KEY"]
        self.auth = "Basic " + base64.b64encode(keys.encode()).decode()
        self.url = url.rstrip("/")

    def request(self, path, body=None):
        headers = {"Authorization": self.auth, "Content-Type": "application/json",
                   "x-langfuse-ingestion-version": "4"}
        request = urllib.request.Request(self.url + path,
            data=json.dumps(body).encode() if body is not None else None, headers=headers)
        started = time.perf_counter()
        with urllib.request.urlopen(request, timeout=30) as response:
            data = json.load(response)
        return data, (time.perf_counter() - started) * 1000

    def ingest(self, manifest):
        spans = []
        for s in manifest["sessions"]:
            for m in s["messages"]:
                at = int(datetime.datetime.fromisoformat(m["at"].replace("Z", "+00:00")).timestamp()) * 10**9
                attrs = {"langfuse.session.id": s["id"],
                    "langfuse.observation.type": "tool" if m["kind"] == "tool_result" else "span",
                    "langfuse.observation.output": m["text"],
                    "langfuse.observation.metadata.source_message_id": m["id"],
                    "langfuse.observation.metadata.harness": s["harness"],
                    "langfuse.observation.metadata.repo": s["repo"]}
                spans.append({"traceId": s["id"].replace("-", ""),
                    "spanId": m["id"].replace("-", "")[:16], "name": m["kind"], "kind": 1,
                    "startTimeUnixNano": str(at), "endTimeUnixNano": str(at + 1000000),
                    "attributes": [{"key": k, "value": {"stringValue": v}} for k, v in attrs.items()]})
        for start in range(0, len(spans), 100):
            data, _ = self.request("/api/public/otel/v1/traces", {"resourceSpans": [{
                "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "competitive-pilot"}}]},
                "scopeSpans": [{"scope": {"name": "competitive-pilot"}, "spans": spans[start:start + 100]}]}]})
            if data.get("partialSuccess", {}).get("rejectedSpans", 0):
                raise RuntimeError("Langfuse rejected spans: " + json.dumps(data))

    def query(self, q):
        filters = [{"type": "string", "column": "output", "operator": "matches", "value": q["query"]}]
        params = urllib.parse.urlencode({"fields": "core,basic,io,metadata", "limit": 500,
                                        "filter": json.dumps(filters)})
        return self.request("/api/public/v2/observations?" + params)


def run(args):
    root = args.root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    if (root / "manifest.json").exists():
        raise RuntimeError("Use a fresh --root; existing pilot data is preserved.")
    manifest = corpus(root)
    report = {"machine": {"system": platform.system(), "release": platform.release(),
        "architecture": platform.machine(), "cpu_count": os.cpu_count()},
        "corpus": {k: manifest[k] for k in ("version", "messages", "bytes", "sha256")},
        "samples": [], "setup": {}, "errors": [], "scope": "synthetic retrieval; no real-agent task outcomes"}
    report["seed"], report["runs"] = args.seed, args.runs
    report["started_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    report["versions"] = {}
    for name, path in (("flopwire", args.flopwire), ("agentsview", args.agentsview)):
        if path:
            output, _ = invoke([str(path), "version"], env_for(root / (name + "-version-home")))
            report["versions"][name] = {"version": output.strip(),
                "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
    home = root / "corpus"
    fw_env = env_for(root / "flopwire-home")
    av_env = env_for(root / "agentsview-home")
    av_data = root / "agentsview-data"
    av_data.mkdir()
    av_env.update(AGENTSVIEW_DATA_DIR=str(av_data), CLAUDE_PROJECTS_DIR=str(home / ".claude/projects"),
        CODEX_SESSIONS_DIR=str(home / ".codex/sessions"), AGENTSVIEW_TELEMETRY_ENABLED="false")
    (av_data / "config.toml").write_text('telemetry_enabled = false\narchive_content = "full"\n'
        '[vector]\nenabled = false\n')
    processes, handles, queries = [], [], {}
    try:
        if args.rg:
            paths = {str(root / s["source"]): s["id"] for s in manifest["sessions"]}
            def rg_query(q):
                command = [str(args.rg), "--json", "--glob", "*.jsonl", "-e", q["query"]]
                if q["kind"] == "literal":
                    command.append("-F")
                command.extend([str(home / ".claude/projects"), str(home / ".codex/sessions")])
                text, ms = invoke(command, env_for(root / "rg-home"))
                hits = []
                for line in text.splitlines():
                    event = json.loads(line)
                    if event["type"] == "match":
                        hits.append({"session_id": paths[event["data"]["path"]["text"]]})
                return {"hits": hits}, ms
            output, _ = invoke([str(args.rg), "--version"], env_for(root / "rg-home"))
            report["versions"]["rg"] = {"version": output.splitlines()[0]}
            queries["rg"] = rg_query
        if args.flopwire:
            cmd = [str(args.flopwire), "agent", "run", "--no-sync", "--db", str(root / "flopwire-index.db"),
                "--claude-projects", str(home / ".claude/projects"), "--codex-home", str(home / ".codex"),
                "--devin-db", "-", "--opencode-db", "-"]
            _, elapsed = invoke(cmd + ["--once"], fw_env)
            tool_calls = sum(m["kind"] == "tool_result" for s in manifest["sessions"] for m in s["messages"])
            report["setup"]["flopwire"] = {"index_ms": elapsed, "validation":
                validate_archive(root / "flopwire-index.db", "conversations", "session_id", manifest,
                                 manifest["messages"] + tool_calls)}
            def fw_query(q):
                command = [str(args.flopwire), "grep", q["query"], "--json", "--local",
                    "--index", str(root / "flopwire-index.db"), "--limit", "500", "--include-self"]
                if q["kind"] == "literal":
                    command.append("-F")
                text, ms = invoke(command, fw_env)
                return json.loads(text), ms
            queries["flopwire"] = fw_query
        if args.agentsview:
            offline = dict(av_env, AGENTSVIEW_NO_DAEMON="1")
            text, elapsed = invoke([str(args.agentsview), "sync"], offline)
            report["setup"]["agentsview"] = {"sync_ms": elapsed, "output": text[-2000:], "validation":
                validate_archive(av_data / "sessions.db", "sessions", "id", manifest, manifest["messages"])}
            log = (root / "agentsview-serve.log").open("w")
            handles.append(log)
            p = subprocess.Popen([str(args.agentsview), "serve", "--port", "0", "--no-browser",
                "--no-update-check"], env=av_env, stdout=log, stderr=log)
            processes.append(p)
            def av_query(q):
                command = [str(args.agentsview), "session", "search", q["query"], "--json",
                    "--include-children", "--include-automated", "--limit", "500"]
                if q["kind"] == "regex":
                    command.append("--regex")
                text, ms = invoke(command, av_env)
                return json.loads(text), ms
            queries["agentsview"] = av_query
        if args.langfuse_env:
            lf = Langfuse(args.langfuse_env, args.langfuse_url)
            health, _ = lf.request("/api/public/health")
            report["versions"]["langfuse"] = health
            started = time.perf_counter()
            lf.ingest(manifest)
            report["setup"]["langfuse"] = {"ingest_http_ms": (time.perf_counter() - started) * 1000,
                "adapter": "canonical message text to OTLP observation output; session and source IDs attached"}
            expected = {m["id"] for s in manifest["sessions"] for m in s["messages"]}
            deadline = time.monotonic() + 60
            while True:
                data, _ = lf.request("/api/public/v2/observations?limit=1000&fields=core,basic,metadata")
                sources = {x.get("metadata", {}).get("source_message_id") for x in data["data"]}
                if sources == expected and len(data["data"]) == manifest["messages"]:
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"Incomplete Langfuse import: {len(sources)} canonical message IDs")
                time.sleep(.2)
            report["setup"]["langfuse"]["validation"] = {"observations": len(data["data"]), "source_ids_complete": True}
            queries["langfuse"] = lf.query
        # Warm-up also bounds readiness. Never retry measured invocations.
        for product, query in queries.items():
            for q in manifest["queries"]:
                if product == "langfuse" and q["kind"] == "regex":
                    report["samples"].append({"product": product, "query": q["name"], "status": "unsupported"})
                    continue
                deadline = time.monotonic() + 10
                while True:
                    try:
                        value, ms = query(q)
                        check = correctness(q["expected_sessions"], session_ids(value))
                        if check["pass"] or time.monotonic() >= deadline:
                            break
                    except Exception:
                        if time.monotonic() >= deadline:
                            raise
                    time.sleep(.2)
                dump(root / (product + "-" + q["name"] + "-response.json"), value)
                report["samples"].append({"product": product, "query": q["name"], "phase": "warmup",
                    "elapsed_ms": ms, "correctness": check})
        rng = random.Random(args.seed)
        for iteration in range(args.runs):
            jobs = [(p, q) for p in queries for q in manifest["queries"]
                    if not (p == "langfuse" and q["kind"] == "regex")]
            rng.shuffle(jobs)
            for product, q in jobs:
                sample = {"product": product, "query": q["name"], "phase": "measured", "iteration": iteration}
                try:
                    value, ms = queries[product](q)
                    sample.update(elapsed_ms=ms, correctness=correctness(q["expected_sessions"], session_ids(value)))
                except Exception as e:
                    sample.update(status="failed", error=str(e))
                report["samples"].append(sample)
            dump(root / "results.json", report)
        report["summary"] = []
        for product in queries:
            for q in manifest["queries"]:
                samples = [s for s in report["samples"] if s["product"] == product and s["query"] == q["name"]
                           and s.get("phase") == "measured"]
                if not samples:
                    continue
                successful = [s for s in samples if "elapsed_ms" in s]
                row = {"product": product, "query": q["name"], "attempts": len(samples),
                    "command_failures": len(samples) - len(successful),
                    "correct_samples": sum(s.get("correctness", {}).get("pass", False) for s in samples)}
                if successful:
                    row.update(summary([s["elapsed_ms"] for s in successful]))
                report["summary"].append(row)
        print(json.dumps(report["summary"], indent=2))
    except Exception as e:
        report["errors"].append(str(e))
        raise
    finally:
        for p in processes:
            p.terminate()
            try:
                p.wait(timeout=10)
            except subprocess.TimeoutExpired:
                p.kill()
                p.wait()
        for handle in handles:
            handle.close()
        dump(root / "results.json", report)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--flopwire", type=Path)
    parser.add_argument("--agentsview", type=Path)
    parser.add_argument("--rg", type=Path, help="Raw native-JSONL scan baseline")
    parser.add_argument("--langfuse-env", type=Path)
    parser.add_argument("--langfuse-url", default="http://127.0.0.1:43180")
    parser.add_argument("--runs", type=int, default=30)
    parser.add_argument("--seed", type=int, default=1)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    if not any((args.flopwire, args.agentsview, args.langfuse_env, args.rg)):
        parser.error("Select at least one product")
    run(args)


if __name__ == "__main__":
    main()
