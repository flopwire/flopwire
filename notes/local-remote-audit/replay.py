#!/usr/bin/env python3
"""Replay historical observations against an isolated copy of the audited source."""
import json
from pathlib import Path
import re
import subprocess
import tempfile

here = Path(__file__).resolve().parent
checkout = here.parents[1]
audited = "883a467f427be88f05a58c2751a08f1cb30e6d86"
fixtures = {
    "retrieval": "internal/retrieval/local",
    "metadata": "cmd/flopwire",
    "delegation": "cmd/flopwire",
    "inbox": "internal/devicebus",
    "policy": "internal/agent",
}
with tempfile.TemporaryDirectory(prefix="flopwire-boundary-audit-") as work:
    # Go canonicalizes its working directory on macOS (/var -> /private/var).
    # Overlay keys must use the same spelling or added tests are not discovered.
    repo = (Path(work) / "source").resolve()
    repo.mkdir()
    archive = Path(work) / "source.tar"
    with archive.open("wb") as output:
        subprocess.run(["git", "archive", "--format=tar", audited],
                       cwd=checkout, stdout=output, check=True)
    subprocess.run(["tar", "-xf", str(archive), "-C", str(repo)], check=True)
    print(f"Replaying historical defects against {audited}; not testing current main.",
          flush=True)
    mapping = {}
    expected = set()
    for name, package in fixtures.items():
        fixture = here / "repros" / f"{name}_test.go.txt"
        expected.update(re.findall(r"^func (TestAudit\w+)\(", fixture.read_text(), re.M))
        formatted = subprocess.run(["gofmt"], input=fixture.read_text(), text=True,
                                   capture_output=True, check=True).stdout
        target = Path(work) / f"{name}_test.go"
        target.write_text(formatted)
        mapping[str(repo / package / f"boundary_audit_{name}_test.go")] = str(target)
    overlay = Path(work) / "overlay.json"
    overlay.write_text(json.dumps({"Replace": mapping}))
    packages = sorted({"./" + p for p in fixtures.values()})
    passed = set()
    process = subprocess.Popen(
        ["go", "test", "-overlay=" + str(overlay), *packages,
         "-run", "^TestAudit", "-count=1", "-json"], cwd=repo,
        stdout=subprocess.PIPE, text=True)
    for line in process.stdout:
        event = json.loads(line)
        print(event.get("Output", ""), end="", flush=True)
        if event.get("Action") == "pass" and event.get("Test") in expected:
            passed.add(event["Test"])
    result = process.wait()
    if result:
        raise SystemExit(result)
    if missing := expected - passed:
        raise SystemExit("Replay did not execute every observation: " + ", ".join(sorted(missing)))
    print(f"Verified {len(passed)} named historical observations.")
