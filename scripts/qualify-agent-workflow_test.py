#!/usr/bin/env python3
"""Synthetic credential guard and cleanup checks; no model or network calls."""
import base64
import datetime
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("qualify-agent-workflow.py")
SPEC = importlib.util.spec_from_file_location("qualification", SCRIPT)
qualification = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(qualification)
NOW = datetime.datetime(2026, 10, 15, tzinfo=datetime.timezone.utc)


def auth(last=NOW, expires=NOW + datetime.timedelta(days=1), readable=True):
    claims = base64.urlsafe_b64encode(json.dumps({"exp": expires.timestamp()}).encode()).decode().rstrip("=")
    return json.dumps({"last_refresh": last.isoformat() if last else None,
                       "tokens": {"refresh_token": "synthetic-refresh", "access_token": "x." + claims + ".x" if readable else "opaque"}}).encode()


class CredentialGuardTests(unittest.TestCase):
    def test_source_read_checks_full_returned_identity(self):
        owner = "01a13fd2-e7ae-7303-93a0-5235f7d5a5d1"
        result = {"conversation": {"session_id": owner}, "messages": [{"text": "synthetic source"}]}
        self.assertTrue(qualification.source_read("flopwire read 01a13fd2/123", result, owner))
        self.assertFalse(qualification.source_read("flopwire read 01a13fd2/123", result, "different-owner"))
        self.assertFalse(qualification.source_read("flopwire read 01a13fd2/123", {"conversation": {"session_id": owner}}, owner))
        self.assertFalse(qualification.source_read("flopwire read 01a13fd2/123", {"conversation": {"session_id": owner}, "messages": [{"text": ""}]}, owner))

    def test_native_text_source_read_requires_header_and_evidence(self):
        owner = "01a13fd2-e7ae-7303-93a0-5235f7d5a5d1"
        command = "flopwire read 01a13fd2/123"
        header = f"# {owner} agent=codex repo=/tmp/synthetic title=\"fixture\"\n"
        message = ">> 01a13fd2/123  2026-10-15 12:00Z  user\n     1  Original synthetic evidence.\n"
        self.assertTrue(qualification.source_read(command, "[scope: local device]\n" + header + message, owner))
        self.assertTrue(qualification.source_read(command, header + message.replace(">> ", "   ").replace("     1  ", "    "), owner))
        self.assertFalse(qualification.source_read(command, header + message, "different-owner"))
        self.assertFalse(qualification.source_read(command, header, owner))
        self.assertFalse(qualification.source_read(command, header + message.splitlines()[0] + "\n", owner))
        self.assertFalse(qualification.source_read(command, header + message.splitlines()[0] + "\n     1  \n", owner))
        self.assertFalse(qualification.source_read(command, header + message.replace("01a13fd2/123", "unrelated/123"), owner))
        wrong_header = '# other-owner agent=codex title="' + owner + '"\n'
        self.assertFalse(qualification.source_read(command, wrong_header + message + "    " + header, owner))
        self.assertFalse(qualification.source_read(command, "    " + header + message, owner))
        self.assertFalse(qualification.source_read(command, wrong_header + header + message, owner))

    def test_probe_metadata_cases(self):
        for name, raw, reason in [
            ("fresh", auth(), None),
            ("old timestamp, readable expiry", auth(NOW - datetime.timedelta(days=9)), None),
            ("stale fallback", auth(NOW - datetime.timedelta(days=9), readable=False), "8-day"),
            ("refresh soon", auth(NOW - datetime.timedelta(days=8) + datetime.timedelta(minutes=10), readable=False), "8-day"),
            ("missing timestamp, readable expiry", auth(None), None),
            ("missing fallback timestamp", auth(None, readable=False), "last_refresh"),
            ("refresh boundary", auth(expires=NOW + datetime.timedelta(minutes=35)), "refresh is due"),
            ("access expires soon", auth(expires=NOW + datetime.timedelta(minutes=20)), "refresh is due"),
            ("access outside margin", auth(expires=NOW + datetime.timedelta(minutes=40)), None),
            ("bad jwt", json.dumps({"last_refresh": NOW.isoformat(), "tokens": {"refresh_token": "synthetic", "access_token": "bad"}}).encode(), "expiry"),
            ("API key", b'{"tokens":null}', None),
            ("bad json", b'{', "JSON"),
        ]:
            with self.subTest(name=name):
                if reason:
                    with self.assertRaisesRegex(RuntimeError, reason):
                        qualification.checked_auth(raw, NOW)
                else:
                    self.assertEqual(qualification.checked_auth(raw, NOW), raw)

    def test_rejected_metadata_never_copied(self):
        for raw in (auth(NOW - datetime.timedelta(days=900), readable=False), auth(None, readable=False)):
            with self.subTest(raw_length=len(raw)), tempfile.TemporaryDirectory() as tmp:
                source = Path(tmp) / "source"
                source.mkdir()
                (source / "auth.json").write_bytes(raw)
                root = Path(tmp) / "trial"
                with patch.dict(os.environ, {"CODEX_HOME": str(source)}), patch.object(sys, "argv", [str(SCRIPT), "--root", str(root), "--flopwire", str(SCRIPT)]), patch.object(qualification.subprocess, "check_output") as version:
                    with self.assertRaises(RuntimeError):
                        qualification.main()
                version.assert_not_called()
                self.assertFalse((root / "home/.codex/auth.json").exists())
                self.assertEqual((source / "auth.json").read_bytes(), raw)
                self.assertFalse(json.loads((root / "report.json").read_text())["completed"])

    def test_setup_failure_removes_fresh_copy(self):
        with tempfile.TemporaryDirectory() as tmp:
            source = Path(tmp) / "source"
            source.mkdir()
            (source / "auth.json").write_bytes(auth(datetime.datetime.now(datetime.timezone.utc), datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=1)))
            root = Path(tmp) / "trial"
            with patch.dict(os.environ, {"CODEX_HOME": str(source)}), patch.object(sys, "argv", [str(SCRIPT), "--root", str(root), "--flopwire", str(SCRIPT)]), patch.object(qualification.subprocess, "check_output", side_effect=FileNotFoundError("synthetic missing codex")):
                with self.assertRaises(FileNotFoundError):
                    qualification.main()
            self.assertFalse((root / "home/.codex/auth.json").exists())
            self.assertFalse(json.loads((root / "report.json").read_text())["completed"])


