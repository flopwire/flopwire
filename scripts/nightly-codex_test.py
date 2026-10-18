#!/usr/bin/env python3
"""Exercise credential isolation and fail-closed reporting without model calls."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("nightly", Path(__file__).with_name("nightly-codex.py"))
nightly = importlib.util.module_from_spec(spec)
spec.loader.exec_module(nightly)


class NightlyTest(unittest.TestCase):
    def exercise(self, report=None, login_failure=False, timeout=False):
        key = "synthetic-secret-for-test"
        homes = []

        def fake_run(args, env, limit, stdin=None):
            self.assertNotIn("OPENAI_API_KEY", env)
            self.assertNotIn("GH_TOKEN", env)
            homes.append(Path(env["CODEX_HOME"]))
            if args[1] == "--version":
                return 0, "codex-cli fixture\n", ""
            if args[1] == "login":
                self.assertEqual(stdin, key + "\n")
                (homes[-1] / "auth.json").write_text(key)
                return (1 if login_failure else 0), key, ""
            self.assertIn("--as-installed", args)
            if timeout:
                raise subprocess.TimeoutExpired(args, limit)
            result = report if report is not None else {
                "results": [{"case": c, "pass": True, "evidence": key} for c in nightly.EXPECTED_CASES]}
            return 0, json.dumps(result), key

        with tempfile.TemporaryDirectory() as tmp, patch.dict(os.environ, {
            "OPENAI_API_KEY": key, "GH_TOKEN": "synthetic-github-token", "GITHUB_STEP_SUMMARY": ""
        }), patch.object(nightly, "run_owned", side_effect=fake_run):
            output = Path(tmp) / "artifacts"
            code = nightly.main("/tmp/flopwire", output)
            summary = json.loads((output / "summary.json").read_text())
            self.assertEqual({p.name for p in output.iterdir()}, {"summary.json", "probe.log"})
            for path in output.iterdir():
                self.assertNotIn(key, path.read_text())
            self.assertTrue(homes)
            self.assertTrue(all(not home.exists() for home in homes))
            return code, summary

    def test_success_is_complete_and_redacts_credentials(self):
        code, report = self.exercise()
        self.assertEqual(code, 0)
        self.assertEqual(report["status"], "passed")

    def test_empty_failed_duplicate_and_skipped_cases_fail(self):
        good = [{"case": c, "pass": True} for c in nightly.EXPECTED_CASES]
        for report in [[], {}, {"results": None}, {"results": [None]}, {"results": []}, {"results": good[:-1]},
                       {"results": good + [good[0]]},
                       {"results": good, "skipped": ["credential unavailable"]},
                       {"results": [dict(r, **({"pass": False} if i == 0 else {})) for i, r in enumerate(good)]}]:
            with self.subTest(report=report):
                code, result = self.exercise(report)
                self.assertEqual(code, 1)
                self.assertEqual(result["status"], "failed")

    def test_login_failure_and_timeout_remove_credentials(self):
        for kwargs in [{"login_failure": True}, {"timeout": True}]:
            code, result = self.exercise(**kwargs)
            self.assertEqual(code, 1)
            self.assertIn("error", result)

    def test_missing_key_never_starts_a_process(self):
        with patch.dict(os.environ, {"OPENAI_API_KEY": ""}), patch.object(nightly, "run_owned") as run:
            with self.assertRaisesRegex(ValueError, "no live checks ran"):
                nightly.main("unused", "unused")
            run.assert_not_called()

    def test_usage_collection_failure_preserves_diagnostics(self):
        with patch.object(nightly, "usage_totals", side_effect=ValueError("malformed usage")):
            code, report = self.exercise()
        self.assertEqual(code, 1)
        self.assertEqual(report["usage_error"], "malformed usage")
        self.assertEqual(report["status"], "failed")

    def test_usage_counts_final_totals_and_keeps_reviewer_separate(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, model, totals in [("main", "luna", [10, 30, 30]), ("child", "luna", [7]), ("review", "reviewer", [9])]:
                events = [{"type": "turn_context", "payload": {"model": model}}]
                events += [{"payload": {"type": "token_count", "info": {"total_token_usage": {"input_tokens": n}}}} for n in totals]
                (root / f"rollout-{name}.jsonl").write_text("\n".join(map(json.dumps, events)) + "\n{partial")
            self.assertEqual(nightly.usage_totals(root), {"luna": {"input_tokens": 37}, "reviewer": {"input_tokens": 9}})

    def test_process_timeout_returns_promptly(self):
        with self.assertRaises(subprocess.TimeoutExpired):
            nightly.run_owned([sys.executable, "-c", "import time; time.sleep(60)"], os.environ.copy(), .1)


if __name__ == "__main__":
    unittest.main()
