import importlib.util
import os
from pathlib import Path
import re
import subprocess
import textwrap
import unittest

spec = importlib.util.spec_from_file_location("scope", Path(__file__).with_name("ci-scope.py"))
scope = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scope)


class ScopeTest(unittest.TestCase):
    def test_prose(self):
        self.assertTrue(scope.docs_only(["README.md", "docs/dev/test.md", "notes/log/a.md"]))

    def test_unknown_or_executable_paths_require_tests(self):
        for path in ["go.mod", ".github/workflows/ci.yml", "scripts/test.sh",
                     "docs/perf/nightly-baseline", "docs/example.sh",
                     "internal/webapp/dist/index.html", "plugins/codex/README.md"]:
            with self.subTest(path=path):
                self.assertFalse(scope.docs_only(["README.md", path]))

    def test_empty_diff_fails_closed(self):
        self.assertFalse(scope.docs_only([]))

    def test_rename_from_code_requires_tests(self):
        self.assertFalse(scope.docs_only(["scripts/old.sh", "docs/new.md"]))

    def test_required_gate_outcomes(self):
        root = Path(__file__).resolve().parent.parent
        gates = []
        for workflow, job in [("ci.yml", "test"), ("e2e.yml", "e2e-sync")]:
            source = (root / ".github/workflows" / workflow).read_text()
            job_source = source.split(f"\n  {job}:\n", 1)[1]
            run = re.search(r"(?m)^        run: \|\n((?:          .*\n|\n)+)", job_source)
            self.assertIsNotNone(run)
            gates.append((job, textwrap.dedent(run.group(1))))

        # Execute the actual workflow shell, including bash -e semantics.
        # An unsuccessful first command in an && list does not trigger -e.
        cases = [
            ("success", "false", "skipped", "skipped", True),
            ("success", "true", "success", "success", True),
            ("failure", "false", "skipped", "skipped", False),
            ("cancelled", "false", "skipped", "skipped", False),
            ("success", "", "skipped", "skipped", False),
            ("success", "true", "skipped", "success", False),
            ("success", "true", "failure", "success", False),
            ("success", "false", "success", "success", False),
        ]
        for scope_result, full, go, checks, expected in cases:
            env = dict(os.environ, SCOPE=scope_result, FULL=full,
                       GO_TEST=go, CHECKS=checks, SCENARIOS=go)
            for job, script in gates:
                with self.subTest(job=job, scope=scope_result, full=full, go=go):
                    result = subprocess.run(["bash", "-e", "-c", script],
                                            env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode == 0, expected, result.stdout)


if __name__ == "__main__":
    unittest.main()
