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
  A fresh full import at 4 GiB has not been measured.

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
