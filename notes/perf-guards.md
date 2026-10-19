# Performance guards

Goal: a PR that adds a quadratic loop, a full-table rescan, an N+1 query pattern or an unindexed hot query fails CI. Wall-clock speed is not gated on PRs; it is tracked at release.

## Current state

Two tiers run in CI:

- **PR tier: complexity guards.** `internal/perfguard` holds the
  mechanisms of decision 2. Tests in `internal/agent`, `bus`,
  `devicebus`, `devicesync`, `ingest`, `localindex`, `retrieval`, `store`
  and `transcript/claude` use them. They run inside `go test -race ./...`
  in `ci.yml`, against the workflow's Postgres service, so a guard failure
  fails the PR like any other test.
- **Timing tier: A/B bench on a synthetic corpus.**
  [`docs/perf/README.md`](../docs/perf/README.md) describes it.
  `.github/workflows/perf-ab.yml` is a reusable workflow: it picks the
  baseline with `scripts/perf-baseline.sh` (the latest `v*` tag, or the pin
  in `docs/perf/nightly-baseline` when the pin is newer), builds the
  baseline and the commit under test, generates and verifies the 1.5GB
  corpus with `flopwire bench corpus`, and runs `flopwire bench ab`.
  - `perf-nightly.yml` calls it at 06:23 UTC on `main` and on manual
    dispatch. A `REGRESSED` verdict opens or comments on a
    `perf-regression` issue; a clean run closes it.
  - `perf-release.yml` calls it only on the release-please PR, posts the
    comparison as a PR comment, and fails the required `perf-gate` check
    unless the verdict is `CLEAN`. On every other PR and in the merge
    queue, `perf-gate` passes at once.
- **Local acceptance.** `flopwire bench acceptance` on the reference
  laptop over the real corpus is optional; a run commits its JSON record
  under `docs/perf/`.

## Decisions

1. **PR gate = complexity guards, not timing.** Hosted runners plus `-race` make wall-clock noisy. Guards are deterministic.
2. **Four mechanisms**, all built:
   - Postgres plan assertions on hot queries.
   - Query-count tracer per operation, to catch N+1 and per-row loops. It implements `pgx.BatchTracer` as well as `pgx.QueryTracer`: statements queued in a `pgx.Batch` (the ingest sink's per-row writes) never reach `TraceQueryStart`.
   - N vs 8N scaling ratio on cost.
   - Heap/allocation bounds for pure-Go paths (extends the `large_test.go` pattern). `-race` adds allocations, so `AllocsPerRun` bounds skip under the race build tag or carry race-specific limits.
3. **Cost metric = rows and blocks touched**, from per-database `pg_stat_user_tables` / `pg_statio_user_tables` deltas (`seq_tup_read`, `idx_tup_fetch`, `n_tup_ins/upd/del`, `heap_blks_hit+read`, `idx_blks_hit+read`), read only after every backend that did the work has flushed its statistics: `perfguard.TakeSnapshot` runs `pg_stat_force_next_flush()` on every connection of the pool that did the work, which forces each backend's flush before it answers, then reads the views on one connection. It refuses to read while another connection to the database stays open, because that backend cannot be flushed in-band. Idle backends otherwise flush after 1 to 10 s. Tuple counts are the primary metric. `seq_tup_read` skips tuples a scan cannot see, so a scan over rows deleted earlier in the same transaction (a foreign-key action after a cascade) costs nothing in tuples; seq pages (`seq_scan` delta times the table's pages) is gated with them. Block counts are secondary, since autovacuum and analyze touch blocks. Fixture tables set `autovacuum_enabled = off`. No `pg_stat_statements`: it needs `shared_preload_libraries`, which a GitHub service container cannot set. `pgtest` already gives each test its own database, and the stats views are per database, so parallel packages on one cluster do not mix counts.
4. **Small fixtures.** Plan tests run with `SET enable_seqscan = off`; a remaining Seq Scan on a hot table means no usable index (catches missing FK indexes and index-defeating casts such as `c.id::text > $2`). With seq scans disabled the planner falls back to a full index or bitmap scan, so the assertion also fails on any scan of a hot table without an `Index Cond` that bounds it. Scaling tests use a synthetic transcript generator at N≈500 and 8N≈4000 messages. Target: seconds per package under `-race`.
5. **Each guard lands with its fix** in one small PR. Main stays green; no allowlist. Indexes land as new forward-only migrations; existing migrations are never edited.
6. **Thresholds per operation class**, declared by each test:
   - Bulk (reparse, rules upgrade, delete): linear, cost ratio ≤ 12× at k=8.
   - Incremental (append parse, one retrieval page): constant in session/corpus size, ratio ≤ 2×.
7. **Retrieval pagination:** `sessions` uses a keyset cursor on its sort key `(last_activity_at, id)` and read outline one on `(ordinal, id)`. Both return a more flag (`has_more`, `outline_more`) and a cursor instead of an exact `count(*)` total.
8. **Release tier:** the A/B bench on the synthetic corpus gates the release-please PR (`perf-gate`) and runs nightly as the primary drift signal. A metric regresses only when B's median grew by more than 20% and by more than its minimum change, and every B run is above every A run. The real-corpus `flopwire bench acceptance` run is an optional reality check; `scripts/acceptance.sh` diffs its record against the previous one.
9. **Device side mirrors the server:** SQLite `EXPLAIN QUERY PLAN` checks for hot local queries, N vs 8N on local index apply via a counting driver hook, heap/alloc bounds on scan, chunk and parse.

Out of scope: web bundle budgets (admin-only console) and HTTP load tests.
