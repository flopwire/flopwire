# Broad regex search investigation, 2026-10-14

Status: experiments in progress. This record does not qualify a production
implementation or a 30-person deployment.

## Objective

Return a small page of verified evidence quickly, with explicit search
completion and a way to continue unfinished work. The target is a hosted
team of at least 30 people. A proposed target is p95 below one second for
the first useful page of representative indexed queries. Corpus size,
concurrency, hardware and freshness bounds must be specified before this
becomes an acceptance criterion.

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
| Current startup cost can precede the first verified result | Small page limits still time out | ID cursor applies text predicates and ordering before fetch; prior recheck measurements | Reproducible current query trace | code-confirmed | Run isolated PostgreSQL comparison |
| More bitmap memory alone solves broad searches | Memory spent without useful latency improvement | Earlier trial removed lossy pages but remained slow | Controlled current workload | contradicted as a general solution | Measure query-local memory separately |
| An embedded positional index can improve candidate selection | Dependency and index maintenance may buy little | Zoekt positional design | Same-corpus timings and semantic equivalence | unresolved | Embed pinned library in an isolated POC |
| Default code-search settings preserve all transcript text | Silent false negatives on large messages | Source review identifies file-size and result limits | Executable large-message checks | unresolved | Compare defaults with explicit settings |
| A timestamp cursor gives reliable continuation | Missed ties, late arrivals and changed messages | Current hit offsets do not resume scans | Executable mutation/retry model | unresolved | Test snapshot and continuation invariants |
| Thirty-person latency target is achieved | Misleading capacity commitment | No qualifying experiment yet | Representative corpus, concurrency and ingestion tests | unresolved | Define and run a later capacity qualification |

## Experiments

All experiments use synthetic content in an isolated worktree. No production
database, deployment or configuration is changed. PostgreSQL and embedded
index timing runs are serialized to reduce mutual interference. Laptop
measurements are exploratory and do not establish hosted capacity.

1. PostgreSQL: compare the current whole-literal prefilter with justified
   alternatives; collect exact results, first-page latency and recheck data.
2. Embedded Zoekt: use the same corpus; compare results with Go regexp;
   inspect large-message, Unicode, multiline and result-limit behavior.
3. Continuation model: exercise ordering ties, ingestion, mutations,
   redaction, visibility changes, retries, expiry, deduplication and oversized
   messages. Model success is not production concurrency qualification.

## Primary references

- [Google Code Search trigram design](https://swtch.com/~rsc/regexp/regexp4.html)
- [Zoekt design](https://github.com/sourcegraph/zoekt/blob/main/doc/design.md)
- [GitHub Blackbird architecture](https://github.blog/engineering/the-technology-behind-githubs-new-code-search/)
- [PostgreSQL pg_trgm](https://www.postgresql.org/docs/17/pgtrgm.html)
- [PostgreSQL bitmap combination and ordering](https://www.postgresql.org/docs/17/indexes-bitmap-scans.html)
- [Sourcegraph interactive and exhaustive query behavior](https://sourcegraph.com/docs/code-search/queries)
