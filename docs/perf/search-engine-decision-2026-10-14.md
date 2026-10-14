# Search engine decision experiment — 2026-10-14

Recommendation: keep PostgreSQL authoritative and pursue embedded candidate
acceleration behind a flag, alongside bounded continuation. This experiment
justifies that next implementation step; it does not qualify the current POC for
production or a 30-person hosted deployment. A separate network service is not
justified by this evidence.

This follows [#268](https://github.com/flopwire/flopwire/pull/268) and
[#269](https://github.com/flopwire/flopwire/pull/269). The common-path comparison
isolates a material PostgreSQL candidate bottleneck. The freshness experiment
also exposes maintenance and publication costs that must be fixed before rollout.
Production code and dependencies remain unchanged.

## Accepted contract

A search fixes its initial history. Later revocation, redaction, hiding, and deletion can suppress results. A new search includes newly committed messages. Distinct message addresses remain separate even with identical text. Retries reauthorize the same page. A server restart or deployment may expire cursors explicitly in v1.

## Decision gates

Measure the real PostgreSQL schema and embedded Zoekt against the same synthetic corpus, original Go matcher, ordering, scopes, and batched current-body verification. Include candidate startup in first-page cost. Share immutable generations; do not hide per-search corpus copies outside the timer.

The aspirational gate is p95 first useful page below one second for common indexable queries at a declared workload. It is not a promise for arbitrary regex completion. Report overload and incomplete coverage separately from successful results. Test mixed queries, large tool outputs, exact UUID parity, and freshness during out-of-order commits.

Keep Postgres if it meets the measured workload. Advance embedded search only if candidate enumeration remains a material bottleneck and the benefit warrants generation management. Neither a synthetic burst nor an engine microbenchmark qualifies a hosted team of 30 people.

## Engine evidence

The real migrated PostgreSQL 17.6 schema contains 50,000 deterministic synthetic
messages totaling 134,144,076 text bytes. One hundred messages are approximately
270 KiB. Every message has a distinct UUID and unique text. The collision-heavy
hash corpus reproduces broad trigram rechecks; it is not a natural transcript
sample. Scope zero contains every `heron` match and every large tool body.

Both candidate engines use the same initial version/SHA/rank manifest, current
visibility query, frozen ordering, 128-row body hydration and original Go
collector. Each first page returns twenty distinct addresses or completes the
search. Index construction clones/hashes the generation once; query startup does
not clone the entire corpus. The runner retains matching IDs after invoking the
collector, rather than rendering a complete HTTP response.

Warm total first-page call times, milliseconds; twenty interleaved repeats per
cell. p95 is descriptive nearest-rank, with no confidence or capacity claim.

| Unscoped query | Actual Store.Grep p50 / p95 | Common PostgreSQL p50 / p95 | Common embedded p50 / p95 |
| --- | ---: | ---: | ---: |
| Rare old `ancientneedle` | 47.1 / 65.1 | 6.3 / 8.8 | 3.7 / 5.2 |
| Collision-heavy `heron` | 1,862 / 2,212 | 1,627 / 2,182 | 84.5 / 115.6 |
| Broad `timeout` or `failure` | 807 / 894 | 795 / 884 | 105 / 133 |

All three routes returned the same first twenty addresses in every sample in
this table. Full traversal matched the ordered oracle for both common engines
on all five unscoped patterns. Median candidate time explains the selective gain:
`heron` fell from 1,528 to 8.3 ms, and the alternation from 722 to 30.0 ms.
Both common routes hydrated the same bytes and ran the same exact verifier.

Actual Store.Grep remains a separate comparator. It uses a 256-row cursor batch,
resolves more result metadata, and counts additional results after filling the
page. `Exact=false` can mean a bounded footer count, while the first page is
correct. Its whole-call ratio to the common embedded path is therefore not a
pure index speedup. Current production also refuses unscoped literal-free regex;
the common runner's bounded fallback is an experimental extension.

For `[A-Za-z]{30}`, completed scans on both common routes examine all 134 MB. The PostgreSQL median
was 6.41 seconds; the embedded-path median was 6.07 seconds, with one of twenty
embedded attempts hitting its 30-second deadline. That attempt returned explicit
incompleteness after checking 43,392 messages, not a false empty result. Host
contention rose sharply during these scans. They establish neither a latency
advantage nor a capacity target for exhaustive regex.

The original run exhausted its ten-minute measurement budget during scoped
`heron` traversal. Its failed log contains 311 failures, including the genuine
per-call timeout and subsequent already-canceled calls. Those canceled calls are
not latency samples or engine failures. The original harness is archived; the
follow-up stops at global deadline expiry and labels the interrupted stage.
Scoped tests and the mixed burst were separated into a new run.

The scoped follow-up passed in 380.64 seconds including setup. All ten full
engine/pattern traversals matched the ordered oracle, and all 75 warm calls
(five repeats × five patterns × three routes) returned the expected page. Five
samples are reported as descriptive medians only. For scoped broad alternation,
common PostgreSQL took 414 ms and embedded 423 ms; for scoped `heron`, 324 ms and
175 ms. Keep scoped PostgreSQL available: global embedded enumeration is not a
universal improvement.

A separate thirty-call mixed burst per engine used eight active slots and a
64-call waiting limit, with both scopes and all five patterns. All sixty calls
matched their expected page and completed within the per-call deadline.
Queue-inclusive median / descriptive p95 were 12.30 / 24.66 seconds for common
PostgreSQL and 3.27 / 19.47 seconds for embedded. Median queue wait was 9.46 and
2.86 seconds respectively. The expensive scans dominate tail latency. This
single sequential pair of bursts on a shared host does not establish a capacity
ratio or model thirty people. It does show why fairness and scan budgets remain
necessary despite faster candidates.

Initial setup took 161.8 seconds for fixture generation/load/analyze and 35.1
seconds for embedded construction. PostgreSQL's messages relation including
indexes/TOAST occupied 789,528,576 bytes. These are separate setup costs, excluded
from warm query timings. The embedded index is additional storage, not a database
replacement; a deployment storage/RSS/rebuild budget remains unqualified.

Environment: macOS 26.2, Go 1.26.6, ten logical CPUs, 16 GiB host RAM, Docker
PostgreSQL 17.6 with about 7.75 GiB available to its VM. Zoekt is pinned to
`v0.0.0-20261014141728-4abc8196181a`. Only one of this experiment's heavy jobs ran
at a time, but unrelated host jobs were not controlled. Recorded load rose from
about 12 to over 60, and system swap was in use. Cold cache, HTTP/authentication,
concurrent ingestion and multi-instance routing are outside the engine timings.

Candidate results still materialize before scope/cap enforcement in the embedded
adapter. A downstream candidate cap does not bound engine CPU or memory. Thus
this POC supports a gated acceleration trial, not adoption of its current pager.
A 250k engine run was deferred because it would add another uncontrolled scale
sample; representative capacity qualification remains a separate rollout gate.

## Freshness design and counterexamples

A full metadata sweep avoids cloning bodies but still scales with history. At
250,000 covered rows, the first prototype allocated 127,615,624 bytes and took
822.8 ms to capture a new search, despite hydrating zero bodies. At 50,000 rows,
it allocated 25,357,016 bytes and took 122.4 ms. Reject that capture algorithm.

The replacement ledger pins a shared immutable generation and reads committed,
uncovered events in one short repeatable-read snapshot. A per-message locked
revision orders changes to that address. Publication acknowledges only the exact
events it observed, so a transaction that allocates an earlier event number but
commits later remains pending. Tests reproduce the failure of a bare global
sequence watermark and verify the replacement across publication/commit races.

Publication installs the cache, commits the generation pointer and exact event
acknowledgments, marks the generation current, and retires old generations under
one mutex. A reviewer found the original gap between commit and current-pointer
assignment; a deterministic barrier regression now covers it. An ambiguous
commit fails closed and requires restart. Refcounts, expiry, capture bounds,
visibility/deletion overlays, redaction and credential revocation are tested.

The replacement remains a fixture. Production mutation integration is absent;
its document Version is fixed at one and ledger revisions carry ordering. It
retains historical event bodies and copies the whole base map on publication.
Capture is serialized while it reads the pending tail. These are production
gates, even if a small fresh tail is cheap. The partial pending index can retain
physical dead entries until cleanup; its indexed plan alone does not prove
history-independent work.

At 250,000 covered events plus ten pending events (260 body bytes), the ledger
cost test found the following. Autovacuum was disabled to expose acknowledged
entries; this is a maintenance stress case, not the default production setting.

| Operation | Elapsed | Go allocations / database work |
| --- | ---: | --- |
| Initial publication, 250k × 128-byte bodies | 6.081 s | 376,203,824 bytes allocated |
| First pending-index EXPLAIN ANALYZE before vacuum | 1.770 s | 7,210 shared hits + 1,302 reads |
| Capture after that first scan, before vacuum | 37.24 ms | 12,976 bytes allocated |
| Pending-index EXPLAIN ANALYZE after vacuum | 0.030 ms | 2 shared hits |
| Capture after vacuum | 3.04 ms | 12,992 bytes allocated |
| Republish base with ten changed entries | 86.61 ms | 25,201,928 bytes allocated |

The first EXPLAIN executes and warms the pending scan, so the 37 ms capture is
not its first-touch cost. Physical pending-index work is history-sensitive before
cleanup. Do not claim that a partial index alone makes startup proportional only
to fresh changes. Frequent full-base copies are also unresolved. These measured
limits block production qualification of the current ledger implementation.

## Proof table

| Claim | Impact if false | Strongest evidence | Missing or contradictory evidence | Status | Next action |
| --- | --- | --- | --- | --- | --- |
| Embedded candidates remove a material broad-literal bottleneck | Dependency cost without useful gain | Same verifier, twenty repeats, phase timings and full UUID oracle | Synthetic 50k corpus; shared host | POC-confirmed at fixture scale | Gated representative-corpus trial |
| Every new search can capture only a small fresh tail cheaply | Startup regresses as history grows | 13 KB capture allocations for ten pending rows | First pre-vacuum lookup took 1.77 s | contradicted for current physical design | Bound pending storage/retirement and test sustained churn |
| Late commits survive publication | Silent missing messages | Real PostgreSQL delayed-commit and exact-ack race tests | Production mutation paths not integrated | POC-confirmed in fixture | Audit and test every atomic mutation path |
| Snapshot state can safely coexist with current authority | Redacted/revoked evidence leaks | Real API redaction/revocation tests and exact-version checks | Engine/ledger are separate POCs; response consistency point remains | unresolved end to end | Integrate batch authority and replay tests |
| Candidate caps bound all work | CPU/memory exhaustion | Downstream cap and admission tests | Embedded enumeration materializes all matches first | contradicted | Bound work inside engine/scan steps |
| Current implementation meets hosted-team latency | Misleading rollout promise | No qualifying capacity run | 50k fixture, busy host, exhaustive timeout, unknown production resource budget | unresolved | Declared-hardware capacity trial before rollout |

## Validation and retained evidence

The final combined correctness run passed under Go race detection in 35.270
seconds. It covers candidate/oracle parity, scope and escaping, timestamp/body
mutation, late arrivals, generation mismatch, admission/cancellation, current
redaction/revocation, exact event acknowledgment, expiry and publication races.
The 250k ledger-cost test passed in 15.961 seconds; passing means its assertions
held, not that all measured costs were acceptable.

Sol subagents independently reviewed the engine comparison and publication
protocol. Their review added the actual Store.Grep baseline and caught the
cache retirement race. The earlier read-only Claude consultation recommended
isolating PostgreSQL and batching costs first; its suggested bare sequence
watermark was rejected by the delayed-commit counterexample.

The accompanying [evidence archive](search-engine-decision-2026-10-14.tar.gz) contains the pinned standalone module,
synthetic generators, original and corrected harnesses, raw results, plans,
environment records, summary script and review notes. No user traces, generated
corpus, index files, binaries or database data are included. Extract it under
`.poc-stress-test/` in the repository and follow its README. The experiment changes
no production code or module file.

## Production sequence

1. Add the engine-independent response and cursor contract behind a flag. Bind
   cursors to normalized server, principal, process instance, query, initial scope,
   generation and continuation sequence. Distinct message addresses appear once
   during forward traversal. Replays reauthorize the same page. Report progress,
   completion, overload and explicit restart separately. Do not infer scan-wide
   absence from an empty terminal page.
2. Add an atomic search-change ledger and audit every mutation path. Cover ingest,
   edits without version bumps, superseding, branch/default-path changes,
   conversation visibility/scope, redaction and deletion. Acknowledge exact
   captured event IDs; an allocated global sequence is not a commit watermark.
3. Publish shared immutable generations plus bounded committed overlays. Replace
   the POC's full-map publication copies, bound retained generations and bodies,
   and test backlog, vacuum and compaction under sustained ingestion. Recover a
   usable generation from authoritative storage after restart; old cursors may
   expire, but new-search coverage must recover explicitly. A new search
   must include committed uncovered changes or return explicit incomplete/retry
   status; it must not silently search only an older published index.
4. Integrate bounded continuation with current batch authority and exact-version
   body checks. Use primary reads initially and route in-memory cursors to their
   owning instance. Define the authorization consistency point for each response;
   a long-lived repeatable-read transaction cannot enforce later revocations.
   Preserve #267's receiver-authorized reads and invalidation behavior.
5. Connect candidate acceleration behind a flag only after work and memory bounds
   are enforced inside enumeration. Maintain explicit fallback coverage for every
   unsupported document/pattern. Measure first-page latency, queue wait, continuation
   progress, cancellation, ingest lag, storage, rebuild cost and peak memory together.
   Add per-principal admission limits so expensive searches cannot consume all
   interactive capacity; the POC admission controller has no fairness guarantee.

These are separate reviewable PRs. A hosted release requires representative
corpus/query distributions and sustained ingestion/concurrency on declared server
hardware. The prior shared-server record contains about 2.2 million messages;
50,000 synthetic messages are a diagnostic fixture, not that capacity test.

## Concurrent changes

The experiment uses production base `b4f4bb5e`, including merged
[#267](https://github.com/flopwire/flopwire/pull/267). Open
[#272](https://github.com/flopwire/flopwire/pull/272) preserves repository scope
without a full local index; [#273](https://github.com/flopwire/flopwire/pull/273)
binds delegated execution and waits for upload drain;
[#274](https://github.com/flopwire/flopwire/pull/274) isolates retained inboxes by
server/device authority. They do not change this hosted grep workload. Preserve
their scope and identity contracts during integration. An upload acknowledgment
is not proof that search publication covers the upload. PR state was checked on
2026-10-14 and may change before implementation.

## Primary references

The earlier [engine investigation](https://github.com/flopwire/flopwire/pull/268)
records the code-search design research and omission counterexamples.
PostgreSQL's [transaction isolation documentation](https://www.postgresql.org/docs/17/transaction-iso.html)
explains snapshot visibility and the special behavior of sequences. Its
[vacuum documentation](https://www.postgresql.org/docs/17/routine-vacuuming.html)
explains why acknowledged/deleted entries still require physical cleanup.
[Bitmap index combination](https://www.postgresql.org/docs/17/indexes-bitmap-scans.html)
can lose index ordering and require a separate sort; a small result limit does
not by itself bound candidate startup.

## Archive integrity and cleanup

The archive contains 44 files and is 110,353 bytes. SHA-256:
`f42de153fccd376e49372518775189c039acf596b2d2f00dbe42de2642a9c577`.
Every archived source/evidence file was verified against its SHA-256 manifest.
The isolated PostgreSQL container was stopped and removed, and temporary POC
files were removed after verification. The research worktrees remain while their
PRs are unmerged.
