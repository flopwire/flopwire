# Search engine decision experiment — 2026-10-14

Status: in progress. This bounded experiment follows [#268](https://github.com/flopwire/flopwire/pull/268) and [#269](https://github.com/flopwire/flopwire/pull/269). It compares candidate engines through one verification path and tests freshness independently. Production code remains unchanged.

## Accepted contract

A search fixes its initial history. Later revocation, redaction, hiding, and deletion can suppress results. A new search includes newly committed messages. Distinct message addresses remain separate even with identical text. Retries reauthorize the same page. A server restart or deployment may expire cursors explicitly in v1.

## Decision gates

Measure the real PostgreSQL schema and embedded Zoekt against the same synthetic corpus, original Go matcher, ordering, scopes, and batched current-body verification. Include candidate startup in first-page cost. Share immutable generations; do not hide per-search corpus copies outside the timer.

The aspirational gate is p95 first useful page below one second for common indexable queries at a declared workload. It is not a promise for arbitrary regex completion. Report overload and incomplete coverage separately from successful results. Test mixed queries, large tool outputs, exact UUID parity, and freshness during out-of-order commits.

Keep Postgres if it meets the measured workload. Advance embedded search only if candidate enumeration remains a material bottleneck and the benefit warrants generation management. Neither a synthetic burst nor an engine microbenchmark qualifies a hosted team of 30 people.

## Pending evidence

- Candidate-engine comparison on real migrated tables and a shared verification path.
- Shared-generation allocation and candidate enumeration bounds.
- Real PostgreSQL freshness counterexamples and version coverage.
- Independent source/methodology review, raw outcomes, and source archive.
