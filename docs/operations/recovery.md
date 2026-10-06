# Production recovery plan

Status: procedure pending full production qualification, 2026-10-06.
This document does not authorize a maintenance outage, endpoint cutover,
key deletion, or backup deletion.

The earlier lab backup passed a full restore test. On October 6, the coordinated
production backup completed and verification passed for 66,213 objects. An
isolated database restore and startup migration rehearsal also completed.
The full production database and raw-object restore, independent key escrow,
and off-host backup retrieval remain unverified. See the
[recorded qualification status](../perf/shared-stack-2026-10-03.md#production-backup-and-migration-rehearsal-2026-10-06).
The owner chose to keep the existing local secret files while preparing this
plan. Keep the CASS snapshots and working recovery key until the replacement
recovery path passes verification.

## Assign production and staging roles

1. Record the existing capacity VM as the production deployment.
2. Record its allocated memory and the shared host's reserved memory.
3. Use the separate persistent staging VM for destructive tests and restore drills.
   The `flopwire` project contains `flopwire-staging-20261006`, allocated
   4 GiB RAM, four virtual CPUs, and 100 GiB disk.
4. Restrict the staging endpoint to designated operators.
5. Keep production collectors disconnected from staging.
6. Give staging separate database and object-storage targets.

Use the capacity report for the measured allocation:
[shared stack trial](../perf/shared-stack-2026-10-03.md).

## Record the recovery dependencies

1. Record the production Flopwire commit and binary checksum.
2. Record the PostgreSQL and object-storage versions.
3. Record the applied migration names and checksums.
4. Retain the matching binary and migration files.
5. Record the service configuration without secret values.
6. Record the private endpoint and its verified certificate fingerprint privately.
7. Record the protected locations of server credentials and TLS key material.
8. Record how an operator can obtain those secrets during host loss.

The database backup does not include server environment files, TLS private
keys, Tailscale enrollment, or laptop configuration. Recover those separately.
Do not include secret values or transcript samples in public incident records.

Pre-release migrations can change in place. Restore with the production build
that created the backup first. A new build can reject the restored migration
checksums. Test any upgrade separately after recovery succeeds.

## Create a coordinated production backup

1. Confirm the operator environment targets the current production database.
2. Confirm that `S3_BUCKET` names the current production bucket.
3. Load the protected database and object-storage credentials.
4. Disable shell tracing before loading secrets.
5. Mount an encrypted backup destination.
6. Choose a new or empty output directory on that destination.
7. Confirm that pending archived-message redaction repairs have finished.
8. Record the production build identifier with the backup.
9. Run the coordinated backup command.

```sh
flopwire backup --encrypted-destination --output /encrypted-backups/production-checkpoint
flopwire backup-verify --input /encrypted-backups/production-checkpoint
```

These paths are examples. Use the encrypted destination selected by the
operator. The flag acknowledges encryption; it does not encrypt the files.

The commands use `DATABASE_URL`, `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`,
`S3_BUCKET`, and optional `S3_SECURE`. Use PostgreSQL client tools compatible
with the server. The backup command requires `pg_dump` on `PATH`.

The command coordinates the database snapshot and committed chunk inventory.
It verifies copied object content addresses. Uploads can continue during the
backup. Physical purges wait for its shared purge lock.

1. Confirm that `state.json` reports `complete`.
2. Confirm that `manifest.json` exists.
3. Confirm that `backup-verify` reports `verified: true`.
4. Keep the entire backup directory together.
5. Keep failed or incomplete attempts outside the verified-backup inventory.

Do not use `--allow-pending-redaction-repair` for this qualification backup.
Follow the redaction-repair procedure in the [main runbook](../runbook.md#recover)
if the backup reports `redaction_repair_pending`.

## Establish independent recovery copies

1. Select an encrypted backup destination outside the Pandora host.
2. Copy the completed backup through an authenticated encrypted channel.
3. Keep the destination encrypted while stored.
4. Retrieve the copy without using the original host.
5. Decrypt or mount the retrieved copy in staging.
6. Run `flopwire backup-verify` against the retrieved directory.
7. Record the successful retrieval and verification privately.

The application backup does not provide off-host transport or encryption.
Choose those mechanisms before executing this procedure.

1. Select a recovery-key escrow outside the owner's Mac.
2. Keep the escrow independent of the encrypted backup location.
3. Store the key through the selected protected mechanism.
4. Retrieve the escrowed key without reading the working Mac key file.
5. Use that key to open the retrieved backup copy in staging.
6. Record the successful key recovery privately.
7. Keep the working key file until the complete restore drill passes.

## Restore into an isolated VM

1. Provision a new empty PostgreSQL database in staging.
2. Provision a new empty object bucket in staging.
3. Keep the staging API stopped before the restore.
4. Load credentials that can access only the staging targets.
5. Confirm that `DATABASE_URL` names the staging database.
6. Confirm that `S3_ENDPOINT` and `S3_BUCKET` name the staging object target.
7. Run verification against the retrieved backup copy.
8. Run the matching production build's restore command.

```sh
flopwire backup-verify --input /encrypted-backups/retrieved-checkpoint
flopwire restore --input /encrypted-backups/retrieved-checkpoint --state-output /encrypted-backups/restore-state.json
```

The restore command requires `pg_restore` on `PATH`. It refuses nonempty
targets. It verifies restored objects and checks the database object inventory.
It does not restore into an existing production database or bucket.

1. Confirm that the command reports `data_restored: true`.
2. Confirm that the restore state reports `complete`.
3. Retain the restored state file with the private drill record.
4. Start the matching server build against the restored targets.
5. Check server readiness with certificate verification enabled.

If restore fails, keep the state file. Provision fresh empty targets for the
next attempt. A failed restore can leave partial objects or database state.

## Accept the recovered deployment

1. Record live message, conversation, source, and chunk counts before startup.
2. Compare those counts with the saved checkpoint inventory, if available.
3. Record counts by harness and device.
4. Verify the restored migration checksums against the retained build.
5. Confirm that parser work settles without failed or quarantined sources.
6. Verify session listing for Claude, Codex, and Devin CLI.
7. Verify shared search with a known private query for each harness.
8. Compare selected message text, ordering, and addresses with checkpoint samples.
9. Compare selected archived records with their checkpoint raw bytes.
10. Verify a recovered CASS record's provenance and content.
11. Verify Claude and Codex raw reads at known addresses.
12. Verify Devin archived exports through supported source ranges.
13. Verify deletion and access restrictions using disposable staging records.

Keep checkpoint samples encrypted. Use aggregate results in public reports.
Live production counts can change after the backup snapshot. Counts from a
different time are not an exact checkpoint oracle. If exact source counts are
required, arrange a separate maintenance checkpoint with writers paused.
The restore command's inventory verification remains mandatory in either case.

1. Use a separate operator profile for staging authentication.
2. Verify the staging TLS fingerprint through a trusted channel.
3. Authenticate with an authorized restored account.
4. Test restored credential behavior without starting production collectors.
5. Test re-enrollment with a disposable staging device.
6. Confirm that invalid credentials and certificate pins fail visibly.
7. Record how each Mac will regain authenticated access after a real cutover.

Restored device credentials can differ from credentials rotated after the
backup. Endpoint changes can also require a new trusted certificate pin.
Do not bypass certificate verification or silently reuse production profiles
for the drill. Follow the [login and credential procedures](../runbook.md).

## Recover from a real outage

1. Stop writes to the damaged deployment during an approved recovery window.
2. Preserve the damaged state for diagnosis.
3. Select a verified backup within the accepted data-loss window.
4. Restore into replacement isolated targets using this procedure.
5. Complete the acceptance checks before changing the production endpoint.
6. Recover the TLS identity or verify a replacement identity independently.
7. Renew authentication or re-enroll affected clients.
8. Resume collectors after the replacement endpoint passes verification.
9. Verify that uploads and indexing settle.
10. Audit history gaps against retained laptop sources and CASS archives.

Do not assume collectors will recreate every record newer than the backup.
CASS-only recovery records and removed native files can lack another source.
Do not use a database rollback as the default application-version rollback.
Migration compatibility and post-checkpoint writes require separate decisions.

## Decisions required before execution

- Select the off-host encrypted backup destination and copy mechanism.
- Select the independent recovery-key escrow and its access policy.
- Set the acceptable data-loss window and recovery-time target.
- Set backup frequency and retention.
- Define expiry for backups containing subsequently deleted content.
- Set the maintenance and endpoint-cutover procedure.
- Set credential revocation requirements after suspected compromise.
- Decide when to retire the working key file and migration archives.

Do not delete the working key, CASS snapshots, or existing verified backups
while those recovery and retention decisions remain unresolved.
