#!/usr/bin/env python3
"""Replay synthetic observations without modifying package source files."""
import json
from pathlib import Path
import subprocess
import tempfile

here = Path(__file__).resolve().parent
repo = here.parents[1]
fixtures = {
    "retrieval": "internal/retrieval/local",
    "metadata": "cmd/flopwire",
    "delegation": "cmd/flopwire",
    "inbox": "internal/devicebus",
    "policy": "internal/agent",
}
with tempfile.TemporaryDirectory(prefix="flopwire-boundary-audit-") as work:
    mapping = {}
    for name, package in fixtures.items():
        fixture = here / "repros" / f"{name}_test.go.txt"
        formatted = subprocess.run(["gofmt"], input=fixture.read_text(), text=True,
                                   capture_output=True, check=True).stdout
        target = Path(work) / f"{name}_test.go"
        target.write_text(formatted)
        mapping[str(repo / package / f"boundary_audit_{name}_test.go")] = str(target)
    overlay = Path(work) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": mapping}))
    packages = sorted({"./" + p for p in fixtures.values()})
    raise SystemExit(subprocess.run(
        ["go", "test", "-overlay=" + str(overlay), *packages,
         "-run", "^TestAudit", "-count=1", "-v"], cwd=repo).returncode)
