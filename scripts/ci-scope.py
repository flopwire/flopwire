#!/usr/bin/env python3
"""Classify a complete git diff; unknown paths always require the full suite."""

import os
import subprocess
import sys


def docs_only(paths):
    return bool(paths) and all(
        path in ("README.md", "CONTRIBUTING.md", "AGENTS.md")
        or (path.startswith(("docs/", "notes/")) and path.endswith(".md"))
        for path in paths
    )


def main():
    base, head = sys.argv[1:]
    # No rename detection: moving code into docs must include the deleted code.
    result = subprocess.check_output(
        ["git", "diff", "--no-renames", "--name-only", "-z", base, head, "--"]
    )
    paths = result.decode("utf-8", errors="surrogateescape").split("\0")[:-1]
    full = not docs_only(paths)
    print(f"{len(paths)} changed paths; full suite: {full}")
    with open(os.environ["GITHUB_OUTPUT"], "a") as output:
        output.write(f"full={str(full).lower()}\n")


if __name__ == "__main__":
    main()
