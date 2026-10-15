#!/usr/bin/env python3
"""Bounded real-model CLI workflow trial on a private local Flopwire bus.

Copies Codex auth into a scratch home, removes the copy on exit, and keeps
synthetic transcripts/evidence. Requires an authenticated Codex CLI and a
Flopwire binary built from the revision being qualified. No live peer is used.

Run: python3 scripts/qualify-agent-workflow.py --root /tmp/fwqual-new \
    --flopwire /tmp/flopwire-under-test
Uses four paid model turns. A pass proves local inbox recovery after an
explicit resume; it does not imply hook delivery or automatic session wake.
Refuses auth copying when refresh could occur within 30 minutes, matching
the installed probe. Run Codex normally to refresh its source login first.
"""
import argparse
import base64
import datetime
import hashlib
import json
import os
from pathlib import Path
import secrets
import subprocess
import time


def checked_auth(raw, now=None):
    """Mirror cmd/flopwire/probe.go codexRefreshDue; never log token data."""
    now = now or datetime.datetime.now(datetime.timezone.utc)
    until = now + datetime.timedelta(minutes=30)
    try:
        auth = json.loads(raw)
        tokens = auth.get("tokens")
    except (ValueError, AttributeError):
        raise RuntimeError("Codex auth.json is not a JSON object") from None
    if not tokens or not tokens.get("refresh_token"):
        return raw
    try:
        parts = tokens["access_token"].split(".")
        if len(parts) != 3:
            raise ValueError()
        claims = json.loads(base64.urlsafe_b64decode(parts[1] + "=" * (-len(parts[1]) % 4)))
        exp = float(claims["exp"])
        if exp <= 0:
            raise ValueError()
        expiry = datetime.datetime.fromtimestamp(exp, datetime.timezone.utc)
    except (KeyError, ValueError, TypeError, OverflowError, AttributeError):
        expiry = None
    # Codex 0.160.0 and 0.162.1 should_refresh_proactively: readable JWT
    # expiry takes precedence over last_refresh (age is fallback only).
    # https://github.com/openai/codex/blob/rust-v0.162.1/codex-rs/login/src/auth/manager.rs#L3001
    if expiry is not None:
        if expiry - datetime.timedelta(minutes=5) <= until:
            raise RuntimeError("Codex access token refresh is due; run codex normally to refresh before retrying")
        return raw
    try:
        last = datetime.datetime.fromisoformat(auth["last_refresh"].replace("Z", "+00:00"))
        if last.tzinfo is None:
            raise ValueError()
    except (KeyError, ValueError, TypeError, AttributeError):
        raise RuntimeError("Codex last_refresh is missing or unreadable; run codex normally to refresh before retrying") from None
    if last + datetime.timedelta(days=8) <= until:
        raise RuntimeError("Codex 8-day refresh is due; run codex normally to refresh before retrying")
    # Fail closed even when the age fallback would currently allow a run.
    raise RuntimeError("Codex access token expiry is unreadable; run codex normally to refresh before retrying")


