#!/usr/bin/env python3
"""Derive expected hits for testdata/acceptance/queries.yaml by brute force.

Reads testdata/acceptance/queries.in.yaml, greps the raw Claude and Codex
transcripts (read-only) for each query with ripgrep, keeps the lines where
the query matches message text inside the filter scope (repo, agent, since),
and writes queries.yaml with expectations the local index must meet:

  find:   at least min(N, limit) hits, and the hits include the session of
          the newest brute-force match (find returns newest first).
  search: at least min(N, limit) hits, and when every match fits in one
          page, the hits include the two sessions with the most matches.

N counts matching transcript lines, and only matches within the first
HEAD characters of the line's joined text count: the index stores capped text
(tool output about 6KB) and the trigram table sees each row's head (2KB),
tail and error-like lines, so a match deep inside a long tool output is
out of reach by design. The full count is kept in `evidence`. The
comparison is deliberately coarse: a line can yield several rows (text plus tool call), rows that
are superseded or off the active branch are hidden, and Codex event_msg
lines duplicate response_item lines (only response_item lines count).
Devin's sessions.db is not brute-forced; its rows only add hits.

Usage: scripts/acceptance-expect.py [--names a,b] [--in F] [--out F]
Requires: rg, PyYAML.
"""
import argparse
import json
import os
import re
import subprocess
import sys
from datetime import datetime, timezone

import yaml

HOME = os.path.expanduser("~")
CLAUDE = os.path.join(HOME, ".claude", "projects")
CODEX = [os.path.join(HOME, ".codex", d) for d in ("sessions", "archived_sessions")]
LIMIT_FIND, LIMIT_SEARCH = 50, 100
# Every query is bounded to transcripts written before the query set was
# curated, so later sessions (including the ones that curate or run it)
# cannot push the expected hits off the first page.
DEFAULT_UNTIL = "2026-09-29T00:00:00Z"

HEAD = 1500

TOK = re.compile(r"[\w\-./]+", re.UNICODE)


def tokens(text):
    """fts_tok's tokenizer with the index's trim rules (text.go)."""
    out = []
    for t in TOK.findall(text):
        t = t.lstrip("./").rstrip(".-/")
        if t:
            out.append(t.lower())
    return out


def strings_in(v, skip=()):
    if isinstance(v, str):
        # Codex wraps some tool outputs as a JSON string ({"output": ...});
        # the parser indexes the inner text, so heads are measured there.
        if v.startswith("{") and v.endswith("}"):
            try:
                inner = json.loads(v)
            except ValueError:
                inner = None
            if isinstance(inner, dict):
                yield from strings_in(inner, skip)
                return
        yield v
    elif isinstance(v, dict):
        for k, x in v.items():
            if k not in skip:
                yield from strings_in(x, skip)
    elif isinstance(v, list):
        for x in v:
            yield from strings_in(x, skip)


codex_meta = {}


def codex_session(path):
    """(session id, cwd, history start) from a rollout's first session_meta.
    A forked subagent copies its parent's history below
    subagent_history_start_ordinal; the parser skips those lines."""
    if path not in codex_meta:
        sid, cwd, hso = "", "", 0
        with open(path, "rb") as f:
            for raw in f:
                try:
                    rec = json.loads(raw)
                except ValueError:
                    continue
                if rec.get("type") == "session_meta":
                    p = rec.get("payload", {})
                    sid, cwd, hso = p.get("id", ""), p.get("cwd", ""), p.get("subagent_history_start_ordinal") or 0
                    break
        codex_meta[path] = (sid, cwd, hso)
    return codex_meta[path]


def message(path, raw):
    """(agent, session, cwd, ts, texts) for a transcript line, or None."""
    try:
        rec = json.loads(raw)
    except ValueError:
        return None
    if path.startswith(CLAUDE):
        if rec.get("type") not in ("user", "assistant") or "message" not in rec:
            return None
        texts = list(strings_in(rec["message"], skip=("signature", "id", "model", "usage")))
        sid = rec.get("sessionId", "")
        if "/subagents/" in path:  # a subagent is its own conversation: agent-<id>
            sid = os.path.basename(path)[: -len(".jsonl")]
        return "claude", sid, rec.get("cwd", ""), rec.get("timestamp", ""), texts
    if rec.get("type") != "response_item":
        return None
    sid, cwd, hso = codex_session(path)
    if hso and isinstance(rec.get("ordinal"), int) and rec["ordinal"] < hso:
        return None
    texts = list(strings_in(rec.get("payload", {}), skip=("encrypted_content", "id", "call_id")))
    return "codex", sid, cwd, rec.get("timestamp", ""), texts


