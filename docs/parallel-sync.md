# Parallel sync and catch-up

Status: proposed. This document scopes implementation; it does not enable parallel uploads.

Implementation status as of October 5, 2026: PR #148 delivered serial `Retry-After` handling, pressure labels, and hook cooldown protection. PR #149 strengthened the E2E content/address oracle. These are partial prerequisites; deterministic interruption barriers, parallel cooldown epochs, shared-state ownership, and concurrent uploads remain unimplemented.

## Outcome

An enrolled Mac should upload separate sources concurrently, make recent history searchable first, and show whether missing search results could reflect incomplete synchronization. A large backfill must leave capacity for live sessions, search, and messaging.

Start with up to four upload workers per device. Keep one durable sync database, one spool, and the existing source identities, generation rules, redaction rules, and acknowledgment protocol. Each device path has at most one capture/upload operation in progress, including across file-identity changes.

Do not ship the experiment's independent stores or environment switch. Do not add a second bulk-import data path.

## Evidence and limits

On October 4, 2026, a private 4 GiB VM tested 64 sampled Claude transcripts, approximately 25 MB, against PostgreSQL and MinIO. Each case added an ignored JSON field to make its chunk hashes fresh. The final run kept server indexing active.

| Upload workers | Added request latency | Upload time |
| --- | --- | --- |
| 1 | 0 ms | 2.277 s |
| 4 | 0 ms | 0.755 s |
| 1 | 130 ms | 10.056 s |
| 4 | 130 ms | 2.666 s |

Four workers improved the latency case by 3.77 times. All 512 sources across two valid runs finished indexing with zero pending, failing, or quarantined sources. A separate connection measurement from the Mac had a 128.8 ms median over a reused connection.

This establishes throughput potential, not production correctness or peak memory. The benchmark used disjoint sources and independent client stores. It did not cover simultaneous shared-spool operations, restart recovery, large Codex records, or sustained mixed-agent backfill. The network delay was simulated inside the VM. Memory available after the final run was approximately 2.45 GiB; that is not a peak measurement.

## Existing constraints

- `devicesync.Scheduler` runs one upload operation and stores one running count. New notifications can arrive while that operation runs.
- `devicesync.Syncer` holds a global mutex through capture and network upload. Its scan buffer, retained compressed body, and held descriptors depend on serialization.
- `Spool.write` checks existence, checks available space, writes a shared `.tmp` name, then updates accounting. The checks and publication are not one atomic operation.
- Chunk cleanup checks durable references and then deletes the spool file. A concurrent capture can add a reference between those operations.
- `PendingSpecs` sorts recovered work by path. Initial local indexing sorts by oldest modification time. Neither is a recent-history upload policy.
- Server HTTP admission permits one flush per device. Each admitted flush holds a database connection and chunk locks while receiving its body.
- A partial acknowledgment can mean another request holds a shared chunk. The client currently stops after three no-progress responses. Parallelism can turn normal contention into this error.
- PR #148 distinguishes admission pressure from outages, honors `Retry-After`, and prevents hooks from bypassing cooldowns. Completion handling still assumes serialization: a concurrent success could clear newer global backoff if copied unchanged into a worker pool.

## Design

### 1. Server admission and capability negotiation

Add an authenticated `GET /v1/sync/capabilities` response with protocol version, maximum concurrent flushes per device, and supported request limits. The existing version-1 flush codec stays intact. Cache capabilities per normalized server and credential/configuration epoch. Recheck after reconnect and configuration changes.

Use one worker on an unsupported-endpoint/404 response and report that compatibility decision; a 404 cannot prove whether the server is old or a proxy route is wrong. Authentication, pin, network, and malformed-response failures must remain visible; they must not masquerade as an older server. If a flush returns the legacy `flush_in_progress` 429, clamp to one worker for that capability epoch as a defensive fallback. New admission errors should distinguish source contention, device capacity, and global capacity. Never infer concurrency from the binary version or deliberately provoke 429s to discover it.

Initially advertise one active flush per device. After qualification, enable four per device and eight globally for the two-Mac deployment. Clamp the global limit against configured PostgreSQL pool capacity so retrieval, parse workers, messaging, and maintenance retain headroom. Validate these settings on the 4 GiB VM. Advertise the effective limit, including a serial rollback setting.