def source_read(command, result, owner):
    # Read addresses use a variable-length unique prefix; verify the full
    # identity returned with the source evidence rather than its spelling.
    return (" read " in command and result.get("conversation", {}).get("session_id") == owner
            and bool(result.get("messages")))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, required=True)
    parser.add_argument("--flopwire", type=Path, required=True)
    parser.add_argument("--model", default="gpt-6.1-sol")
    args = parser.parse_args()
    root = args.root.resolve()
    root.mkdir(mode=0o700)  # Never reuse a previous trial's state.
    home = root / "home"
    codex = home / ".codex"
    codex.mkdir(parents=True)
    auth = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))) / "auth.json"
    project = root / "project"
    project.mkdir()
    env = {k: os.environ[k] for k in ("PATH", "TMPDIR", "LANG", "TERM") if k in os.environ}
    env.update(HOME=str(home), CODEX_HOME=str(codex), XDG_CONFIG_HOME=str(home / ".config"),
               XDG_CACHE_HOME=str(home / ".cache"), XDG_DATA_HOME=str(home / ".local/share"),
               FLOPWIRE_CONFIG=str(root / "config.json"), FLOPWIRE_INDEX=str(root / "index.db"),
               FLOPWIRE_SOCKET=str(root / "agent.sock"), FLOPWIRE_CLOUD="off")
    binary = str(args.flopwire.resolve())
    marker = "fwqual" + secrets.token_hex(6)
    answer = "decision-" + secrets.token_hex(8)
    report = {"model": args.model, "marker": marker, "scope": "real model, CLI retrieval and messaging, explicit CLI resume; no hook or MCP qualification", "turns": [],
              "flopwire_sha256": hashlib.sha256(args.flopwire.read_bytes()).hexdigest()}
    daemon = None
    log = None

    def cli(*parts, session=None):
        call_env = dict(env)
        if session:
            call_env.update(FLOPWIRE_SESSION_ID=session, FLOPWIRE_AGENT="codex")
        result = subprocess.run([binary, *parts], env=call_env, cwd=project, text=True, capture_output=True, timeout=30)
        if result.returncode:
            raise RuntimeError(result.stderr[-2000:])
        return json.loads(result.stdout)

    def turn(name, prompt, session=None):
        cmd = ["codex", "exec", "-m", args.model, "--skip-git-repo-check", "--ignore-rules", "--json",
               "-c", 'approval_policy="never"', "-c", 'sandbox_mode="danger-full-access"',
               "-c", 'model_reasoning_effort="low"']
        if session:
            cmd.extend(["resume", session])
        started = time.monotonic()
        with (root / (name + ".jsonl")).open("w") as out, (root / (name + ".stderr")).open("w") as err:
            result = subprocess.run([*cmd, prompt], env=env, cwd=project, stdin=subprocess.DEVNULL,
                                    stdout=out, stderr=err, timeout=240)
        if result.returncode:
            raise RuntimeError(f"{name} exited {result.returncode}; see scratch stderr")
        events = [json.loads(line) for line in (root / (name + ".jsonl")).read_text().splitlines() if line.strip()]
        thread = next((e["thread_id"] for e in events if e.get("type") == "thread.started"), session)
        if not thread:
            raise RuntimeError(f"{name}: no thread ID")
        report["turns"].append({"name": name, "session": thread, "seconds": round(time.monotonic() - started, 2)})
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(f"completed {name}: {thread}", flush=True)
        return thread

    try:
        auth_bytes = checked_auth(auth.read_bytes())
        report["credential_refresh_guard_enforced"] = True
        (codex / "auth.json").write_bytes(auth_bytes)
        (codex / "auth.json").chmod(0o600)
        report["codex_version"] = subprocess.check_output(["codex", "--version"], env=env, text=True).strip()
        log = (root / "daemon.log").open("w")
        daemon = subprocess.Popen([binary, "agent", "run", "--no-sync", "--sweep", "1s", "--db", str(root / "index.db"),
                                   "--socket", str(root / "agent.sock"), "--codex-home", str(codex),
                                   "--claude-projects", str(root / "empty-claude"), "--devin-db", "-", "--opencode-db", "-"],
                                  env=env, cwd=project, stdout=log, stderr=log)
        owner = turn("owner-create", f"This is a synthetic isolated Flopwire qualification. You are the author of decision {marker}. The exact rationale label is {answer}. Run only /bin/sleep 8 once to let the local observer discover this live process. Then state the marker and label in your final answer and end. Do not send messages. A later turn will ask you to answer a colleague about this decision.")
        deadline = time.monotonic() + 30
        while True:
            try:
                found = cli("sessions")
                presence = cli("peers", "--session", owner)
                if owner in json.dumps(found) and presence.get("peers") == []:
                    (root / "owner-ended-presence.json").write_text(json.dumps(presence, indent=2) + "\n")
                    break
            except (RuntimeError, json.JSONDecodeError):
                pass
            if time.monotonic() > deadline:
                raise RuntimeError("owner never indexed and ended; inspect observer lifecycle")
            time.sleep(.2)
        requester = turn("requester-discover", f"This is an isolated workflow trial. Use only the shell to run {binary} retrieval/messaging commands; do not inspect files, environment, process lists, or other tools. You need the rationale for decision {marker}. Discover the producing session from Flopwire transcript evidence, inspect enough evidence to distinguish author from later references, inspect its presence, and send that session one request with --intent request asking it to return its rationale label via a correlated reply. Do not invent session IDs. Use --help as needed. Also attempt one send to nonexistent session 00000000-0000-4000-8000-000000000000, report its refusal truthfully, and do not retry it. End after reporting evidence, target, request ID, delivery state, and limitations; you are not expected to receive a reply this turn.")
        sent = cli("inbox", "--sent", session=requester)
        (root / "sent-before-resume.json").write_text(json.dumps(sent, indent=2) + "\n")
        report.update(owner=owner, requester=requester)
        discovery = [json.loads(line) for line in (root / "requester-discover.jsonl").read_text().splitlines()]
        commands = [e["item"] for e in discovery if e.get("type") == "item.completed" and e.get("item", {}).get("type") == "command_execution"]
        receipts = []
        refused_unknown = False
        inspected_source = False
        for item in commands:
            try:
                value = json.loads(item.get("aggregated_output", ""))
            except json.JSONDecodeError:
                continue
            inspected_source |= source_read(item.get("command", ""), value, owner)
            if value.get("kind") == "send_receipt":
                receipts.append(value)
            refused_unknown |= value.get("error", {}).get("code") == "unknown_recipient"
        if not inspected_source or not refused_unknown:
            raise RuntimeError("missing actual source read or unavailable-recipient refusal")
        if len(receipts) != 1 or receipts[0].get("to", {}).get("session") != owner or receipts[0].get("arrives") != "only_if_resumed":
            raise RuntimeError("request did not target the exact ended owner with only_if_resumed receipt")
        report.update(inspected_source=True, refused_unknown=True, initial_arrives=receipts[0]["arrives"])
        turn("owner-resume", f"You are explicitly resumed for the synthetic trial. Use only shell commands for {binary}. Read your Flopwire inbox, locate the request about decision {marker}, and answer the requesting session with the rationale label you recorded earlier. Send --intent done and --reply-to the actual request message ID. Do not send any other message. End by reporting the receipt.", owner)
        turn("requester-verify", f"The harness explicitly resumed the controlled owner and has now resumed you. Use only shell commands for {binary}. Inspect the thread for your earlier request about {marker}. Verify the reply belongs to that request by reply_to/thread evidence and comes from the producing session. Report the rationale label, evidence IDs, and whether the initial send alone woke the ended owner. Do not send further messages.", requester)
        final = cli("inbox", "--sent", session=requester)
        (root / "sent-after-resume.json").write_text(json.dumps(final, indent=2) + "\n")
        requests = [m for m in sent["messages"] if m.get("intent") == "request" and m.get("to_session") == owner]
        if len(requests) != 1:
            raise RuntimeError("requester did not send exactly one request to the producing session")
        request = requests[0]
        replies = cli("inbox", "--thread", request["id"], session=requester)["messages"]
        (root / "verified-thread.json").write_text(json.dumps(replies, indent=2) + "\n")
        settled_request = next(m for m in replies if m["id"] == request["id"])
        report.update(request_final_state=settled_request.get("state"), request_final_reason=settled_request.get("reason"))
        correlated = [m for m in replies if m.get("reply_to") == request["id"] and m.get("from") == owner
                      and m.get("to_session") == requester and answer in m.get("body", "")]
        if len(correlated) != 1:
            raise RuntimeError("no unique actual owner reply with correct correlation and token")
        verified = [json.loads(line) for line in (root / "requester-verify.jsonl").read_text().splitlines()]
        final_answer = "\n".join(e["item"].get("text", "") for e in verified if e.get("type") == "item.completed"
                                 and e.get("item", {}).get("type") == "agent_message")
        if answer not in final_answer or request["id"] not in final_answer or correlated[0]["id"] not in final_answer:
            raise RuntimeError("requester final answer omitted token or correlation IDs")
        report.update(request_id=request["id"], reply_id=correlated[0]["id"])
        report["expected_answer"] = answer
        report["completed"] = True
    except Exception as error:
        report.update(completed=False, failure=str(error))
        raise
    finally:
        if daemon:
            daemon.terminate()
            try:
                daemon.wait(timeout=10)
            except subprocess.TimeoutExpired:
                daemon.kill()
                daemon.wait()
        if log:
            log.close()
        (codex / "auth.json").unlink(missing_ok=True)
        (root / "report.json").write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
