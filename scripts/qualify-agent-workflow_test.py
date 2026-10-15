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


def auth(last=NOW, expires=NOW + datetime.timedelta(days=1)):
    claims = base64.urlsafe_b64encode(json.dumps({"exp": expires.timestamp()}).encode()).decode().rstrip("=")
    return json.dumps({"last_refresh": last.isoformat() if last else None,
                       "tokens": {"refresh_token": "synthetic-refresh", "access_token": "x." + claims + ".x"}}).encode()


class CredentialGuardTests(unittest.TestCase):
    def test_probe_metadata_cases(self):
        for name, raw, reason in [
            ("fresh", auth(), None),
            ("stale", auth(NOW - datetime.timedelta(days=9)), "8-day"),
            ("refresh soon", auth(NOW - datetime.timedelta(days=8) + datetime.timedelta(minutes=10)), "8-day"),
            ("missing timestamp", auth(None), "last_refresh"),
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
        for raw in (auth(NOW - datetime.timedelta(days=900)), auth(None)):
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


if __name__ == "__main__":
    unittest.main()