def in_scope(q, agent, cwd, ts):
    if q.get("agent") and q["agent"] != agent:
        return False
    if q.get("repo"):
        r = os.path.expanduser(q["repo"]).rstrip("/")
        if not (cwd == r or cwd.startswith(r + "/")):
            return False
    try:
        t = datetime.fromisoformat(ts.replace("Z", "+00:00"))
    except ValueError:
        return False
    if q.get("since") and t < datetime.fromisoformat(q["since"]).replace(tzinfo=timezone.utc):
        return False
    return t < datetime.fromisoformat(q["until"].replace("Z", "+00:00"))


def brute(q):
    verb, query = q["verb"], q["query"]
    if verb == "find":
        needle = query.lower()
        rg_term = query
        match = lambda texts, head: any(needle in (t[:head] if head else t).lower() for t in texts)
    else:
        want = tokens(query)
        rg_term = max(want, key=len)  # the rarest-looking term narrows files
        def match(texts, head):
            have = set()
            for t in texts:
                have.update(tokens(t[:head] if head else t))
            return all(w in have for w in want)
    cmd = ["rg", "--no-heading", "--with-filename", "-F", "-i", "-g", "*.jsonl", rg_term, CLAUDE] + [d for d in CODEX if os.path.isdir(d)]
    out = subprocess.run(cmd, capture_output=True, check=False).stdout
    hits = []
    for line in out.split(b"\n"):
        if not line:
            continue
        # rg prints path:line; paths hold no ':' here.
        path, _, raw = line.partition(b":")
        path = path.decode()
        m = message(path, raw)
        if m is None:
            continue
        agent, sid, cwd, ts, texts = m
        if not in_scope(q, agent, cwd, ts) or not match(texts, 0):
            continue
        # Rows join a line's text parts (Codex output arrays, Claude
        # blocks), so the head is measured on the joined text.
        hits.append({"session": sid, "ts": ts, "agent": agent, "head": match(["\n".join(texts)], HEAD)})
    return hits


def expect(q, hits):
    limit = q.get("limit") or (LIMIT_FIND if q["verb"] == "find" else LIMIT_SEARCH)
    q["limit"] = limit
    everywhere = len(hits)
    hits = [h for h in hits if h["head"]]
    n = len(hits)
    e = {"min_hits": min(n, limit), "sessions": []}
    if hits and everywhere <= limit:
        # Every match (anywhere in the text) fits in one page, so the
        # sessions with the most head matches must be among the hits. (With more matches the page is
        # the newest rows by index order, or the best bm25 scores, which
        # brute force does not predict.)
        counts = {}
        for h in hits:
            counts[h["session"]] = counts.get(h["session"], 0) + 1
        e["sessions"] = [s for s, _ in sorted(counts.items(), key=lambda kv: -kv[1])[:2]]
    q["expect"] = e
    sessions = len({h["session"] for h in hits})
    q["evidence"] = (f"brute force {datetime.now().date()}: {n} lines match in the first {HEAD} chars of their text, in {sessions} sessions "
                     f"(claude {sum(h['agent'] == 'claude' for h in hits)}, codex {sum(h['agent'] == 'codex' for h in hits)}); {everywhere} match anywhere")
    return q


def main():
    ap = argparse.ArgumentParser()
    root = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
    ap.add_argument("--in", dest="inp", default=os.path.join(root, "testdata/acceptance/queries.in.yaml"))
    ap.add_argument("--out", default=os.path.join(root, "testdata/acceptance/queries.yaml"))
    ap.add_argument("--names", default="")
    a = ap.parse_args()
    qs = yaml.safe_load(open(a.inp))
    prev = {}
    if os.path.exists(a.out):
        prev = {q["name"]: q for q in yaml.safe_load(open(a.out)) or []}
    names = set(filter(None, a.names.split(",")))
    out = []
    for q in qs:
        if names and q["name"] not in names:
            out.append(prev.get(q["name"], q))
            continue
        q.setdefault("until", DEFAULT_UNTIL)
        if q.get("since") and "T" not in q["since"]:
            q["since"] += "T00:00:00Z"
        q = expect(q, brute(q))
        print(f"{q['name']:32} {q['evidence']}", file=sys.stderr)
        out.append(q)
    with open(a.out, "w") as f:
        f.write("# Generated by scripts/acceptance-expect.py from queries.in.yaml; see both files.\n")
        yaml.safe_dump(out, f, sort_keys=False, width=200, allow_unicode=True)


if __name__ == "__main__":
    main()