Apply global/device admission before reading a large body or acquiring a pool connection. After decoding the bounded header, admit at most one request per authenticated device path, across generations and file identities. A different client process using the same device credential cannot overlap writes to that path. Release every reservation on success, rejection, cancellation, or disconnect. Bound and remove idle admission entries.

Keep existing chunk verification, ownership checks, manifest transactions, redaction locks, and durable parse scheduling. Shared chunks remain content addressed. Do not wait on conflicting chunk locks while holding other chunk locks. Return the existing partial/missing result when a concurrent writer owns a chunk, and make that condition a bounded, retryable contention outcome for the client.

### 2. One client coordinator, bounded worker contexts

The scheduler owns job transitions. Workers receive immutable work snapshots and return results; they do not independently change shared queue state. Represent each path as one of waiting, ready, running, or retrying. A running path may retain one coalesced newer notification. The next operation on that path starts only after completion of the current operation.

Keep a single `Store` and `Spool`. Keep whole-version capture serialized initially, separate from parallel upload turns. Do not introduce capture stepping as part of this rollout. Allocate worker-local upload/redaction buffers and retained compressed bodies. Keep descriptor ownership, refusals, durable source generations, and spool references under explicit shared owners. Run startup spool sweeping exactly once, before workers start; shut down workers before sweeping or closing descriptors.

Use short serialized sync-state write transactions. Never hold a SQLite transaction or database connection across network I/O. Preserve the existing local index writer and its coordination; adding workers must not multiply independent sync databases or SQLite writer connections. Centralize the existing held-descriptor limit rather than applying it separately to each worker.

Make spool publication and accounting atomic under the coordinator: reserve bytes once, write to a unique temporary file, publish once, reconcile a duplicate hash, and release the reservation on failure. Serialize reference insertion and reference-check/delete decisions through that owner. Protect a chunk while a request is reading it. A chunk can be deleted only when neither durable pending references nor active readers need it. Locks for this bookkeeping never span network waits.

Start with a 64 MiB shared budget for retained upload work, in addition to a maximum of four workers. Account for uncompressed buffers, compression input/output, retained batches, provisional tails, and record-redaction work, not just bytes sent. Reserve a complete bounded unit of work before allocating it; workers must not deadlock by holding partial reservations while waiting for more. Existing maximum-record and protocol limits continue to apply. If four large requests cannot fit, fewer workers run. The existing lazy export callback produces an opaque whole `[]byte`, so it cannot satisfy reserve-before-allocation by itself. Allow at most one export capture, measure actual Devin exports, and use a size-aware/streamed staged export when it cannot fit the work budget; preserve a coherent complete export and never truncate history to fit. Validate the budget against actual allocation and PSS measurements before making it a default.

### 3. Scheduling and fairness

Keep initial local indexing behavior separate from upload priority. Upload classes are interactive work, newly changed sources, and historical backfill. Hook-triggered work is interactive. Live-session discovery also promotes work without requiring hooks.

Sort historical work by most recent observed activity/modification time, with a stable path tie-breaker. Persist enough ordering metadata to recover the same policy after restart. Use a concrete initial dispatch ratio of four interactive turns, two changed-source turns, and one historical turn when all classes have eligible jobs; lend unused turns to other classes. Every eighth historical dispatch serves the longest-waiting historical source rather than the newest. Tune these values with starvation and latency tests. Coalesce notifications by path; use indexed queues rather than repeatedly scanning the full backlog.

Give a source one bounded upload request per scheduling turn, then requeue unfinished work. Keep capture's current whole-version semantics: shrinking capture to a scheduling window can make `idle` seal a historical file's tail at artificial EOF, changing chunk boundaries. Step `uploadGen` after a request and its durable local acknowledgment instead. Preserve generation and complete-record/redaction rules. Bound expensive captures separately. When live sessions are known, keep one upload slot available for interactive work (bulk limit three at a total limit of four); use all four for backfill when no live sessions are present. Hooks remain optional because live discovery also promotes work. A large in-flight request is not assumed to be instantly preemptible.

The initial request target remains 4 MiB compressed. Worker count is the minimum of the local cap, advertised server cap, current congestion allowance, and available work-memory budget. Begin conservatively and grow to four after successful acknowledgments. On explicit device/server pressure, reduce concurrency, respect `Retry-After`, and restore capacity gradually. Distinguish pressure, source contention, network outage, and permanent credential/pin rejection.

