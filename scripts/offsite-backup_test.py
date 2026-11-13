#!/usr/bin/env python3
import contextlib
import fcntl
import importlib.util
import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('offsite', Path(__file__).with_name('offsite-backup.py'))
job = importlib.util.module_from_spec(spec)
spec.loader.exec_module(job)


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.root.chmod(0o700)
        self.config = {'encrypted_staging': True, 'staging_dir': str(self.root),
                       'flopwire_command': ['flopwire'], 'restic_command': ['restic']}
        self.calls = []

    def fake(self, command, phase, **kwargs):
        self.calls.append((command, phase))
        if phase == 'backup':
            (self.root / 'snapshot').mkdir()
            (self.root / 'snapshot' / 'manifest.json').write_text('{}')
        if phase == 'upload':
            return '{"message_type":"summary","snapshot_id":"12345678"}\n'
        if phase == 'verify-snapshot':
            return '[{"id":"12345678abcdef"}]'
        return ''

    def execute(self, fake=None):
        with patch.object(job, 'run', side_effect=fake or self.fake), contextlib.redirect_stdout(io.StringIO()):
            job.archive(self.config)

    def test_complete_requires_remote_verification_and_never_prunes(self):
        self.execute()
        state = json.loads((self.root / 'status.json').read_text())
        self.assertEqual(state['status'], 'complete')
        self.assertEqual(state['snapshot_id'], '12345678abcdef')
        self.assertEqual([p for _, p in self.calls], ['backup', 'verify-local', 'upload', 'verify-remote', 'verify-snapshot'])
        self.assertFalse((self.root / 'snapshot').exists())
        self.assertFalse(any('forget' in c or 'prune' in c for c, _ in self.calls))

    def test_failure_preserves_previous_success_and_cleans_staging(self):
        for failed_phase in ['backup', 'verify-local', 'upload', 'verify-remote', 'verify-snapshot']:
            with self.subTest(phase=failed_phase):
                (self.root / 'status.json').write_text('{"last_success":"previous","snapshot_id":"old"}')
                def fail(command, phase, **kwargs):
                    result = self.fake(command, phase, **kwargs)
                    if phase == failed_phase:
                        raise RuntimeError('synthetic failure')
                    return result
                with self.assertRaises(RuntimeError):
                    self.execute(fail)
                state = json.loads((self.root / 'status.json').read_text())
                self.assertEqual(state['status'], 'failed')
                self.assertEqual(state['last_success'], 'previous')
                self.assertEqual(state['snapshot_id'], 'old')
                self.assertFalse((self.root / 'snapshot').exists())

    def test_refuses_leftover_without_deleting_it(self):
        (self.root / 'snapshot').mkdir()
        (self.root / 'snapshot' / 'evidence').write_text('keep')
        with self.assertRaisesRegex(RuntimeError, 'stale'):
            self.execute()
        self.assertEqual((self.root / 'snapshot' / 'evidence').read_text(), 'keep')
        self.assertFalse(self.calls)

    def test_concurrent_run_cannot_remove_active_snapshot(self):
        with (self.root / 'job.lock').open('a') as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            (self.root / 'snapshot').mkdir()
            with self.assertRaises(BlockingIOError):
                self.execute()
            self.assertTrue((self.root / 'snapshot').exists())
        self.assertFalse(self.calls)

    def test_rejects_unacknowledged_or_public_staging(self):
        self.config['encrypted_staging'] = False
        with self.assertRaises(ValueError):
            self.execute()
        self.config['encrypted_staging'] = True
        self.root.chmod(0o755)
        with self.assertRaises(ValueError):
            self.execute()
        self.assertFalse(self.calls)

    def test_incomplete_restic_snapshot_is_failure(self):
        result = type('Result', (), {'returncode': 3, 'stdout': '{"message_type":"summary"}'})()
        with patch.object(job.subprocess, 'run', return_value=result):
            with self.assertRaisesRegex(RuntimeError, 'exit 3'):
                job.run(['restic', 'backup'], 'upload', capture=True)

    def test_age_check_rejects_stale_and_failed_backups(self):
        status = self.root / 'status.json'
        status.write_text(json.dumps({'status': 'complete', 'last_success': job.now()}))
        with contextlib.redirect_stdout(io.StringIO()):
            job.check_age(self.config, 36)
        for state in [
            {'status': 'complete', 'last_success': '2020-01-01T00:00:00+00:00'},
            {'status': 'failed', 'last_success': job.now()},
        ]:
            status.write_text(json.dumps(state))
            with self.assertRaises(RuntimeError):
                job.check_age(self.config, 36)

    def test_recovery_files_are_included_and_missing_files_fail(self):
        recovery = self.root / 'deployment.env'
        recovery.write_text('synthetic deployment settings')
        self.config['recovery_paths'] = [str(recovery)]
        self.execute()
        upload = next(command for command, phase in self.calls if phase == 'upload')
        self.assertEqual(upload[-3:], ['--', 'snapshot', str(recovery)])
        self.assertTrue(recovery.exists())
        recovery.unlink()
        with self.assertRaises(ValueError):
            self.execute()

    def test_missing_snapshot_id_is_failure(self):
        def fake(command, phase, **kwargs):
            return '{}' if phase == 'upload' else self.fake(command, phase, **kwargs)
        with self.assertRaisesRegex(RuntimeError, 'exactly one'):
            self.execute(fake)
        self.assertEqual(json.loads((self.root / 'status.json').read_text())['status'], 'failed')


if __name__ == '__main__':
    unittest.main()
