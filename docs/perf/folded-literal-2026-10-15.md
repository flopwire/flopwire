# Folded literal candidate experiment, 2026-10-15

A folded literal containing three `s` characters generated eight whole-substring
ILIKE alternatives to preserve Go's Unicode long-s matching. On the synthetic
fixture below, PostgreSQL repeatedly evaluated those alternatives while rejecting
trigram false positives. An explicit literal-only regex candidate removes that
amplification. The authoritative matcher remains Go regexp.

The specialization accepts only a capture-unwrapped, ASCII folded literal after
the existing source-size, indexability and 32-variant eligibility checks. It emits
explicit complete Go SimpleFold classes and escapes PostgreSQL metacharacters.
It does not translate user regex syntax. Alternatives, mixed scoped case,
case-sensitive literals, one-variant literals and over-limit expressions retain
their existing candidate paths. Filters, ordering and snapshots are unchanged.

## Production adapter comparison

Baseline: `3fc0415`. Both builds ran the same checked-in
`BenchmarkFoldedLiteralAdapter`, sequentially on an Apple M1 Pro with isolated
Docker PostgreSQL 17. Each measurement is Go benchmark's mean of three measured
calls, following its calibration call. These are warm exploratory results, not
p95, controlled cold-cache measurements or a capacity qualification.

The benchmark uses production migrations and Store.Grep, including filters,
candidate retrieval, hydration and final verification. It creates 30 sessions
with 1,000 messages each. Deterministic hash bodies contain scattered `cass` and
`search`; every hundredth message contains the exact phrase and rare needle;
every five-hundredth has a large repeated body. This deliberately adversarial
fixture is synthetic, not a representative natural transcript distribution.

First-page milliseconds, ordinary / continuation:

| Query | Baseline | Specialized |
| --- | ---: | ---: |
| `cass search` | 2283 / 2321 | 326 / 218 |
| `search` | 370 / 392 | 173 / 194 |
| absent `missingneedle` | 7.6 / 7.2 | 5.7 / 4.4 |
| rare `ancientneedle` (unchanged path) | 229 / 103 | 244 / 113 |
| absent `sssss` (32 old alternatives) | 26.9 / 27.1 | 4.2 / 4.0 |
| mixed scoped case (unchanged path) | 6.6 / 8.1 | 6.9 / 7.2 |

Every ordered first-page message-ID SHA256 matched across builds. Actual
PostgreSQL adapter tests separately compare complete ordinary and continuation
results with a full-scan Go matcher oracle, including Unicode folds, escaped
punctuation and controls. The small changes on unchanged paths show run noise;
no claim is made that every query improves.

Reproduce against a dedicated PostgreSQL role with database-creation rights:

```sh
FLOPWIRE_TEST_DATABASE_URL=postgres://... go test ./internal/retrieval \
  -run '^$' -bench BenchmarkFoldedLiteralAdapter -benchtime=3x -count=1
```

Copy only the benchmark file to the baseline checkout for comparison. Use a
fresh database per run; the helper creates and drops it. No production credentials
or private transcript data are needed.

The [sanitized evidence archive](folded-literal-2026-10-15.tar.gz) contains
the SQL scripts/plans, adapter benchmark source, and both adapter run logs.

## SQL diagnosis and rejected options

The preceding isolated 30,000-row / 31 MB SQL microbenchmark found 29,700
exact-page recheck rejections and zero lossy blocks. Heap rechecking consumed
roughly 1.84 seconds for eight alternatives; final sorting added less than one
millisecond. `ILIKE ANY` retained roughly 1.84 seconds and regressed the frequent
term case, so it was rejected. An explicit factored literal candidate reduced
that fixture to 107–159 ms. Its ordered IDs matched for exact, absent and frequent
terms. The adapter comparison above confirms the useful path through product code.

A metadata index alone did not remove global rechecks. A metadata LIMIT barrier
bounded a progress page, but changes continuation design and cannot guarantee a
first match for sparse queries. That broader work is separate. This change removes folded-literal amplification;
it does not bound startup for every broad grep. Even the single-predicate path
can still recheck many large bodies before the timestamp sort yields a candidate.
