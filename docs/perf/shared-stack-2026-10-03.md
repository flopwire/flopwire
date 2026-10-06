# Shared-server capacity trial, 2026-10-03

Start this deployment at **4 GiB RAM**. The 2 GiB trial killed a PostgreSQL
process and failed 21 of 32 concurrent requests. Do not reduce it to 1 GiB.
This is a capacity result for one private corpus, not a general minimum.

The shared CLI and MCP paths can retrieve Claude, Codex, and Devin CLI history.
Broad grep still returns partial results at its 10-second deadline. Passing
the memory trial does not establish acceptable search latency or complete
results. CASS retirement requires the remaining cutover checks below.

## Workload and environment

- Ubuntu 24.04 Incus VM, four virtual CPUs, 200 GiB disk, no swap.
- Flopwire API, PostgreSQL 17.6, and MinIO in the same guest.
- Two collectors and the CLI load generator also ran inside the guest.
  A deployed setup would normally run collectors and agent clients on laptops.
- Native history from two laptops, plus normalized CASS recovery exports.
- 2,198,090 live indexed messages and approximately 11 GB of PostgreSQL data.
- Recovery exports contained 11,866 conversations and 103,915 messages.
  SQL counts matched these exports after ingestion. The global ingestion
  queue had no pending, failed, or quarantined sources at the settled checks.
- The initial full import ran with 8 GiB. The smaller guests were tested with
  the indexed corpus, searches, and forced large-source reprocessing.
  A subsequent fresh native import at 4 GiB is measured below. Recovery
  exports have not yet been added to that fresh deployment.

