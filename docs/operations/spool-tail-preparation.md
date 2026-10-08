# Prepare spool tails for a legacy reader

`flopwire agent prepare-legacy-tails` converts verified immutable tail files to
the legacy canonical layout. It certifies tail format only. It does not approve
an older binary's schema, policy enforcement, or application compatibility.

Immutable tail APIs are dormant in this build. Existing capture, upload, and
startup sweep callers still use the legacy layout. This command does not enable
immutable runtime writes or parallel uploads.

## Procedure

1. Stop the collector through its service manager.
2. Identify the collector's existing index database and matching spool directory.
3. Retain the deployment checkpoint for explicit recovery.
4. Run the preparation command with those paths:

   ```sh
   flopwire agent prepare-legacy-tails --db /path/to/index.db --spool /path/to/spool --json
   ```

5. Require a successful exit before activating a legacy-only tail reader.
6. Qualify the intended binary's schema and policy compatibility separately.

The command takes the same exclusive index lock as the collector. A running
collector or another index owner prevents preparation. It does not request a
pass from a running agent or stop a service automatically.

`--db` defaults to `FLOPWIRE_INDEX`, or the normal user-cache index path.
`--spool` defaults to `spool` beside the client configuration. Custom collector
paths must refer to the same state and spool pair. The database must already
exist as a regular file. Preparation opens it read-only and creates no schema,
rebuild, sync store, or collector.

## Validation and interruption

Committed generation metadata selects the needed tail hash and size. The
command validates all needed candidates before the first conversion. It also
verifies recognized immutable files before removing obsolete versions.
Nonregular recognized candidates, corrupt immutable bytes, and missing matching
required tails stop preparation. A current native tail may legitimately be unspooled only when its
source metadata and watermark establish that case. An existing mismatching
candidate cannot use that exception.

A verified immutable tail may replace stale regular canonical bytes. Each
conversion uses an atomic rename without allocating another spool copy.
SQLite state and chunk files remain unchanged. Unrecognized tail files and
deployment snapshots remain intact.

Cancellation or process death can leave a mixture of converted and immutable
tails. Completed conversions already match committed hashes. Keep the collector
stopped, resolve the failure, and rerun preparation. The command verifies
canonical files again and resumes the remaining conversions. Do not restore an
old checkpoint automatically; that can discard captures made after it.

The conversion has no directory-fsync or power-loss qualification. Concurrent
spool readers and writers are outside its exclusive offline contract.

## Output

Text and `--json` output contain aggregate counts only:

| Field | Meaning |
| --- | --- |
| `needed` | Committed tails requiring a canonical file or a verified unspooled exception |
| `converted` | Immutable tails renamed to the canonical layout |
| `canonical` | Needed tails already present with matching canonical bytes |
| `unspooled` | Current native tails legitimately absent from the spool |
| `removed_versions` | Verified immutable versions removed after validation and conversion |

These counts do not establish upload completion, recovered history, or CASS
retirement readiness. Follow the [recovery plan](recovery.md) for recovery and
deployment rollback decisions.
