# Local Code and Cowork rollout proposal

This proposal requires owner approval before production changes. No server or Mac collector has been upgraded during qualification.

## Candidate and release gates

The release candidate is `a41dc8edbe12ddc8c9c9652d3ad6116da791ecba`. It contains the reviewed client, scoped Code authorization, and both companion-first and native-main server identity fixes. It advertises policy placements capability one and serial concurrency one. Refreshed required CI and owner approval remain required before rollout.

Clean builds used Go 1.26.6, `CGO_ENABLED=0`, `-trimpath`, and version, full commit, and clean-state linker values. The Mac artifact reports version `a41dc8edbe12`.

| Artifact | SHA256 |
| --- | --- |
| Linux amd64 server | `0ca3b75a19a4dd1e70185a5b1f3437b4ae49140484f6995b5d146a7a059149c9` |
| Darwin arm64 collector | `77c65fccc2f7b9efe45be05ea20ac0c798bd98da6a319a2c1fc15c249ec3f2e8` |

Required release evidence:

- Required CI and E2E checks pass on the exact client and server heads.
- The real PostgreSQL/S3 tests pass for mapped parent, child, and companion uploads, policy changes, pending replay, active authorized raw repair, overflow revocation, and same-device recovery receipts.
- The same cases pass against the actual capability-one route without a test override.
- The restored schema 013 upgrade tests pass on the final server candidate.

The schema upgrade rehearsal at `3ad289a` passed all three cases without skips. It used `pg_dump` and `pg_restore` before applying migration 014. It preserved old rows and hashes, verified failed-migration rollback, retained S3 bytes, and verified that the old server refused schema 014 without writes.

The exact release candidate passed 25 focused cases with no skips against real PostgreSQL and MinIO through the production capability-one route. The affected ingest race suite passed 261 cases with two intentional skips: private corpus opt-in and the capability-zero-only hold case. Independent review cleared both the companion and native-main identity gate closures and their lock ordering.

The preceding `eefe90b` candidate passed the nine-package private race suite with 1,374 passes and 17 intentional skips. That result is prior evidence. The final native-main gate change received the focused and affected-ingest checks above. Artifact source `a41dc8e` and the final dependency composition have identical implementation files; their differences are reviewed documentation and migration test additions.

## Current installations

| Installation | Observed version | Process |
| --- | --- | --- |
| Production server | `8023e172ecbd` | `flopwire-production.service` |
| Local Mac | `8023e172ecbd` | `com.flopwire.agent` |
| MacBook M5P | `cutover-661a48b` | `com.flopwire.agent` |

The two Mac collectors have different verified device identities. Both use `~/.local/bin/flopwire`, `~/Library/LaunchAgents/com.flopwire.agent.plist`, `~/Library/Application Support/flopwire/config.json`, and `~/Library/Caches/flopwire/index.db`. Their launch arguments are `agent run --workers 2 --opencode-db -`. Neither installation has a config or index path override.

The fresh read-only production check found 13 ledger entries ending at `013_bus_retention_upgrade.sql`. The authenticated, pinned capability request returned HTTP 404 with a non-JSON body. Production is Linux x86-64; both Macs are arm64. Recheck these facts immediately before rollout. The rehearsal is qualification evidence, not proof that production has been upgraded.

## Proposed rollout sequence

1. Record the exact release commit and binary SHA256 values.
2. Obtain owner approval for the server and both Mac upgrades.
3. Stop both Mac collectors with `launchctl bootout gui/$(id -u)/com.flopwire.agent` on each Mac.
4. Preserve each entire index database with its WAL and SHM files. Preserve `bus.db`, configuration, device credentials, recovery receipts, and the redaction journal with its previous file and key. Do not reset device identity.
5. Back up the production PostgreSQL database and retain the archive objects.
6. Upgrade the server with the qualified binary and migration 014.
7. Verify server health, the migration ledger, authenticated policy capability one, and unchanged serial concurrency one.
8. Install the qualified Mac binary at `~/.local/bin/flopwire` on each Mac.
9. Verify `flopwire version` on each Mac.
10. Start each collector with `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flopwire.agent.plist`.
11. Verify `flopwire agent status` on each Mac. Check Cowork discovery, current mapping readiness, historical uncertainty, and policy registration errors.
12. Check known source, status, and policy metadata on each Mac. Compare the result with the isolated synthetic qualification cases. Any new Claude UI session or prompt requires separate owner approval.