The tested binary combined the byte-bounded ingestion, shared search default,
JSON NUL handling, and CASS recovery changes from PRs
[126](https://github.com/flopwire/flopwire/pull/126),
[127](https://github.com/flopwire/flopwire/pull/127),
[130](https://github.com/flopwire/flopwire/pull/130), and
[132](https://github.com/flopwire/flopwire/pull/132).
The reboot trials also used the dependency restart change in
[133](https://github.com/flopwire/flopwire/pull/133).
These measurements precede integration with the repository identity changes
in PR 125. Repeat the acceptance checks on the final deployment build.

## Measurement

The monitor sampled `/proc` once per second. It summed proportional set size
(PSS) for the API, collectors, PostgreSQL, and MinIO at each sample. PSS avoids
counting shared PostgreSQL pages once for each backend. The reported stack
peak is the largest simultaneous sum, not the sum of separate service peaks.

Guest pressure used `MemTotal - MemAvailable`. This includes the OS, clients,
and other guest work. It does not treat all reclaimable file cache as required
memory. Kernel OOM counters and logs were checked separately. One-second
sampling can miss shorter peaks.

| Configured RAM | Guest MemTotal | Peak stack PSS | Peak guest nonavailable memory | Minimum MemAvailable | Kernel OOM kills |
| --- | ---: | ---: | ---: | ---: | ---: |
| 4 GiB | 3,838 MiB | 1,522 MiB | 2,027 MiB | 1,812 MiB | 0 |
| 2 GiB | 1,825 MiB | 1,198 MiB | 1,743 MiB | 82 MiB | 1 |

At the 4 GiB stack peak, PSS was API 23 MiB, PostgreSQL 1,255 MiB,
MinIO 155 MiB, and collectors 89 MiB. Rounding can change the sum by 1 MiB.
At the 2 GiB peak it was 221, 744, 153, and 81 MiB respectively.
The lower observed peak in the failed trial is not evidence of lower demand.

The API used `GOMEMLIMIT=768MiB` at 4 GiB and `384MiB` at 2 GiB.
These values configure the Go runtime; they do not limit PostgreSQL, MinIO,
or total guest memory.

## Search and reprocessing results

Each group issued four full-text searches and four literal grep queries,
with a five-hit limit and a ten-second timeout. The groups used one, four,
and sixteen workers. Every successful request in the 4 GiB trial reported
shared scope without an explicit `--server` flag.

| RAM | Workers | Requests | Command errors | Partial responses | Group wall time |
| --- | ---: | ---: | ---: | ---: | ---: |
| 4 GiB | 1 | 8 | 0 | 3 | 44.904 s |
| 4 GiB | 4 | 16 | 0 | 6 | 20.242 s |
| 4 GiB | 16 | 32 | 0 | 16 | 12.242 s |
| 2 GiB | 1 | 8 | 0 | 3 | 46.308 s |
| 2 GiB | 4 | 16 | 0 | 6 | 23.411 s |
| 2 GiB | 16 | 32 | 21 | 0 | 8.931 s |

The last row is a failure, not a faster result. The kernel killed PostgreSQL
during concurrent requests and large-source reprocessing. PostgreSQL recovered.
The live indexed count remained unchanged and the ingestion queue settled
without pending, failed, or quarantined sources. This is not a backup restore
or a complete integrity audit.

Both guests reprocessed the four largest selected native sources. Reprocessing
took 2.11 seconds at 4 GiB and 4.77 seconds at 2 GiB. Indexed message text
reached 16,777,216 bytes; 23 messages exceeded 1 MiB. This is a replay test,
not the peak of an initial bulk import.

CLI session listing and reading succeeded for all three harnesses. MCP
initialization described shared scope, and its search returned shared scope.
These checks used the CLI and MCP protocol. They did not launch each agent
product against its installed integration on a laptop.

Reboot testing found that PostgreSQL and MinIO did not restart automatically.
The Compose restart change fixed this in the lab. A subsequent VM reboot
started the dependencies, API, and collectors without manual intervention.

## Search diagnosis

An experimental change sorted candidate IDs before fetching message bodies.
It passed retrieval tests and reduced a synthetic query's sort memory, but
did not establish a latency improvement on the real corpus. It was removed
from the running lab server and is not part of the recommendation.

A diagnostic broad grep query took 35.09 seconds at PostgreSQL's default
4 MiB `work_mem`. Its bitmap heap scan took 34.04 seconds, with 66,206 lossy
heap blocks and 556,042 index rechecks that rejected a row. A query-local
32 MiB trial removed lossy heap blocks, but still took 32.29 seconds.
The two runs were sequential, not a controlled A/B benchmark. No global
PostgreSQL memory setting was changed. Raising query memory alone did not
solve the broad grep problem.

After restoring the baseline binary and 4 GiB allocation, another 56-request
run had no command errors. Its three groups returned 3, 6, and 15 partial
responses. All harness reads and the MCP scope check passed again.

## Cutover gates

1. Build the final combination of reviewed changes on current main.
2. Provision a private endpoint and enroll both laptops.
3. Reserve the permanent VM's memory in the host worker budget.
4. Run CLI and installed agent integration checks from both laptops.
5. Verify shared search, explicit local search, and visible server failures.
6. Audit CASS-only selection and overlaps before claiming complete coverage.
7. Measure a fresh full import at the chosen memory limit.
8. Test backup and restore of the complete database and raw object set.
9. Keep the CASS database snapshots and providers outside this migration scope.
10. Stop CASS watchers and remove integrations after those checks pass.

Use 4 GiB as the starting allocation for the measured workload. Recheck memory
and search latency after corpus growth, parser changes, and increased query
concurrency. The private raw measurements contain no material that should be
copied into public test fixtures or logs.

## Subsequent cutover verification

The integrated build combined current main, including PR 125, with PRs 126,
127, 130, 132, and 133. Its complete Go race suite passed in the remote VM with
PostgreSQL and MinIO integration checks enabled. The maintained
`make e2e-sync` test passed all ten scenarios, including server outage,
interrupted upload, raw retrieval, deletion, and the fixture backup restore.
Linux and macOS arm64 binaries were built in the VM.

The full-corpus backup used a LUKS2 encrypted destination. It completed and
verified in 354 seconds. Restore targeted a separate empty database and
object bucket, and completed in 869 seconds. The restore state was `complete`.
The original and restored databases agreed on these counts:

| Inventory | Original | Restored |
| --- | ---: | ---: |
| Live messages | 2,198,090 | 2,198,090 |
| Conversations | 30,327 | 30,327 |
| Sources | 37,456 | 37,456 |
| Chunks | 51,008 | 51,008 |

The restored server passed shared session listing and reading for Claude,
Codex, and Devin. Archived message records were retrieved for Claude and
Codex. Devin's archived session export was retrieved by source range;
`read --raw` does not support individual Devin rows without byte ranges.
A recovered CASS message retained its recovery label, original path, and
readable normalized raw record.

These are measurements of the earlier lab schema. They establish recovery of
that deployment, not an upgrade path into the changed pre-release schema.
The integrated build uses a separate empty database and bucket for its full
import. The fresh native import completed at 4 GiB with 2,093,664 live
messages and 25,590 sources. No source was pending, failed, or quarantined.

During that import, peak simultaneous stack PSS was 1,285 MiB: API 74 MiB,
PostgreSQL 528 MiB, MinIO 299 MiB, and collectors 383 MiB. Peak guest
nonavailable memory was 2,075 MiB, and minimum available memory was 1,764 MiB.
There was no swap use or OOM kill. The guest also ran brief verification
work during this measurement. This measures native bulk import; it does not
replace the combined native and recovered-history query trial above.

The fresh count was 511 below the earlier native count. All source paths
matched, and only one Codex fork differed. Every one of its 511 extra old
messages matched a message in the indexed parent by kind and content hash.
Every fresh child message matched an old child message. The earlier run
indexed a copied parent prefix in the child; the fresh run skipped that
prefix. This accounts for the difference without missing message content.
Parent archive availability during ingestion can affect this duplicate count.

Production must start with enrollment and uploads from the actual laptops.
Do not promote the copied corpus or its device credentials into production.
The copied sources use VM paths, and source-specific identity for records
without native IDs can cause duplicates when the original laptop paths upload.

An overlap audit found 144 selected CASS recovery conversations with readable
source files outside the ordinary agent roots. Of their 9,015 recovered
messages, 4,529 have text also present in the copied native sources. Matching
text does not prove matching record identity, metadata, or original ordering.
The recovery counts above must not be described as 11,866 wholly missing
conversations. At this stage, the overlap decision was pending. The subsequent
private rollout section records the accepted native-only selection; the
snapshots and lab records are preserved.

Setup verification also found an incorrect warning: an enrolled device with
no local index was told that MCP search would find nothing. PR
[135](https://github.com/flopwire/flopwire/pull/135), dependent on PR 127,
reports the default search scope and suppresses that warning for shared
search. All setup tests and retrieval scope selection passed with the race
detector in the VM. The plugin requirements now describe shared search.

At this stage, no laptop CASS watcher or integration had been retired. Private
endpoint enrollment, installed agent checks on both laptops, permanent host
memory reservation, and the selected recovery import were still outstanding.
The next section records their subsequent verification.

## Private rollout verification, 2026-10-04

The VM joined the laptops' Tailscale network. A persistent raw TCP Serve
forwarder exposes only the loopback Flopwire TLS endpoint to that tailnet.
The clients retain the Flopwire certificate pin. Both laptops reached the
endpoint and passed authenticated retrieval checks.

Production uses a separate database, object bucket, TLS identity, and enrolled
laptop devices. The copied VM corpus remains separate. The production API
uses the integrated build plus PR 135. Each laptop has the CLI, a persistent
launchd collector, and the Claude Code, Codex, and Devin CLI plugins installed.
The collectors keep local indexes for explicit `--local` queries. OpenCode
collection and plugins were not enabled as part of this migration.

Both laptops passed shared session listing, reading, and raw retrieval for
Claude, Codex, and Devin. Those read samples were native records, not CASS
recovery. A rare-term search reported shared scope. Explicit local session
listing reported local scope. MCP initialization described shared scope,
and its session tool reported shared scope. An unreachable endpoint in an
isolated enrolled configuration caused search to fail; it did not return
local results. Explicit local retrieval still worked with that configuration.
These checks validate installed configuration, CLI behavior, and the MCP
protocol. They do not constitute a model-driven end-to-end trial in each agent.
Codex's hook trust review remains a separate user action; it is not required
for CLI search.

The owner chose native records for the 144 overlapping recovery conversations,
with the overlapping CASS records retained in the archive. Production received
11,722 non-overlapping recovery conversation records and 94,900 messages.
All 143 outside-directory native transcripts were captured from their actual
laptop paths with the regular redaction and synchronization implementation.
No selected file was missing at capture. The temporary upload helper retried
a stalled source and completed; the regular collectors resumed afterward.
The helper is a migration artifact, not continuous discovery of these extra
roots. Future history written outside the normal roots needs a capture path
before claiming ongoing coverage there.

Pandora's existing `PANDORA_BUDGET_MIB` override reserves 5,120 MiB for the
4 GiB VM and host overhead, in addition to the scheduler's normal 1,536 MiB
host floor. With host MemTotal 128,412 MiB, the worker budget is 121,756 MiB.
The override persists through the SSH PAM environment and the worker engine
user-service drop-in. A fresh SSH session and the unit configuration both
reported the override. Recalculate it if the host or VM allocation changes.
No Pandora daemon upgrade or live laptop Pandora state change was performed.

## Bulk synchronization verification, 2026-10-05

By 07:47 EDT, both laptops' upload queues and the server indexing backlog were
zero for two consecutive checks. This establishes that the initial backlog
drained. It does not establish complete history coverage. New sessions can
create further upload and indexing work after that settled check.

CASS retirement still requires the final history-coverage audit, ongoing
discovery of any required extra roots, model-driven agent checks, and removal
of remaining CASS references. Future Claude Desktop and Cowork capture remains
an owner decision. The one-time outside-directory capture does not provide
ongoing discovery there.

The read-only CASS snapshots and encrypted lab backup remain available. The
full-corpus restore above used the earlier lab schema and corpus. It does not
verify restoration of the current production deployment. The October 6
production backup and database migration rehearsal are recorded below.
Restore the complete database and raw object set from an independently
retrieved backup before treating production recovery as proven.

## Operational follow-up

Production currently occupies the VM originally provisioned for capacity
testing on the shared Pandora host. Its name does not weaken the isolation,
but future destructive trials and restore tests must use a separate staging
VM. The persistent `flopwire-staging-20261006` VM in the `flopwire` project
now provides 4 GiB RAM, four virtual CPUs, and 100 GiB disk for separate staging
work. Record the production role, reserved memory, service configuration, and
recovery procedure before further capacity experiments.

The private endpoint uses self-signed TLS with certificate pins retained by
the enrolled clients. Tailscale provides private network reachability; client
pin verification authenticates the Flopwire TLS identity. Keep pin verification
enabled. Document certificate rotation and client recovery. A public certificate
is an operational option, not a prerequisite for this private pinned endpoint.

The migration's administrator password and backup decryption key are stored
as plaintext files on the owner's Mac. The files have mode `0600`, their
directory has mode `0700`, and FileVault is enabled. These controls restrict
file access and protect the volume while locked. They do not protect the
secrets from a process with access as the owner while the Mac is unlocked.

The owner chose to retain the current secret files while preparing the recovery
plan. Establish an independent off-Mac recovery-key escrow before considering
removal of the working key file. Keep encrypted production backups off the
Pandora host, and keep key escrow separate from those backups. Verify recovery
using the escrowed key. The existing local secret files and lab restore do not
establish those operational recovery safeguards. Follow the
[production recovery runbook](../operations/recovery.md) before changing the
secret storage or retiring the migration archives.

## Production backup and migration rehearsal, 2026-10-06

The coordinated encrypted production backup completed between 14:32:30 and
14:38:16 UTC. Its state reports `complete`. Backup verification reports
`verified: true` for 66,213 objects. This updates the October 5 backup status;
it does not establish independent off-host retrieval or key recovery.

The isolated production database snapshot rehearsal completed at 15:58 UTC.
The actual startup migrator applied migrations 011, 012, and 013 together.
The original migration ledger entries remained byte-identical, and the
recorded before and after count checks agreed. The older binary correctly
refused the upgraded ledger. These outcomes complete the rehearsal described
as running in [PR 168](https://github.com/flopwire/flopwire/pull/168).

The candidate then failed to connect to its deliberately unavailable staging
object endpoint. This rehearsal establishes the database migration behavior;
it does not qualify raw-object restoration or a recovered server's readiness,
retrieval, authentication, or certificate checks. The production readiness
record after the upgrade reports the database, object store, and search ready.
Production readiness is separate from recovery qualification.

The persistent staging VM is available for synthetic qualification. Synthetic
fixtures can verify deployment, backup, restore, object consistency, and
access checks there. Such checks cannot establish production corpus coverage,
independent backup retrieval, or recovery with an escrowed production key.
No synthetic staging qualification result is recorded here.

On October 6, the owner chose to defer off-host backup work and finish
collection and staging work first. This decision leaves off-host retrieval,
independent key recovery, and full production recovery qualification pending.
Retain the existing working key, CASS snapshots, and migration archives.

Full production recovery still requires an encrypted off-host backup copy,
retrieval without the original host, independent off-Mac key escrow and
recovery, and a complete database and raw-object restore into isolated staging
targets. Complete the acceptance checks in the
[production recovery runbook](../operations/recovery.md). The owner must select
the off-host destination and escrow mechanism and set recovery targets,
backup frequency, retention, and cutover policy before executing that procedure.
Keep the working key, CASS snapshots, and migration archives until the
replacement recovery path passes verification and retention decisions are
recorded.
