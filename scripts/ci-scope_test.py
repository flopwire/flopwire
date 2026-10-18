import importlib.util
from pathlib import Path
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


if __name__ == "__main__":
    unittest.main()