## Reviewed command forms

Run these commands only after owner approval. Upload the verified artifacts to `/root/production/rollout-a41dc8e/` inside the production VM and `/tmp/flopwire-rollout-a41dc8e/` on each Mac before installation. Compare each artifact with the SHA256 value above.

On each Mac, stop the collector:

```sh
launchctl bootout gui/$(id -u)/com.flopwire.agent
```

Confirm the collector process has stopped. Copy both state directories:

```sh
set -e
rollout_backup=$(mktemp -d "$HOME/Library/Application Support/flopwire-rollout.XXXXXX")
ditto "$HOME/Library/Caches/flopwire" "$rollout_backup/cache"
ditto "$HOME/Library/Application Support/flopwire" "$rollout_backup/config"
cp -p "$HOME/.local/bin/flopwire" "$rollout_backup/flopwire-before"
cp -p "$HOME/Library/LaunchAgents/com.flopwire.agent.plist" "$rollout_backup/com.flopwire.agent.plist"
```

The cache copy includes the index, WAL/SHM files, and redaction journal with its key. The configuration copy includes bus state, credentials, and recovery receipts.

Inside the production VM, stop the service and dump the actual application database:

```sh
set -e
systemctl stop flopwire-production.service
rollout_backup=$(mktemp -d /root/production/rollout-backup.XXXXXX)
docker exec flopwire-postgres-1 sh -c 'exec pg_dump -Fc -U "$POSTGRES_USER" -d flopwire_production_20261004' > "$rollout_backup/database.dump"
test -s "$rollout_backup/database.dump"
docker exec -i flopwire-postgres-1 pg_restore --list < "$rollout_backup/database.dump" > "$rollout_backup/database.list"
cp -p /root/production/flopwire "$rollout_backup/flopwire-before"
```

The container's `POSTGRES_DB` names a different database. Use the explicit application database above. The `pg_restore --list` check verifies basic dump readability; it does not establish coordinated disaster recovery. Retain the named volumes `flopwire_postgres-data` and `flopwire_object-data`; do not remove or recreate them.

After backup and artifact verification, install and start the server:

```sh
set -e
install -m 0755 /root/production/rollout-a41dc8e/flopwire-linux-amd64 /root/production/flopwire
/root/production/flopwire version
systemctl start flopwire-production.service
systemctl is-active flopwire-production.service
```

Verify migration 014 and authenticated policy capability one before restarting either Mac. On each Mac:

```sh
set -e
install -m 0755 /tmp/flopwire-rollout-a41dc8e/flopwire-darwin-arm64 "$HOME/.local/bin/flopwire"
"$HOME/.local/bin/flopwire" version
launchctl bootstrap gui/$(id -u) "$HOME/Library/LaunchAgents/com.flopwire.agent.plist"
"$HOME/.local/bin/flopwire" agent status
```

## Rollback

This proposal uses stop-only rollback. No rollback binary is approved. Stop affected collectors and sync if a qualification assumption fails. Retain the upgraded state, including `cowork_history`, placements, protected source markers, generation proofs, receipts, and redaction data. A running rollback would require a separately qualified new build that enforces these gates and advertises capability zero.

Do not restart the old Mac binaries with sharing enabled. They do not enforce the new history and protected-source gates. Do not restore an old index or remove new tables to make an old binary run. The old server refuses schema 014; keep the qualified server or stop the service while resolving the issue.

## Coverage limits

Ongoing Local Code and Cowork collection is in scope. SSH and WSL collection is deferred. The known 11,722 archived records uploaded by the separate recovery device remain unreconciled in this phase. Same-device receipts provide restriction-only reconciliation for verified retained export uploads; they do not establish cross-device ownership or authorize new content.

The Local Code probe observed the existing normal CLI transcript location on the local Mac. The scoped Code and Cowork authorization paths were qualified with isolated synthetic fixtures. Both production Mac upgrades and their live collection checks remain pending owner approval.
