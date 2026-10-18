import copy
import json
import unittest
from pathlib import Path

from candidates import candidates


class CandidateTests(unittest.TestCase):
    def setUp(self):
        root = Path(__file__).parent / "testdata"
        self.pr = json.loads((root / "pr-247.json").read_text())
        self.sessions = json.loads((root / "sessions.json").read_text())

    def run_candidates(self, target=None):
        return candidates(self.pr, self.sessions, target or self.pr["url"])

    def test_squash_commit_follows_pr_lineage_without_selecting_owner(self):
        result = self.run_candidates(self.pr["mergeCommit"]["oid"])
        rows = result["candidates"]
        self.assertEqual([r["session_id"] for r in rows], ["synthetic-author", "synthetic-reviewer"])
        self.assertEqual(rows[0]["evidence"][0]["kind"], "observed_pr_commit")
        self.assertEqual(rows[1]["evidence"][0]["kind"], "pr_reference")
        self.assertNotIn("owner", result)
        self.assertNotIn("recipient", result)

    def test_multiple_commit_candidates_are_preserved(self):
        second = copy.deepcopy(self.sessions["sessions"][0])
        second["session_id"] = "synthetic-contributor"
        self.sessions["sessions"].append(second)
        self.assertEqual(len(self.run_candidates()["candidates"]), 3)

    def test_prefix_collision_is_explicit(self):
        self.pr["commits"].append({"oid": "69a3719" + "0" * 33})
        evidence = self.run_candidates()["candidates"][0]["evidence"][0]
        self.assertEqual(evidence["kind"], "ambiguous_commit_prefix")
        self.assertEqual(len(evidence["matches"]), 2)

    def test_missing_and_capped_evidence_is_not_a_negative_claim(self):
        self.sessions["complete"] = False
        self.sessions["sessions"] = [{"repository": "flopwire/flopwire",
                                      "session_id": "synthetic-truncated", "more": ["commits", "prs"]}]
        result = self.run_candidates()
        self.assertEqual(result["status"], "no_evidence")
        self.assertEqual(len(result["warnings"]), 2)

    def test_merged_sha_is_distinct_evidence(self):
        self.sessions["sessions"][0]["commits"] = [self.pr["mergeCommit"]["oid"]]
        self.assertEqual(self.run_candidates()["candidates"][0]["evidence"][0]["kind"],
                         "observed_merged_commit")

    def test_repository_and_target_must_match(self):
        with self.assertRaises(ValueError):
            self.run_candidates("f" * 40)
        self.sessions["repository"] = "other/project"
        with self.assertRaises(ValueError):
            self.run_candidates()

    def test_duplicate_session_export_is_refused(self):
        self.sessions["sessions"].append(copy.deepcopy(self.sessions["sessions"][0]))
        with self.assertRaises(ValueError):
            self.run_candidates()

    def test_invalid_or_too_short_sha_is_refused(self):
        self.sessions["sessions"][0]["commits"] = ["69a"]
        with self.assertRaises(ValueError):
            self.run_candidates()


if __name__ == "__main__":
    unittest.main()
