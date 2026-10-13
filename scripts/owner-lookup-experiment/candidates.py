#!/usr/bin/env python3
"""Offline attribution experiment. Produces evidence candidates, never an owner."""

import argparse
import json
import re
from pathlib import Path
from urllib.parse import urlparse


def sha(value, minimum=40):
    if not isinstance(value, str) or not re.fullmatch(r"[0-9a-fA-F]{%d,40}" % minimum, value):
        raise ValueError("expected a hexadecimal Git SHA")
    return value.lower()


def candidates(pr, exported, target):
    url = urlparse(pr["url"])
    match = re.fullmatch(r"/([^/]+/[^/]+)/pull/([0-9]+)", url.path)
    if url.scheme != "https" or url.netloc != "github.com" or not match:
        raise ValueError("expected a canonical github.com PR URL")
    repository, number = match.groups()
    if int(number) != pr["number"] or pr["state"] != "MERGED":
        raise ValueError("expected consistent metadata for a merged PR")
    if exported["repository"] != repository:
        raise ValueError("session export must be scoped to the PR repository")
    if not isinstance(exported["complete"], bool):
        raise ValueError("export completeness must be explicit")
    merge = sha(pr["mergeCommit"]["oid"])
    lineage = sorted(set(sha(c["oid"]) for c in pr["commits"]))
    if target != pr["url"] and sha(target) != merge:
        raise ValueError("target must be this PR URL or its full merged SHA")
    known = sorted(set([merge] + lineage))
    out, seen, warnings = [], set(), []
    if not exported["complete"]:
        warnings.append("session export is incomplete; absence is inconclusive")
    for index, row in enumerate(exported["sessions"]):
        if row["repository"] != repository:
            continue
        sid = row["session_id"]
        if not isinstance(sid, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]+", sid):
            raise ValueError("expected a nonempty session identifier")
        if sid in seen:
            raise ValueError("duplicate session identifier in export")
        seen.add(sid)
        evidence = []
        if set(row.get("more", [])) & {"commits", "prs"}:
            warnings.append("digest evidence is capped for " + sid)
        for ci, observed in enumerate(row.get("commits", [])):
            prefix = sha(observed, minimum=7)
            matches = [full for full in known if full.startswith(prefix)]
            if not matches:
                continue
            # Never turn a colliding prefix into an attribution claim.
            kind = "ambiguous_commit_prefix" if len(matches) != 1 else (
                "observed_merged_commit" if matches[0] == merge else "observed_pr_commit"
            )
            evidence.append({"kind": kind, "observed": prefix, "matches": matches,
                             "source": f"sessions[{index}].commits[{ci}]"})
        ref = repository + "#" + number
        for pi, observed in enumerate(row.get("prs", [])):
            if observed == ref:
                evidence.append({"kind": "pr_reference", "observed": ref,
                                 "source": f"sessions[{index}].prs[{pi}]"})
        if evidence:
            out.append({"session_id": sid, "evidence": evidence,
                        "inspect": {"tool": "flopwire_read", "address": sid, "outline": True}})
    return {"kind": "attribution_candidates", "target": target, "repository": repository,
            "pr": pr["url"], "merge_commit": merge, "pr_commits": lineage,
            "status": "review_candidates" if out else "no_evidence",
            "candidates": sorted(out, key=lambda c: c["session_id"]),
            "warnings": warnings,
            "limitations": ["PR references are mentions, not authorship evidence",
                            "observed commits do not prove sole authorship or current presence",
                            "GitHub PR commits may differ from pre-rebase transcript hashes",
                            "digest evidence has no message ordinal; inspect the source session"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pr-metadata", type=Path, required=True)
    parser.add_argument("--sessions", type=Path, required=True)
    parser.add_argument("--target", required=True, help="PR URL or full merged SHA")
    args = parser.parse_args()
    try:
        result = candidates(json.loads(args.pr_metadata.read_text()),
                            json.loads(args.sessions.read_text()), args.target)
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.error(str(error))
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
