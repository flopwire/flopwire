#!/usr/bin/env python3
"""Archive a verified Flopwire backup in an initialized restic repository."""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def run(command, phase, *, cwd=None, capture=False):
    result = subprocess.run(command, cwd=cwd, stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                            stderr=subprocess.DEVNULL, text=True)
    if result.returncode:
        # Child errors can contain database URLs or object names. Keep the journal private-data free.
        raise RuntimeError(f"{phase} failed (exit {result.returncode})")
    return result.stdout


def write_status(path, state):
    temporary = path.with_suffix('.tmp')
    with temporary.open('w') as out:
        json.dump(state, out, indent=2)
        out.write('\n')
        out.flush()
        os.fsync(out.fileno())
    os.replace(temporary, path)


def archive(config):
    if config.get('encrypted_staging') is not True:
        raise ValueError('encrypted_staging must acknowledge an actually encrypted staging filesystem')
    root = Path(config['staging_dir'])
    if not root.is_absolute() or root.is_symlink() or not root.is_dir():
        raise ValueError('staging_dir must be an existing absolute directory, not a symlink')
    if root.stat().st_mode & 0o077:
        raise ValueError('staging_dir must be private (mode 0700)')
    state_root = Path(config.get('state_dir', str(root)))
    if not state_root.is_absolute() or state_root.is_symlink() or not state_root.is_dir() or state_root.stat().st_mode & 0o077:
        raise ValueError('state_dir must be an existing private absolute directory, not a symlink')
    command = config['flopwire_command']
    if not isinstance(command, list) or not command or not all(isinstance(v, str) for v in command):
        raise ValueError('flopwire_command must be a nonempty argv array')
    restic = config.get('restic_command', ['restic'])
    if not isinstance(restic, list) or not restic or not all(isinstance(v, str) for v in restic):
        raise ValueError('restic_command must be a nonempty argv array')
    recovery = config.get('recovery_paths', [])
    if not isinstance(recovery, list) or not all(isinstance(v, str) and Path(v).is_absolute() and Path(v).exists() for v in recovery):
        raise ValueError('recovery_paths must list existing absolute files or directories')
    mapped = Path(config.get('command_staging_dir', str(root)))
    if not mapped.is_absolute():
        raise ValueError('command_staging_dir must be absolute')
    # Hold one lock through creation, upload, verification and cleanup.
    with (state_root / 'job.lock').open('a') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        snapshot = root / 'snapshot'
        if snapshot.exists() or snapshot.is_symlink():
            raise RuntimeError('stale snapshot directory: inspect and remove it before retrying')
        status_path = state_root / 'status.json'
        state = json.loads(status_path.read_text()) if status_path.exists() else {}
        state.update(status='running', started_at=now(), phase='backup')
        write_status(status_path, state)
        try:
            run(command + ['backup', '--encrypted-destination', '--object-workers',
                           str(config.get('object_workers', 4)), '--output', str(mapped / 'snapshot')], 'backup')
            state['phase'] = 'verify-local'
            run(command + ['backup-verify', '--input', str(mapped / 'snapshot')], 'verify-local')
            state['phase'] = 'upload'
            output = run(restic + ['backup', '--json', '--quiet', '--force', '--host',
                                  config.get('host', 'flopwire-server'), '--tag', 'flopwire', '--', 'snapshot'] + recovery,
                         'upload', cwd=root, capture=True)
            summaries = [json.loads(line) for line in output.splitlines() if line.strip()]
            ids = [v['snapshot_id'] for v in summaries if v.get('message_type') == 'summary' and v.get('snapshot_id')]
            if len(ids) != 1:
                raise RuntimeError('upload did not return exactly one completed snapshot')
            state['phase'] = 'verify-remote'
            # Check repository metadata before declaring off-site success. Full byte reads and
            # application restores belong to the periodic recovery drill, not each nightly run.
            run(restic + ['check'], 'verify-remote')
            snapshots = json.loads(run(restic + ['snapshots', '--json', ids[0]], 'verify-snapshot', capture=True))
            if len(snapshots) != 1 or not snapshots[0]['id'].startswith(ids[0]):
                raise RuntimeError('uploaded snapshot was not found in repository')
            state.update(status='complete', phase='complete', last_success=now(), snapshot_id=snapshots[0]['id'])
            state.pop('error', None)
        except Exception as error:
            state.update(status='failed', error=str(error))
            raise
        finally:
            state['finished_at'] = now()
            write_status(status_path, state)
            # This run alone created snapshot after the lock and absence check. Never prune
            # the repository, and never remove a leftover directory from another run.
            if snapshot.is_dir() and not snapshot.is_symlink():
                shutil.rmtree(snapshot)
        print(json.dumps({'status': 'complete', 'snapshot_id': state['snapshot_id']}))


def check_age(config, hours):
    if hours <= 0:
        raise ValueError('maximum backup age must be positive')
    state = json.loads((Path(config.get('state_dir', config['staging_dir'])) / 'status.json').read_text())
    last = datetime.datetime.fromisoformat(state['last_success'])
    age = (datetime.datetime.now(datetime.timezone.utc) - last).total_seconds() / 3600
    if age < 0 or age > hours or state['status'] == 'failed':
        raise RuntimeError('offsite backup is failed or stale')
    print(json.dumps({'status': 'healthy', 'age_hours': round(age, 2)}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', required=True)
    parser.add_argument('--check-age-hours', type=float, help='check last off-site success without creating a backup')
    args = parser.parse_args()
    os.umask(0o077)
    try:
        config = json.loads(Path(args.config).read_text())
        if args.check_age_hours is not None:
            check_age(config, args.check_age_hours)
        else:
            archive(config)
    except Exception as error:
        print(f'offsite backup: {error}', file=sys.stderr)
        return 1
    return 0


if __name__ == '__main__':
    sys.exit(main())
