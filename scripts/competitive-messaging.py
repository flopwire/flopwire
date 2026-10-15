#!/usr/bin/env python3
"""Measure isolated CLI messaging and recovery; no real agents are contacted."""
import argparse
import datetime
import hashlib
import importlib.util
import json
from pathlib import Path
import random
import socket
import subprocess
import time

spec = importlib.util.spec_from_file_location("pilot", Path(__file__).with_name("competitive-pilot.py"))
pilot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pilot)


def objects(value):
    if isinstance(value, dict):
        yield value
        for v in value.values():
            yield from objects(v)
    elif isinstance(value, list):
        for v in value:
            yield from objects(v)


def terminate(process):
    process.terminate()
    try:
        process.wait(timeout=10)
    except subprocess.TimeoutExpired:
        process.kill()
        process.wait()


def run(args):
    root = args.root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    if (root / "manifest.json").exists():
        raise RuntimeError("Use a fresh --root; existing data is preserved.")
    manifest = pilot.corpus(root)
    sender, receiver = manifest["sessions"][:2]
    # Recent transcript activity provides Flopwire's normal local presence signal.
    for s in manifest["sessions"][:4]:
        s["messages"][-1]["at"] = datetime.datetime.now(datetime.timezone.utc).isoformat().replace("+00:00", "Z")
        (root / s["source"]).write_text("".join(json.dumps(r) + "\n"
            for r in pilot.native_rows(s, s["messages"])))
        manifest["sha256"][s["source"]] = hashlib.sha256((root / s["source"]).read_bytes()).hexdigest()
    pilot.dump(root / "manifest.json", manifest)
    fw_env = pilot.env_for(root / "fw-home")
    fw_env.update(FLOPWIRE_SOCKET=str(root / "fw.sock"), FLOPWIRE_SESSION_ID=sender["id"], FLOPWIRE_AGENT="claude")
    am_env = pilot.env_for(root / "am-home")
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    am_env.update(STORAGE_ROOT=str(root / "am-archive"), DATABASE_URL="sqlite:///" + str(root / "mail.sqlite3"),
        HTTP_HOST="127.0.0.1", HTTP_PORT=str(port))
    fw_cmd = [str(args.flopwire), "agent", "run", "--no-sync", "--db", str(root / "index.db"),
        "--claude-projects", str(root / "corpus/.claude/projects"), "--codex-home", str(root / "corpus/.codex"),
        "--devin-db", "-", "--opencode-db", "-", "--socket", str(root / "fw.sock")]
    am_cmd = [str(args.agent_mail), "serve-http", "--host", "127.0.0.1", "--port", str(port), "--no-tui", "--no-auth"]
    project = str(root / "am-project")
    project_args = ["--project", project]
    logs = [(root / "fw-server.log").open("w"), (root / "am-server.log").open("w")]
    processes = {}
    report = {"scope": "sequential CLI acceptance, recipient retrieval, hook output and restart recovery; no model visibility",
              "runs": args.runs, "seed": args.seed, "samples": [], "recovery": {}, "errors": []}
    report["started_at"] = datetime.datetime.now(datetime.timezone.utc).isoformat()
    report["versions"] = {}
    for name, path, flag in (("flopwire", args.flopwire, "version"), ("agent-mail", args.agent_mail, "--version")):
        version, _ = pilot.invoke([str(path), flag], pilot.env_for(root / (name + "-version-home")))
        report["versions"][name] = {"version": version.strip(), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
    def launch(name):
        cmd, env, log = (fw_cmd, fw_env, logs[0]) if name == "flopwire" else (am_cmd, am_env, logs[1])
        process = subprocess.Popen(cmd, env=env, stdout=log, stderr=log)
        processes[name] = process
        return process
    def fw_hook():
        text, ms = pilot.invoke([str(args.flopwire), "hook"], fw_env, json.dumps({
            "hook_event_name": "PostToolUse", "session_id": receiver["id"],
            "transcript_path": str(root / receiver["source"])}))
        if not text.strip():
            return "", ms
        return json.loads(text).get("hookSpecificOutput", {}).get("additionalContext", ""), ms
    def am_inbox():
        text, ms = pilot.invoke([str(args.agent_mail), "inbox", *project_args,
            "--agent", "GreenCastle", "--include-bodies", "--json"], am_env)
        return json.loads(text), ms
    def send(name, marker):
        cmd, env = ([str(args.flopwire), "send", receiver["id"], "--intent", "inform", "--", marker], fw_env)
        if name == "agent-mail":
            cmd, env = ([str(args.agent_mail), "mail", "send", *project_args,
                "--from", "BlueLake", "--to", "GreenCastle", "--subject", marker,
                "--body", marker, "--ack-required", "--json"], am_env)
        text, ms = pilot.invoke(cmd, env)
        value = json.loads(text)
        return str(value.get("id") or value.get("message_id") or ""), ms, value
    def ready(name):
        deadline = time.monotonic() + 30
        while True:
            try:
                if name == "flopwire":
                    text, _ = pilot.invoke([str(args.flopwire), "peers"], fw_env)
                    value = json.loads(text)
                    if receiver["id"] not in json.dumps(value):
                        raise RuntimeError("Synthetic receiver is not discovered yet")
                else:
                    # A local CLI fallback can succeed before the daemon binds.
                    # Require its HTTP listener before testing recipient retrieval.
                    with socket.create_connection(("127.0.0.1", port), timeout=.5):
                        pass
                    am_inbox()
                return
            except Exception:
                if processes[name].poll() is not None or time.monotonic() >= deadline:
                    raise
                time.sleep(.2)
    try:
        for name in ("flopwire", "agent-mail"):
            launch(name)
        deadline = time.monotonic() + 30
        for name in ("BlueLake", "GreenCastle"):
            while True:
                try:
                    # Registration returns credentials. Keep its output out of artifacts.
                    pilot.invoke([str(args.agent_mail), "agents", "register", *project_args,
                        "--program", "synthetic-pilot", "--model", "none", "--name", name, "--json"], am_env)
                    break
                except Exception:
                    if time.monotonic() >= deadline:
                        raise
                    time.sleep(.2)
        for name in processes:
            ready(name)
        fw_hook()  # Remove the standing instruction from measured output.
        rng = random.Random(args.seed)
        for iteration in range(args.runs):
            names = ["flopwire", "agent-mail"]
            rng.shuffle(names)
            for name in names:
                marker = f"pilotmarker{iteration:04d}"
                sample = {"product": name, "iteration": iteration, "marker": marker}
                started = time.perf_counter()
                try:
                    mid, ms, receipt = send(name, marker)
                    if not mid:
                        raise RuntimeError("Send receipt has no message ID")
                    sample.update(message_id=mid, acceptance_ms=ms)
                    if name == "flopwire":
                        text, ms = fw_hook()
                        sample.update(recipient_fetch_ms=ms, hook_output_ms=(time.perf_counter() - started) * 1000,
                            exact_body=marker in text, exact_id=mid in text)
                        # An immediate second hook must not emit this confirmed ID.
                        second, _ = fw_hook()
                        sample["duplicate"] = mid in second
                    else:
                        inbox, ms = am_inbox()
                        matches = [x for x in objects(inbox) if str(x.get("id")) == mid]
                        sample.update(recipient_fetch_ms=ms, inbox_visible_ms=(time.perf_counter() - started) * 1000,
                            exact_id=bool(matches), exact_body=any(x.get("body_md") == marker for x in matches),
                            duplicate=len(matches) > 1)
                        pilot.invoke([str(args.agent_mail), "mail", "ack", *project_args,
                            "--agent", "GreenCastle", mid], am_env)
                    sample["pass"] = sample["exact_id"] and sample["exact_body"] and not sample["duplicate"]
                except Exception as e:
                    sample.update(error=str(e), **{"pass": False})
                report["samples"].append(sample)
                pilot.dump(root / "messaging-results.json", report)
        for name in ("flopwire", "agent-mail"):
            marker = "pilotrecoverymarker"
            if name == "flopwire":
                # The default limit is 30 sends/hour/session. Keep recovery
                # independent of the measured sender's exhausted allowance.
                fw_env["FLOPWIRE_SESSION_ID"] = manifest["sessions"][2]["id"]
                receiver = manifest["sessions"][3]
            mid, _, _ = send(name, marker)
            terminate(processes[name])
            launch(name)
            ready(name)
            if name == "flopwire":
                output, _ = fw_hook()
                recovered = marker in output and mid in output
            else:
                inbox, _ = am_inbox()
                recovered = any(str(x.get("id")) == mid and x.get("body_md") == marker for x in objects(inbox))
            report["recovery"][name] = {"message_id": mid, "pass": recovered,
                "case": "accepted unread message, graceful service restart, recipient retrieval"}
        report["summary"] = {}
        for name in ("flopwire", "agent-mail"):
            rows = [s for s in report["samples"] if s["product"] == name]
            report["summary"][name] = {"attempts": len(rows), "correct": sum(s["pass"] for s in rows)}
            for metric in ("acceptance_ms", "recipient_fetch_ms", "hook_output_ms", "inbox_visible_ms"):
                values = [s[metric] for s in rows if metric in s]
                if values:
                    report["summary"][name][metric] = pilot.summary(values)
        print(json.dumps(report["summary"], indent=2))
    except Exception as e:
        report["errors"].append(str(e))
        raise
    finally:
        for p in processes.values():
            if p.poll() is None:
                terminate(p)
        for log in logs:
            log.close()
        pilot.dump(root / "messaging-results.json", report)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--flopwire", required=True, type=Path)
    parser.add_argument("--agent-mail", required=True, type=Path)
    parser.add_argument("--runs", type=int, default=30)
    parser.add_argument("--seed", type=int, default=1)
    args = parser.parse_args()
    if args.runs < 1:
        parser.error("--runs must be positive")
    run(args)
