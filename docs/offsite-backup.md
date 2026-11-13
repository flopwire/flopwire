# Off-site disaster recovery

`scripts/offsite-backup.py` wraps the existing coordinated backup command
with restic upload and repository verification. The Flopwire backup format
is unchanged. Each run adds a dated snapshot; restic deduplicates repeated
content. The script never runs `forget` or `prune`. Older snapshots retain
content that was later deleted or redacted on the server.

The job still reads and stages a full application backup each time. Remote
deduplication reduces upload and stored bytes, not local disk requirements.
Allow enough encrypted staging space for the database dump and all referenced
objects. Uploads continue while the application backup runs; physical purges
wait for its shared lock.

## Provision

Use a private bucket in a separate account/provider from the server, with
no automatic expiration of current repository objects. Backblaze B2 through
its S3-compatible endpoint works with restic. Scope credentials to the backup
bucket. Restic needs to create and remove its repository locks; a bucket-wide
Object Lock rule can prevent normal operation. This job retains snapshots
but does not provide protection against a compromised credential deleting
them. Configure provider immutability separately if that protection is needed.

Install Python 3, restic, and the production Flopwire binary or Compose image.
Keep the deployed Flopwire version available for recovery. Save the restic
password, bucket details, and independent recovery credentials in your password
manager. Losing the password makes the repository unrecoverable.

1. Mount encrypted storage at `/mnt/flopwire-backup` and set its directory mode
   to `0700`. Verify the encryption and reboot mount behavior. The
   `encrypted_staging` setting acknowledges encryption; it does not create it.
2. Install the script at `/opt/flopwire-backup/offsite-backup.py` and copy
   `deploy/systemd/flopwire-backup.config.example.json` to
   `/etc/flopwire-backup/config.json`. Adjust the Compose project directory,
   staging mount, and command arguments to the actual deployment. Use only
   trusted, administrator-owned configuration.
3. Add `recovery_paths` to the JSON config: an array of absolute host paths for
   deployment configuration, required secrets, and the TLS certificate/key
   devices pin. Restic includes these alongside the coordinated snapshot.
   Do not include live Postgres or object-store data directories. Keep recovery
   passwords independently available rather than only inside their own backup.
4. Create `/etc/flopwire-backup/restic.env` with mode `0600`:

   ```sh
   RESTIC_REPOSITORY=s3:https://s3.REGION.backblazeb2.com/BUCKET/flopwire
   AWS_ACCESS_KEY_ID=BUCKET_SCOPED_KEY_ID
   AWS_SECRET_ACCESS_KEY=BUCKET_SCOPED_APPLICATION_KEY
   RESTIC_PASSWORD_FILE=/etc/flopwire-backup/repository-password
   ```

   Write the repository password to the named file with mode `0600`. Load
   these variables into the operator shell before running restic manually.
5. Run `restic init` once, then run the script with `--config` and perform
   the recovery drill below before enabling the timer.
6. Install `deploy/systemd/flopwire-backup.service` and
   `deploy/systemd/flopwire-backup.timer` in `/etc/systemd/system`, then run:

   ```sh
   systemctl daemon-reload
   systemctl enable --now flopwire-backup.timer
   systemctl list-timers flopwire-backup.timer
   ```

The timer runs nightly at 04:00 UTC, with up to 15 minutes of jitter, and
catches up after downtime. The service requires the staging mount. Ensure the
mount is actually encrypted before enabling it; a directory alone does not
establish that fact.

## Monitor

The job writes private `status.json` in the staging directory. `last_success`
and `snapshot_id` change only after upload succeeds, restic checks repository
metadata, and the new snapshot can be listed. A failed attempt preserves the
previous successful recovery point. Exit code 3 from restic, which indicates
an incomplete backup, is a failure.

Connect the following command to your existing monitoring system with an alert
on nonzero exit or missing host heartbeat:

```sh
python3 /opt/flopwire-backup/offsite-backup.py \
  --config /etc/flopwire-backup/config.json --check-age-hours 36
```

Installing the timer does not configure external alerts. The console's
`backup.complete` audit event records the local application backup only; it
does not establish off-site success. Inspect failures with
`systemctl status flopwire-backup.service`. The job reports the failed phase
and exit code without printing child output that may include credentials or
transcript paths. Reproduce that phase in a private operator terminal for
more detail.

A process lock prevents overlapping runs. Normal failures clean the temporary
snapshot. After a power loss or forced termination, the job refuses a leftover
`snapshot` directory. Confirm no job is running, inspect it, and remove that
local staging directory before retrying. Remote snapshots are unaffected.

## Recovery drill

Run this before relying on the backup and periodically afterward. Use a
separate encrypted recovery directory and empty, isolated Postgres and object
storage targets. Never run the drill against production targets.

```sh
restic check --read-data
restic snapshots --tag flopwire
restic restore SNAPSHOT_ID --target /mnt/encrypted-recovery --verify
```

Restic reconstructs original absolute paths below the target. Locate the
application `snapshot/manifest.json`, then use the matching Flopwire release:

```sh
flopwire backup-verify --input RESTORED_SNAPSHOT_DIRECTORY
flopwire restore --input RESTORED_SNAPSHOT_DIRECTORY
```

Set `DATABASE_URL` and the `S3_*` variables to the empty recovery targets
before `restore`. Restore configuration and TLS files to the replacement host,
start the server in isolation, and run a known provenance search. Check the
restored users, devices, and object inventory. Record which snapshot and
release passed. A successful restic upload or metadata check alone does not
prove application recovery.

## Synthetic validation

`make test` runs failure-path tests for the operator script. With isolated
Postgres/S3 test variables configured and restic, Python, `pg_dump`, and
`pg_restore` available, run:

```sh
FLOPWIRE_TEST_RESTIC=1 go test -race ./internal/backup \
  -run TestBackupVerifyRestoreRoundTrip -count=1 -v
```

This runs the actual script twice, checks both snapshots remain, reads all
repository bytes, retrieves the first snapshot after staging cleanup, and
restores its database and objects into empty test targets. Its repository is
local and its data is synthetic; this does not test B2 connectivity or the
production server's scheduler.