A cooldown or permanent stop has an epoch. Completions dispatched before that epoch cannot clear it. Hook `Flush` promotes a path but cannot bypass a pressure `Retry-After` deadline. A single designated probe reopens the gate. A permanent stop cancels/drains active work before re-pinning or installing worker configuration. Token rotation and re-pinning retain the existing authentication rules; stale in-flight results cannot install older credentials or pins. `reloginError` is permanent in the current client and must participate in the same gate.

### 4. Progress and searchable coverage

Extend `flopwire agent status` with discovered/indexed sources, sources waiting for capture, active uploads, captured unacknowledged bytes, acknowledged bytes, recent throughput, effective worker count, and blocking reason. Report an approximate upload ETA only after a useful sample; source count alone is insufficient for mixed file sizes. Label unknown byte totals rather than pretending they are zero.

Report server parsing separately: pending, failing, quarantined, and oldest pending age. Do not equate an upload acknowledgment with searchability. For exact catch-up verification, an additive acknowledgment field can identify the source's requested parse revision, and an authenticated device-scoped status endpoint can report whether that revision has been indexed. Responses and polling must be bounded and batched.

Expose this device's incomplete-sync state in CLI and MCP retrieval metadata and a short text note. Shared coverage may be unknown for an offline second device; say so rather than asserting completeness. Queue-empty plus parse-empty is a snapshot, not proof that discovery covered every intended history root. Search remains shared after enrollment and never silently falls back to local.

## Concrete state traces

### Shared chunk publication and cleanup

```text
Current code if global serialization is removed:
t0 A={needs:H}; B={needs:H}; spool={H:absent, used:0}
t1 A and B both observe H absent and both pass Reserve(size(H))
t2 A and B open H.tmp; one may truncate the other's in-progress write
t3 publication/accounting can fail or disagree with the file on disk

Proposed owner:
t0 owner={H:absent, reserved:0}; A={needs:H}; B={needs:H}
t1 A reserves H; owner={H:publishing, reserved:size(H)}
t2 B attaches its pending reference; B does not publish or reserve again
t3 A atomically publishes H; owner={H:present, refs:[A,B], readers:0}
t4 A upload acknowledges H; owner={H:present, refs:[B], readers:0}
t5 B starts reading; owner={H:present, refs:[B], readers:1}
t6 B acknowledges and releases reader; owner={H:present, refs:[], readers:0}
t7 owner removes H and decrements bytes once
```

Pending references and publication ordering must remain recoverable after a crash. Startup reconciliation handles abandoned temporary files; it must never discard a published chunk referenced by committed sync state.

### Notification, pressure, and restart

```text
t0 scheduler={A:running(snapshot1), B:running, epoch:4}
t1 A changes; scheduler={A:running(snapshot1,pending:snapshot2), B:running}
t2 B receives parse_backlog; gate={throttled, epoch:5, retry_after:5s}
t3 A succeeds from epoch4; its ack commits, but gate remains epoch5
t4 process stops before snapshot2 runs; durable watermark/ack state survives
t5 restart discovers A's newer bytes and pending generations; capabilities refresh
t6 probe succeeds after cooldown; A(snapshot2) resumes with the same device/path
```

The scheduler must recheck path policy and parsed upload bounds immediately before each new turn. A queued or running source cannot bypass a new deny rule merely because its earlier snapshot was allowed.

## PR boundaries and dependencies

| PR | Scope | Gate |
| --- | --- | --- |
| 0 | Content/address oracle delivered in #149; deterministic interruption barriers and independent fixture/provenance checks remain | Existing serial behavior; fail if the intended crash boundary is not reached |
| 1 | Serial pressure classification, `Retry-After`, and hook cooldown protection delivered in #148; parallel cooldown epochs and drain safety remain | A hook or stale success cannot undo newer pressure; permanent stop drains correctly |
| 2 | Serial one-request upload turns, recent-first order and fair queueing | Chunk boundaries unchanged; live append during backfill; restart order and starvation |
| 3 | Capability endpoint and bounded server/device/path admission, advertise one | Old-client compatibility; release on disconnect; retain pool headroom |
| 4 | Shared spool owner, worker-local upload state, export/descriptor budgets, still serial | Publication/read/delete races; existing recovery/redaction corpus; coherent exports |
| 5 | Negotiated bounded worker pool | Same-path running exclusion; shared-chunk contention recovery; pressure fallback; rotation; race tests |
| 6 | Upload versus indexing progress and retrieval coverage notes | Honest unknown states; bounded polling; CLI/MCP output contracts |

