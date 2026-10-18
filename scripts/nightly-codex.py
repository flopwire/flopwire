#!/usr/bin/env python3
"""Run the installed Codex probe with disposable API credentials and safe artifacts."""

import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time


MODEL = "gpt-5.6-luna"
PROBE_TIMEOUT = 600
EXPECTED_CASES = {"idle", "prompt-submit", "framing", "mid-turn", "subagent", "guardian", "hook-binary"}


def run_owned(args, env, timeout, stdin=None):
    """Allow cleanup on timeout, then terminate the process group we started."""
    process = subprocess.Popen(args, env=env, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               text=True, start_new_session=True)
    try:
        stdout, stderr = process.communicate(stdin, timeout=timeout)
        return process.returncode, stdout, stderr
    finally:
        try:
            os.killpg(process.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            pass
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        for pipe in (process.stdin, process.stdout, process.stderr):
            if pipe:
                pipe.close()


def usage_totals(root):
    """Count final cumulative usage once per session, including child sessions."""
    totals = {}
    for path in root.rglob("rollout-*.jsonl"):
        model, usage = None, None
        for line in path.read_text().splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue  # A terminated run may leave a partial final line.
            payload = event.get("payload", {})
            if event.get("type") == "turn_context":
                model = payload.get("model")
            if payload.get("type") == "token_count" and payload.get("info"):
                usage = payload["info"].get("total_token_usage")
        if usage:
            total = totals.setdefault(model or "unknown", {})
            for key, value in usage.items():
                if isinstance(value, int):
                    total[key] = total.get(key, 0) + value
    return totals


def main(binary, output):
    key = os.environ.get("OPENAI_API_KEY", "")
    if not key:
        raise ValueError("OPENAI_API_KEY is required; no live checks ran")
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    started = time.monotonic()
    summary = {"model": MODEL, "status": "failed", "results": [], "usage_by_model": {}}
    log = ""
    code = 1
    # Keep both login copies outside the artifact directory. Remove them even
    # if login, installation, a probe, or report parsing fails.
    with tempfile.TemporaryDirectory(prefix="flopwire-nightly-") as tmp:
        scratch = Path(tmp)
        auth = scratch / "auth"
        auth.mkdir(mode=0o700)
        (auth / "config.toml").write_text('cli_auth_credentials_store = "file"\n')
        env = dict(os.environ, CODEX_HOME=str(auth))
        env.pop("OPENAI_API_KEY", None)
        env.pop("GH_TOKEN", None)
        env.pop("GITHUB_TOKEN", None)
        try:
            rc, version, stderr = run_owned(["codex", "--version"], env, 30)
            if rc:
                raise RuntimeError("codex --version failed: " + stderr)
            summary["codex_version"] = version.strip()
            rc, stdout, stderr = run_owned(["codex", "login", "--with-api-key"], env, 60, key + "\n")
            log += stdout + stderr
            if rc:
                raise RuntimeError("Codex API-key login failed")
            code, stdout, stderr = run_owned(
                [str(Path(binary).resolve()), "probe", "--harness", "codex", "--local",
                 "--as-installed", "--model", MODEL, "--turn-timeout", "90s",
                 "--json", "--dir", str(scratch / "probe")], env, PROBE_TIMEOUT)
            log += stderr
            report = json.loads(stdout)
            if not isinstance(report, dict) or not isinstance(report.get("results", []), list):
                raise ValueError("Probe returned an invalid report shape")
            summary["results"] = report.get("results", [])
            summary["skipped"] = report.get("skipped", [])
            if not all(isinstance(r, dict) for r in summary["results"]):
                raise ValueError("Probe returned an invalid case result")
            cases = [r.get("case") for r in summary["results"]]
            complete = len(cases) == len(EXPECTED_CASES) and set(cases) == EXPECTED_CASES
            if code == 0 and complete and not summary["skipped"] and all(
                    r.get("pass") is True for r in summary["results"]):
                summary["status"] = "passed"
            else:
                code = 1
                summary["error"] = "Probe failed, skipped checks, or returned an incomplete case set"
        except (OSError, ValueError, RuntimeError, subprocess.TimeoutExpired) as error:
            summary["error"] = str(error)
            code = 1
        finally:
            try:
                summary["usage_by_model"] = usage_totals(scratch)
            except (OSError, ValueError, TypeError, AttributeError) as error:
                summary["usage_error"] = str(error)
                summary["status"] = "failed"
                code = 1
    summary["seconds"] = round(time.monotonic() - started, 3)
    # Model/tool output is untrusted. Redact the exact credential before any
    # artifact write, including exception text and probe evidence.
    serialized = json.dumps(summary, indent=2).replace(key, "[REDACTED]") + "\n"
    (output / "summary.json").write_text(serialized)
    (output / "probe.log").write_text(log.replace(key, "[REDACTED]"))
    message = f"Codex installed plugin: {summary['status']} ({summary['seconds']}s)."
    print(message)
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a") as stream:
            stream.write(message + " See the nightly-codex artifact for results and token usage.\n")
    return code


if __name__ == "__main__":
    sys.exit(main(*sys.argv[1:]))
