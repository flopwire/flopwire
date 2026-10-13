#!/usr/bin/env python3
"""Read-only local probe; print aggregate counts, never transcript text or IDs."""

import argparse
import json
import subprocess
from pathlib import Path

from candidates import candidates


def read_json(command):
    result = subprocess.run(command, capture_output=True, text=True)
    if result.returncode:
        # Tool stderr may contain private context. Do not propagate it.
        raise RuntimeError(command[0] + " failed; stderr withheld")
    return json.loads(result.stdout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--checkout", type=Path, required=True)
    parser.add_argument("--repository", required=True, help="GitHub OWNER/REPO")
    parser.add_argument("--flopwire-bin", default="flopwire", help="CLI binary to probe")
    parser.add_argument("--pr", type=int, action="append", required=True)
    args = parser.parse_args()
    command = [args.flopwire_bin, "sessions", "--local", "--repo", str(args.checkout.resolve()),
               "--detail", "--include-self", "--limit", "500", "--max-bytes", "1000000"]
    raw = read_json(command)
    # Scope is asserted by the caller and flopwire --repo. The offline reducer
    # does not independently authenticate repository or transcript provenance.
    exported = {"repository": args.repository, "complete": not raw["has_more"], "sessions": []}
    for i, session in enumerate(raw["sessions"]):
        digest = session.get("digest") or {}
        exported["sessions"].append({"repository": args.repository,
                                     "session_id": "session-" + str(i),
                                     "commits": digest.get("commits", []),
                                     "prs": digest.get("prs", []), "more": digest.get("more", [])})
    for number in args.pr:
        pr = read_json(["gh", "pr", "view", str(number), "--repo", args.repository,
                        "--json", "number,url,mergeCommit,commits,headRefName,state"])
        result = candidates(pr, exported, pr["mergeCommit"]["oid"])
        counts = {}
        for row in result["candidates"]:
            for kind in set(e["kind"] for e in row["evidence"]):
                counts[kind] = counts.get(kind, 0) + 1
        branch = read_json(command + ["--branch", pr["headRefName"]])
        print(json.dumps({"pr": number, "sessions_examined": len(exported["sessions"]),
                          "complete": exported["complete"], "candidates_by_evidence": counts,
                          "warning_count": len(result["warnings"]),
                          "branch_scoped_sessions": len(branch["sessions"]),
                          "branch_complete": not branch["has_more"]}))


if __name__ == "__main__":
    try:
        main()
    except (RuntimeError, ValueError, KeyError, TypeError):
        raise SystemExit("probe failed; private tool output withheld")