PRs 0 through 4 can land with serial behavior. PR 5 enables concurrency only after capability support and shared-state safety. Define optional parse-revision fields in the capability contract in PR 3 and implement their reporting in PR 6. This delivers pressure handling and recent-history availability before the riskier worker refactor. Ship small reviewable pieces; do not merge the private experiment branch as the implementation.

### Claude review and independent assessment

A read-only Claude consultation reviewed this proposal and the relevant code on October 4, 2026. The peer ran no tests and made no edits. Its conclusion was that the direction is sound but concurrency needs explicit prerequisites: pressure classification, same-path exclusion, shared-spool ownership, and retryable shared-chunk contention.

Accepted changes: deliver serial pressure handling and request-level fairness first; keep whole-version capture; spell out a hard running-path invariant, `Retry-After` behavior for hooks, drain-before-repin semantics, a legacy-429 fallback, and a concrete queue policy. Capture currently shortens `id.Size` for `upTo`, then seals idle tails; naive stepping would create artificial chunk boundaries. At the October 4 review, the client discarded `Retry-After`. Both findings were independently checked against the source; PR #148 subsequently fixed the serial retry behavior without implementing parallel cooldown epochs.

Caveats: whole-file capture is not free merely because it scans with a bounded buffer. It consumes CPU, accumulates manifest metadata, and opaque export callbacks allocate complete byte slices. Shared-store throughput and live latency during the largest captures remain release measurements; safe resumable capture would need a separate design if those gates fail. At review time, the consistency oracle verified native-ID/part multiplicity rather than full extracted content. PR #149 added semantic, text-hash, ordinal, and byte-address comparisons. Mandatory interruption barriers and independent fixture/provenance checks remain prerequisites. The current PostgreSQL pool uses retrieval/parse bounds plus 24 headroom connections; eight flush slots still need whole-stack qualification rather than an assumption of spare capacity.

Resolve the remaining cutover stack against current main before final integration. The retained-text byte bound in PR #126 and JSON preservation in #130 are merged and affect mixed-history validation. Recovery import in #132 and shared retrieval/setup PRs #127 and #135 remain integration dependencies. Keep those reviews distinct from the new concurrency work.

## VM validation and release gates

### Existing E2E coverage audit

`internal/e2e/sync_test.go:TestTwoDeviceSync` runs a real TLS server, PostgreSQL, MinIO, and two agent processes with separate homes, credentials, local indexes, and sync state. Its scenarios cover initial sync, live append and hook flush, cross-device retrieval, server outage/catch-up, client kill/restart, Claude rewrite, Devin deletion, admin deletion, companions/raw retrieval, backup/restore, and final consistency. GitHub's E2E workflow runs synthetic fixtures; real corpus sampling is opt-in and caps sampled individual files at 64 MiB.

This is a useful serial baseline, not sufficient coverage for enabling parallel sync. The initial-sync scenario waits for the optional corpus to drain before live-append scenarios. The kill scenario logs instead of failing when it misses the mid-flush window. PR #149 supplemented the native-ID/part multiplicity checks with `content_test.go` comparisons of message semantics, text hashes, ordinals, and byte addresses. Those expectations still use the transcript parsers; independent fixture expectations and source generation/provenance checks remain to be added. Raw-byte checks exist for selected sources, not every consistency comparison.