class CoverageInterpretationVerifier(unittest.TestCase):
    def test_owned_process_group_cleanup_on_success_and_timeout(self):
        import subprocess
        import time
        for timeout in (False, True):
            with tempfile.TemporaryDirectory() as tmp:
                marker = Path(tmp) / "child.pid"
                code = ("import pathlib,subprocess,sys,time; "
                        "p=subprocess.Popen([sys.executable,'-c','import signal,time; signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(60)']); "
                        "pathlib.Path(sys.argv[1]).write_text(str(p.pid)); "
                        + ("time.sleep(60)" if timeout else "time.sleep(.1)"))
                cmd = [sys.executable, "-c", code, str(marker)]
                with open(os.devnull, "w") as out:
                    if timeout:
                        with self.assertRaises(subprocess.TimeoutExpired):
                            qualification.run_owned_coverage_process(cmd, os.environ.copy(), tmp, out, out, timeout=1)
                    else:
                        self.assertEqual(qualification.run_owned_coverage_process(cmd, os.environ.copy(), tmp, out, out), 0)
                pid = int(marker.read_text())
                deadline = time.monotonic() + 2
                while True:
                    state = subprocess.run(["ps", "-p", str(pid), "-o", "stat="], capture_output=True, text=True).stdout.strip()
                    if not state or state.startswith("Z"):
                        break
                    if time.monotonic() > deadline:
                        self.fail("owned descendant survived process-group cleanup")
                    time.sleep(.02)

    def test_fixture_set_must_be_complete_and_unique(self):
        names = ["empty", "unavailable", "pending", "held"]
        fixture = lambda ns: {"instructions": "native guidance", "cases": [{"name": n} for n in ns]}
        qualification.validate_coverage_fixtures(fixture(names))
        for invalid in ([], names[:2], names + ["held"], names[:-1] + ["empty"]):
            with self.assertRaises(ValueError):
                qualification.validate_coverage_fixtures(fixture(invalid))

    def test_rejects_global_absence_even_with_correct_observations(self):
        answer = dict(pending_upload_generations=0, pending_parses=0, policy_holds=0,
                      self_excluded=False, global_absence_established=False,
                      interpretation="No matches in this scope; discovery and other devices unknown.",
                      next_step="Inspect intended source coverage.")
        self.assertTrue(qualification.coverage_answer_valid("empty", answer))
        answer["global_absence_established"] = True
        self.assertFalse(qualification.coverage_answer_valid("empty", answer))

    def test_unavailable_is_not_zero_and_backlog_is_not_caught_up(self):
        answer = dict(pending_upload_generations=None, pending_parses=None, policy_holds=None,
                      self_excluded=False, global_absence_established=False,
                      interpretation="Diagnostics unavailable.", next_step="Inspect agent status.")
        self.assertTrue(qualification.coverage_answer_valid("unavailable", answer))
        self.assertFalse(qualification.coverage_answer_valid("empty", answer))
        self.assertFalse(qualification.coverage_answer_valid("pending", answer))


if __name__ == "__main__":
    unittest.main()
