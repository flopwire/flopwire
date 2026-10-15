#!/usr/bin/env python3
"""Checks that benchmark attribution and scoring cannot manufacture recall."""
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("pilot", Path(__file__).with_name("competitive-pilot.py"))
pilot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pilot)


class AttributionTests(unittest.TestCase):
    def test_prompt_and_storage_ids_are_not_session_attribution(self):
        response = {"hits": [{"session_id": "owner", "conversation_id": "17",
            "message_id": "other", "snippet": 'session_id="wrong"'}]}
        self.assertEqual(pilot.session_ids(response), {"owner"})

    def test_known_harness_namespace_maps_to_native_id(self):
        self.assertEqual(pilot.session_ids({"matches": [{"session_id": "codex:native"}]}), {"native"})

    def test_duplicate_results_do_not_inflate_recall(self):
        result = pilot.correctness(["a", "b"], ["a", "a", "a"])
        self.assertEqual(result["recall"], .5)
        self.assertEqual(result["missing"], ["b"])
        self.assertFalse(result["pass"])

    def test_wrong_owner_is_a_failure_even_with_full_recall(self):
        result = pilot.correctness(["a"], ["a", "wrong"])
        self.assertEqual(result["recall"], 1)
        self.assertEqual(result["precision"], .5)
        self.assertFalse(result["pass"])


if __name__ == "__main__":
    unittest.main()