| Requirement | Present coverage | Required addition before rollout |
| --- | --- | --- |
| Content and addresses survive catch-up | #149 compares semantics, text hashes, native identity, ordinals, and byte addresses; selected raw-byte checks | Add independent explicit fixture expectations and source generation/provenance checks; independently verify redacted raw bytes |
| Live work during bulk sync | Live append after initial drain; scheduler unit test for hook priority | Keep backfill blocked/in progress, fill workers with large sources, append a live message with and without hooks, assert upload and search latency plus eventual older-source progress |
| Multiple workers share client state | Serial two-device agents; prototype uses independent stores | Same actual agent/store/spool at worker counts 1 and 4; sources sharing chunks; rewritten exports; controlled publication/read/delete races |
| Crash recovery at exact boundaries | Large-source kill/restart; component spool recovery | Explicit barriers after publication, after server commit before local ack, and during concurrent requests; hard-fail missed barriers; restart with the same durable state |
| Pressure and stale completions | #148 serial `Retry-After`, pressure labels, and hook cooldown protection; failure isolation and pin/rotation component tests | Delayed success after newer 429/503; changed pin/token while several requests run; parallel cooldown epochs and drain safety |
| Source ownership and server limits | Server test rejects a second same-device flush | Prove different paths overlap, same path cannot overlap across identities/generations/processes, device/global limits hold, and every slot releases on disconnect |
| Version compatibility and progress | No capability or parse-revision acknowledgment contract yet | Old/new client/server combinations; endpoint errors; upload ack with parsing deliberately paused; correct incomplete/unknown retrieval scope notes |
| Capacity on the intended deployment | Separate private VM performance experiments | Sustained mixed-agent two-device backfill, latency/loss, searches and messaging; assert concurrency actually exceeds one; collect client and whole-stack peaks |

Use deterministic barriers at component/integration level for spool races and stale completions. Use a test-controlled transport/proxy or equivalent explicit fault controls for process-level E2E interruption. Production builds must not expose an unauthenticated fault-injection API. Tests should prove the concurrency/boundary they exercise, rather than merely return a passing end state.

Keep existing serial E2E coverage. Add a short synthetic parallel suite for PR CI at workers 1 and 4, plus negotiated limits and old-server fallback cases. Run the larger mixed-agent/real-history and capacity matrix in the private VM for release qualification; exclude private histories from CI artifacts. The concurrency feature must not merge solely on the current serial suite or benchmark result.

The current E2E script builds ordinary agent/server binaries; package-level CI race tests do not instrument those subprocesses. Add a separate targeted E2E run with race-instrumented binaries. Measure production memory and throughput with ordinary binaries so race-detector overhead does not become a misleading capacity result.

### Acceptance and rollout

Run builds, sustained tests, and profiling in the private VM. Use a new database and bucket created by migrations, including seed rows; a schema-only clone is insufficient. Preserve original devices, paths, file identities, generations, and raw-message addresses when comparing actual migration results.

1. Run the existing sync conformance, recovery, redaction, and E2E suites with race detection where appropriate.
2. Compare one, two, and four workers on identical cold and warm mixed Claude/Codex/Devin corpora, with measured network latency, large records, rewritten exports, and companion files.
3. Interrupt between capture, spool publication, upload commit, and local acknowledgment. Restart and verify no missing content, duplicate live messages, or lost generations.
4. Exercise same-path competing clients, shared chunks in opposite order, partial acknowledgments, 429/503, network loss, token rotation, and changed policies.
5. Add interactive appends during large backfill. Measure capture-to-upload and capture-to-searchable latency, including when all workers are busy.
6. Measure whole-stack PSS, guest memory pressure, client retained bytes, PostgreSQL pool waits, and object-storage activity. Measure peaks under simultaneous two-device backfill and searches. Require zero OOM events on the 4 GiB VM; retain that VM size until evidence supports a change.
7. Target at least twice the serial throughput at approximately 130 ms latency, without weakening correctness, exceeding configured budgets, or materially degrading search. Treat 3.77 times as a fixture result, not an ETA guarantee.

Roll out the compatible server with serial limits first. Enable two workers on one Mac, verify catch-up and interactive behavior, then enable four if measurements pass. Enroll the second Mac in the same staged process. Rollback means lowering advertised/local concurrency to one and draining active turns; durable state stays usable and requires no re-enrollment or re-upload reset.

CASS retirement remains a separate coverage gate: both intended source sets discovered, uploads acknowledged, matching parse revisions searchable, recovered-only history present, selected overlaps resolved, and archive retained. A faster uploader alone does not satisfy that gate.

## Deferred work

Multi-source request batching could reduce per-file round trips further. Defer it until concurrent version-1 uploads are measured on the whole corpus. It adds per-source partial-ack framing, body/reference accounting, retry isolation, and fairness rules. Add it only if small-file round trips remain a material bottleneck after the worker pool. Do not require a permanent streaming socket: existing keep-alive/HTTP/2 connections already support reusable transport.

Implementation must settle the exact work-memory reservation formula in PR 4 and validate the initial weighted queue policy in PR 2 with allocation and starvation tests. These are tuning questions, not reasons to weaken source ordering, spool safety, or recovery guarantees.
