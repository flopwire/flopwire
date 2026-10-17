# Broad regex search investigation, 2026-10-14

The embedded positional index merits an integration POC. This experiment found
no consistent benefit from replacing the current PostgreSQL literal prefilter
with native regex. It also disproved adopting a code-search index with its
default settings: valid transcript content can be omitted.

The isolated experiments are complete. They do not qualify a production
implementation or a 30-person deployment. No product behavior or production
dependency changed.

## Objective

Return a small page of verified evidence quickly, with explicit search
completion and a way to continue unfinished work. The target is a hosted
team of at least 30 people. A proposed target is p95 below one second for
the first useful page of representative indexed queries. Corpus size,
concurrency, hardware and freshness bounds must be specified before this
becomes an acceptance criterion.

The user selected stable search snapshots: continuing a search examines the
same searchable history; a new search picks up later arrivals. Later access
revocations and redactions still apply. Initial eligible scope must also be
fixed, so later access grants do not change the meaning of completion.

Keep the tool independent of the underlying search engine. PostgreSQL and
raw objects remain the durable backup contract. An additional search index
must be rebuildable. A separate network service is an option to justify,
not a prerequisite.

## Baseline and concurrent work

- Baseline: `883a467f427be88f05a58c2751a08f1cb30e6d86`, including
  [#266](https://github.com/flopwire/flopwire/pull/266). That change permits
  bounded unindexed regex scans in explicit sessions; it does not solve
  broad unscoped candidate startup. Its author later reported the authorized
  shared-server and local CLI rollout completed at this commit; see the
  [rollout evidence](https://github.com/flopwire/flopwire/pull/266#issuecomment-6171914356).
- [#265](https://github.com/flopwire/flopwire/pull/265) preserves the
  distinction between incomplete searches without verified hits and
  complete empty results. Useful partial results remain successful.
- [#250](https://github.com/flopwire/flopwire/pull/250) already applies
  selective metadata before large text rechecks.
- [#249](https://github.com/flopwire/flopwire/pull/249) already preserves
  required literal spans in regex candidates.
- Open [#254](https://github.com/flopwire/flopwire/pull/254) changes bounded
  cited reads. Open [#253](https://github.com/flopwire/flopwire/pull/253)
  reviews agent interfaces, and [#243](https://github.com/flopwire/flopwire/pull/243)
  supplies a synthetic competitive pilot. These are adjacent work, not
  broad-regex engine replacements.
- The active excerpt-authority session, now drafting
  [#267](https://github.com/flopwire/flopwire/pull/267), confirmed no
  search-engine overlap.
  Its shared excerpts use server-authorized reads, with no fallback to local
  text on denied or missing reads. Any added index must preserve this boundary
  and respect redaction, withholding and current read authority.

The previous rollout receipt records 19.894872 seconds, approximately 17,757
candidate index entries, 16,669 rejected rechecks, and 54 matches. It records
0.78 seconds for a single commit literal. The original SQL, cache conditions
and repeated samples were not recovered, so these are diagnostic observations,
not a reproducible A/B benchmark.

The recovered acceptance query alternated a commit prefix, a branch path and
a pull-request URL fragment, without repository/time filters, with a one-second
timeout and twelve-hit limit. Raw transcript text is omitted from this record.
The retained evidence does not establish that this exact request produced the
19.894872-second result.
The earlier [shared-server trial](shared-stack-2026-10-03.md#search-diagnosis)
is a separate measurement.

## Claims under test

| Claim | Impact if false | Strongest evidence | Missing evidence | Status | Next action |
| --- | --- | --- | --- | --- | --- |
| Current startup cost can precede the first verified result | Small page limits still time out | SQL cursor experiment and 49,500 rejected exact-page rechecks | Current production trace not repeated | POC-confirmed | Measure actual adapter/API path |
| More bitmap memory alone solves broad searches | Memory spent without useful latency improvement | Prior production trial and synthetic exact-page rechecks | This small heap cannot reproduce production bitmap lossiness | contradicted as a general solution | Tune only for demonstrated lossiness |
| An embedded positional index improves candidate selection on this fixture | Dependency and index maintenance may buy little | Same-corpus Go-library search; exact final-repeat ID sets | Production joins, ordering, concurrency and freshness absent | POC-confirmed | Build isolated authoritative adapter |
| Default code-search settings preserve all transcript text | Silent false negatives | Four executable exclusion fixtures | Production corpus eligibility distribution unknown | contradicted | Maintain explicit indexed/fallback coverage |
| A timestamp cursor gives reliable continuation | Missed ties and late commits | Executable counterexamples | None for these counterexamples | contradicted | Pin a published immutable generation |
| Generation scan can reauthorize and serialize retries | Leaked or skipped evidence | Twenty protocol tests, including concurrent same-sequence calls | Durable publication, database CAS and actual policy integration absent | POC-confirmed in model only | Integrate generation and authority boundaries |
| An empty final page proves no matches | False absence claim | Redacted representative / suppressed duplicate counterexample | Exact final permitted grouping not implemented | contradicted | Separate coverage completion from aggregate absence |
| Thirty-person latency target is achieved | Misleading capacity commitment | No qualifying experiment yet | Representative corpus, concurrency and ingestion tests | unresolved | Define and run a later capacity qualification |

## Experiments

All experiments used synthetic content in an isolated worktree. This session
changed no production database, deployment or configuration. PostgreSQL and
embedded-index timing runs were serialized to reduce mutual interference.
The related session's rollout is recorded separately above.

Environment: Apple M1 Pro, ten logical CPUs, 16 GiB RAM; native PostgreSQL
16.15, Python 3.14 with psycopg 3.3.6, and Go 1.26.6. The earlier deployment
trial used PostgreSQL 17.6. Zoekt is pinned to
`v0.0.0-20261014141728-4abc8196181a` in a separate POC module. No production
module file changed.

The shared fixture contains 50,000 messages and 134,144,076 bytes of text
(127.9 MiB). Ordinary rows contain roughly 2 KiB of deterministic hexadecimal
hashes and markers; 100 rows repeat their content to roughly 270 KiB. This
exercises trigram saturation and TOAST, but is not a representative natural
transcript distribution. Scope zero disproportionately includes the large
messages. Its SHA-256 is
`95433ec52159daa65a4dc760c407f08ece41081b3f16d98c26cc44cfd042c211`.

### PostgreSQL

All 28 pattern/scope/memory variants matched the complete ordered oracle.
Each variant has three sequential samples. The verifier is Python regex over
the tested ASCII subset, not the production Go collector. The SQL uses the
current whole-literal predicate shape but omits production session/visibility
joins. A weak scattered-trigram predicate is retained only as a diagnostic;
it is not presented as the current baseline.

Unscoped median milliseconds with 4 MiB `work_mem`:

| Query | Current predicate: candidate page | Current predicate: hydrate and verify all | Native regex: hydrate and verify all |
| --- | ---: | ---: | ---: |
| `heron` | 400 | 471 | 666 |
| `timeout` or `failure` | 245 | 1,198 | 1,235 |
| Rare old `ancientneedle` | 0.93 | 2.34 | 2.35 |

The `heron` plan rejected 49,500 rows during heap recheck to return 500
matches. It touched 417 exact heap blocks and zero lossy blocks. Native regex
had the same rejected-recheck count. Raising query-local memory to 32 MiB
therefore had no bitmap lossiness to remove and produced no consistent gain.
Scope reduction helped, but changes the searched population and is not an
unscoped optimization.

The full table/index/TOAST footprint was 304,316,416 bytes. The first GIN build
was observed still running after 4m02s and complete before the next check,
approximately five minutes; its exact duration was not instrumented. The
archived script now records setup/index duration for future runs.

### Embedded positional index

Zoekt built the shared corpus in 7.09 seconds without a network service.
Its index occupied 409,506,103 bytes, about 3.05 times source text. That is
additional storage while the existing database/indexes remain. It is not a
deployment memory estimate.

Unscoped search times in milliseconds:

| Query | Verified documents | First sample | Repeat 1 | Repeat 2 |
| --- | ---: | ---: | ---: | ---: |
| `heron` | 500 | 1,060 | 74 | 20 |
| `timeout` or `failure` | 15,714 | 351 | 53 | 37 |
| Rare old `ancientneedle` | 50 | 1.37 | 0.156 | 0.178 |
| Literal-free `[A-Za-z]{30}` | 0 | 3,479 | 2,982 | 2,761 |

The final repeat of each shared query matched Go regexp's complete document-ID
set, with zero search skips or crashes in retained stats. Equality and engine
stats are retained for the final repeat, not every timing sample. First samples
are not controlled cold-cache runs: building the index and computing the oracle
already touch data. Three samples cannot establish p95.

The literal-free query read all 134 MB. Positional indexing does not make
arbitrary exhaustive regex constant-cost. Zoekt timings enumerate engine
matches; PostgreSQL timings include a separate Python hydration/verifier.
Neither includes the full Flopwire API, authorization, collector, deduplication
and response path, so these are mechanism-screening measurements, not a
controlled end-to-end speedup ratio.

Default Zoekt settings missed known matches in four isolated fixtures:
a message over 2 MiB, a two-byte message, a NUL-containing message, and a
100 KiB message with more than 20,000 distinct trigrams. Explicit size and
large-file settings restored the large/diverse fixtures; short and NUL content
remained excluded. Flopwire normalizes NUL in `internal/ingest/sink.go:clean` before PostgreSQL
storage, so that
fixture is an adapter-boundary check rather than an observed stored-corpus bug.
Skipped content can be replaced by a synthetic marker, making final original-body
verification necessary as well as an explicit coverage/fallback manifest.

Tested multiline, Unicode folding and duplicate-content document IDs agreed
with Go regexp. These fixtures do not prove complete regex compatibility.
The library's streaming progress concerns one running query. It exposes neither
a durable continuation cursor nor timestamp-ordered pagination. Its index
package also has a larger dependency and maintenance surface than a small
stable search adapter; the pinned source, rather than a stable API promise,
is the basis of this experiment.

### Continuation and authority model

Twenty tests pass, including nine that exercise the proposed generation model.
They cover fixed initial scope, later arrivals, version suppression, current
principal revocation, redaction on cached terminal replay, representative
deduplication, competing same-sequence requests, cancellation and expiry.

Counterexamples reject timestamp-only cursors, allocated-sequence watermarks,
hit-offset retries and independent text chunks as general solutions. A terminal
empty page is also insufficient to establish scan-wide absence. In particular,
an emitted representative can be redacted after a permitted duplicate was
already suppressed. Replaying stored page IDs then loses the representative
without restoring that duplicate. Exact final permitted results require
retained matching groups and representative reselection; that is unimplemented.

The model serializes state and authority with Python locks and receives an
already-published generation as input. It does not implement durable snapshot
publication, database compare-and-swap, crash recovery, production authorization
or bounded progress inside oversized messages. Model success is not a security
or production-concurrency qualification.

## Decision and remaining gates

Proceed with a bounded embedded-adapter POC before choosing a separate service.
Keep PostgreSQL authoritative and the original Go matcher responsible for final
evidence. The adapter must account for every source message as indexed or
explicitly requiring fallback; engine-default omissions cannot become complete
empty answers.

Work backward from the selected snapshot contract:

1. Prove generation publication, pinning, replacement and expiry against
   real ingestion/version changes. Pin initial eligible scope; current read
   authority may only narrow it. A fresh search observes newer data or grants.
2. Prove the exact-version authority fence before rendering every page and
   replay. Store IDs/versions rather than blindly replaying cached plaintext.
3. Define and test chronological continuation, cross-page deduplication and
   scan-level absence. Preserve existing offset behavior separately. Do not
   claim exact final result sets until the matching-group counterexample is fixed.
4. Qualify first verified page and exhaustive completion with the production
   collector, representative transcript distributions, PostgreSQL 17, concurrent
   ingestion and multiple searchers. Set hardware and corpus-growth assumptions
   before claiming the proposed hosted-team p95 target.

Production integration and capacity remain unqualified. The next experiment
should prove the adapter contract before adding a production dependency or
changing the deployment.

## Reproduction and review

The accompanying [evidence archive](broad-regex-poc-2026-10-14.tar.gz) contains only synthetic
generators, standalone POC source, pinned dependencies, raw small results,
plans and per-experiment instructions. It excludes corpus text, database data,
virtual environments, executables and generated indexes. Extract into
`.poc-stress-test/` and follow each README. The original temporary cluster was
stopped and the POC workspace was cleaned after preserving this evidence.

An independent Sol review identified gaps in the initial eleven-test model:
binding was not reauthorization, concurrent retries were untested, and the
generation example did not implement pagination. The revised twenty-test
model addresses those in memory and explicitly retains the integration gaps
above. The review also distinguishes candidate startup from verified results
and rejects cold-cache, percentile and hosted-capacity claims from these runs.

## Primary references

- [Google Code Search trigram design](https://swtch.com/~rsc/regexp/regexp4.html)
- [Zoekt design](https://github.com/sourcegraph/zoekt/blob/main/doc/design.md)
- [GitHub Blackbird architecture](https://github.blog/engineering/the-technology-behind-githubs-new-code-search/)
- [PostgreSQL pg_trgm](https://www.postgresql.org/docs/17/pgtrgm.html)
- [PostgreSQL bitmap combination and ordering](https://www.postgresql.org/docs/17/indexes-bitmap-scans.html)
- [Sourcegraph interactive and exhaustive query behavior](https://sourcegraph.com/docs/code-search/queries)
