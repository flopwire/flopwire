# Embedded search adapter POC — 2026-10-14

Status: integration experiment complete; final race verification passed. This record does not qualify a production search engine.

This follows [the broad-regex experiment](https://github.com/flopwire/flopwire/pull/268) and tests the integration cost that its isolated index benchmark omitted. The intended agent contract is a stable initial search snapshot, current authorization on each continuation, and a fresh search for newer messages.

The experiment uses main `883a467f427be88f05a58c2751a08f1cb30e6d86`, PostgreSQL 17.6, and embedded Zoekt `v0.0.0-20261014141728-4abc8196181a`. Production source and dependencies remain unchanged. All inputs are synthetic.

## Questions under test

- Can every snapshot message be covered by indexed candidates or an explicit fallback?
- Do the production Go matcher and authenticated read endpoint remain authoritative?
- Can bounded continuation handle retries, revocation, redaction, expiry, and concurrent requests without declaring a false negative?
- What do first-page and full traversal cost after authorization and original-body verification?

## Scope

The candidate accelerator handles a conservative finite ASCII literal language. Unsupported patterns scan the fixed snapshot. Short, NUL-containing, non-ASCII, oversized, and high-diversity bodies enter an explicit fallback path. Each generation owns fixed shard handles.

The continuation experiment supports a limited query/filter subset. It does not establish durable distributed cursors, crash recovery, cross-page deduplication, or capacity for a hosted team of 30 people. These remain production design gates.

## Correctness findings

The POC runs real migrations, API authentication, reader authorization, and message reads against PostgreSQL 17.6. Synthetic tests exercise credential revocation, upload-only rejection, hidden/deleted conversations, superseded/off-path messages, version/body changes, and the actual redaction endpoint. Redaction fixtures have no archived source; archive rewriting is outside this experiment.

The index has explicit coverage for every document. Unsafe patterns use the complete generation; document exclusions use exact Go verification. Candidate cancellation, skipped results, crashes, and truncation fail the search. Generated finite-pattern cases compare exact results with a Go regexp oracle.

Independent review identified two defects before final validation:

1. An index and separately supplied snapshot could disagree while a scan claimed absence. The adapter now hashes the complete document, including its address, version, body, and metadata, and validates the full generation before querying.
2. The page ignored the requested hit limit. The adapter now caps verification rows by that limit as well as the work budget. Pages can consequently contain fewer hits than the requested limit.

Continuation tests cover timestamp ties, null timestamps, later ingestion, immutable copies, same-sequence concurrency, cancellation before commit, expiry, oversized-message blocking, and terminal replay. Replays cache IDs and reauthorize their current bodies. An empty terminal page is never sufficient evidence for absence. Original nonmatches need no current-body read because changed versions cannot enter the frozen result set.

## Cost and overload

The non-race run used 1,000 synthetic messages of about 2 KiB each, with distinct bodies and tied timestamps. Index size was 4,094,752 bytes. Seed creation took 1,807 ms, snapshot export 53 ms, and index construction 464 ms; these setup costs are excluded below. First page includes generation validation/copying, candidate query, authorization, and current-body reads. Pages verify at most 50 candidates.

| Pattern | Verified hits | First page | Full traversal | Current-body reads |
| --- | ---: | ---: | ---: | ---: |
| Rare literal | 1 | 72 ms | 72 ms | 1 |
| Broad literal | 200 | 918 ms | 3,300 ms | 200 |
| Broad alternation | 200 | 795 ms | 2,698 ms | 200 |
| Literal-free, matches every message | 1,000 | 695 ms | 10,666 ms | 1,000 |
| Literal-free, no matches | 0 | 61 ms | 343 ms | 0 |

Each completed traversal matched the Go oracle's complete UUID set. The zero-hit row's first page is incomplete: only the final page can establish absence. New-scan startup alone cost 34–84 ms. The remaining first-page time is mainly the sequential authoritative read path for matching messages; these observations do not isolate every component.

These are single-run observations on a heavily loaded developer machine, not percentiles, capacity targets, or a controlled speed comparison with PostgreSQL. The recorded host load averages were 244/151/103 on a 10-core, 16 GiB Mac running macOS 26.2 and Go 1.26.6. PostgreSQL ran in local Docker. No clean-host repeat or representative 30-person corpus was measured.

The final simultaneous rare-query smoke attempted 30 searches: 20 succeeded and 10 received HTTP 429. Successful requests alone had observed p50 396 ms and p95 487 ms. These percentiles exclude failures. The API deliberately allows 16 retrieval requests at once and rejects overflow; it does not queue it. An earlier exploratory run rejected 14 of 30 and is also retained. A passing outcome-accounting test does not mean all requests succeeded.

The first full race run exceeded the timing harness's one-minute scan TTL during all-matching traversal. It failed explicitly with `scan expired`; all other tests passed and no race was reported. The timing harness now uses five minutes, while dedicated expiry tests retain short deadlines. Race-instrumented timings are excluded from the table.

## Proof table

| Claim | Impact if false | Evidence and gaps | Status | Next action |
| --- | --- | --- | --- | --- |
| Every document is indexed or scanned | False negative | Explicit coverage plus omission/oracle tests; conservative pattern subset only | POC-confirmed | Preserve manifest validation when expanding supported patterns |
| Index and snapshot describe the same generation | False negative or unstable history | Full-document digest regression tests; no durable publication protocol | POC-confirmed in process | Publish immutable generations atomically with recovery tests |
| Returned bodies pass current authority | Revoked/redacted text escapes | Real API/PG mutation tests; authorization is checked at request/read time | POC-confirmed for current shared-server policy | Batch current checks within server-owned boundaries |
| Retry cannot silently skip work | Missing results | Mutex, staged progress, ID replay and race tests; no process crash coverage | POC-confirmed in process | Design durable sequence compare-and-swap and expiry |
| 30 simultaneous searches succeed | Hosted agents receive errors | Exploratory run rejected 14 of 30 at HTTP limiter | Contradicted for naive HTTP adapter | Bounded scheduling, overload response, batched reads |
| First useful page stays below 1 second at team scale | Slow agent workflow | Small synthetic measurements only; startup and HTTP transfer unbounded by page work caps | Unresolved | Representative corpus and mixed concurrent workload |
| Global duplicate collapse survives redaction | Missing permitted copy | Prior experiment showed representative invalidation problem; current POC deduplicates within each page only | Unresolved | Decide message-address versus global text-group semantics |

## Production design implications

Keep the embedded candidate-index direction under evaluation. These tests do not justify adding a separate search service. They do justify changing the integration design before building it:

- Share immutable generations across scans. This POC hashes and deep-copies the whole generation for every new scan; startup and memory therefore grow with corpus size even for one rare hit.
- Move authorized body verification into bounded server-side batches. The test's HTTP read per hit preserves the existing authority boundary but adds request/transaction overhead and reaches the retrieval limiter under concurrency.
- Give an overloaded or unfinished search an explicit retry/continuation response. Do not translate cancellation, rejection, missing coverage, or oversized bodies into a zero-hit success.
- Bound generation retention, scan state, index enumeration, wire bytes, and deadlines. The current candidate/byte work budget does not cover those costs. Original snapshot plaintext remains in memory until released even after a redaction suppresses its results.
- Define publication, freshness lag, deletion propagation, recovery, and cursor binding before integration. The current binding is a trusted caller-supplied value, not an exposed security mechanism. SnapshotPG is privileged server code; callers must authenticate before export.

The current server grants read-scoped credentials access to its shared unhidden corpus. This POC does not establish hosted tenant isolation or future row-level authorization. Session and repository filters fail explicitly; supported filters are agent, non-injected kinds, and message-time bounds.

## Evidence

[Source and evidence archive](embedded-search-adapter-poc-2026-10-14.tar.gz): 37,152 bytes, 20 files. SHA256: `426b362245c5727396230bac94ca5fe24bb2227c73da3f9fc0adab32addbd766`.

The archive includes a separate Go module, reproduction instructions, database scripts, final source hashes, independent review, environment data, both final runs, and the earlier overload/expiry failures. It excludes databases, generated indexes, binaries, and private traces. Every archived file was checked against its source bytes before cleanup.

Final local validation:

- Full `go test -race -count=1 -v ./...` with the isolated PostgreSQL URL: passed in 172.248 seconds; 21 top-level tests, 42 passing entries including subtests, no skipped tests or race reports.
- Non-race `go test -count=1 -v -run '^TestPostgresAdapterTimingSmoke$' ./...`: passed in 22.529 seconds, including exact UUID parity and explicit overload accounting.
- The instrumented concurrency run happened to accept all 30 scans, but successful-only p95 was 35.2 seconds. Instrumentation changes timing and request overlap; that result does not override the non-race overload evidence.
- Independent Sol review plus focused regression verification found no remaining material blocker within POC scope. The final injected-kind rejection and timing-harness TTL adjustment were covered by the full race run.

The timing run preceded the final explicit injected-kind rejection; its empty-filter workload is unaffected. Production files and dependencies remain unchanged, so this experiment does not claim a production web build or deployment validation. GitHub CI is separate from the archived local results.

The temporary Docker container was stopped and removed. The `.poc-stress-test` directory, including its database files, was removed after archive verification. This research worktree remains while its PR is unmerged.
