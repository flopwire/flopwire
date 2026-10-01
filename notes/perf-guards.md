# Performance guards

Decided 2026-09-30. Goal: a PR that adds a quadratic loop, a full-table rescan, an N+1 query pattern or an unindexed hot query fails CI. Wall-clock speed is not gated on PRs; it is tracked at release.

## Current state

- CI has no perf gate. `BenchmarkScan`/`BenchmarkBlake3` (`internal/devicesync/bench_test.go`) never run.
- Incidental guards that do run: 200MB sync heap bound (`internal/devicesync/large_test.go`), one SQLite plan check (`internal/localindex/fts_test.go:139`), liveness timeouts.
- Real limits live in `flopwire bench acceptance` (manual, real corpus, results not stored).
- No Postgres-side scale, plan or query-count test exists. The riskiest code is there.

## Decisions

1. **PR gate = complexity guards, not timing.** Hosted runners plus `-race` make wall-clock noisy. Guards are deterministic.
2. **Four mechanisms**, all built:
   - Postgres plan assertions on hot queries.
   - Query-count tracer per operation, to catch N+1 and per-row loops. It implements `pgx.BatchTracer` as well as `pgx.QueryTracer`: statements queued in a `pgx.Batch` (the ingest sink's per-row writes) never reach `TraceQueryStart`.
   - N vs 8N scaling ratio on cost.
   - Heap/allocation bounds for pure-Go paths (extends the `large_test.go` pattern). `-race` adds allocations, so `AllocsPerRun` bounds skip under the race build tag or carry race-specific limits.
3. **Cost metric = rows and blocks touched**, from per-database `pg_stat_user_tables` / `pg_statio_user_tables` deltas (`seq_tup_read`, `idx_tup_fetch`, `n_tup_ins/upd/del`, `heap_blks_hit+read`, `idx_blks_hit+read`), read only after every backend that did the work has exited: the operation runs on its own pool, the test closes it (backend exit flushes pending stats), then a fresh connection reads the views. `pg_stat_force_next_flush()` flushes only the calling backend, and idle pool backends flush after 1–10 s, so it is not enough on its own. Tuple counts are the primary metric. `seq_tup_read` skips tuples a scan cannot see, so a scan over rows deleted earlier in the same transaction (a foreign-key action after a cascade) costs nothing in tuples; seq pages (`seq_scan` delta times the table's pages) is gated with them. Block counts are secondary, since autovacuum and analyze touch blocks. Fixture tables set `autovacuum_enabled = off`. No `pg_stat_statements`: it needs `shared_preload_libraries`, which a GitHub service container cannot set. `pgtest` already gives each test its own database, and the stats views are per database, so parallel packages on one cluster do not mix counts.
4. **Small fixtures.** Plan tests run with `SET enable_seqscan = off`; a remaining Seq Scan on a hot table means no usable index (catches missing FK indexes and index-defeating casts such as `c.id::text > $2`). With seq scans disabled the planner falls back to a full index or bitmap scan, so the assertion also fails on any scan of a hot table without an `Index Cond` that bounds it. Scaling tests use a synthetic transcript generator at N≈500 and 8N≈4000 messages. Target: seconds per package under `-race`.
5. **Each guard lands with its fix** in one small PR. Main stays green; no allowlist. Indexes go into the existing migrations in place (pre-release, see `agent-workflow.md`).
6. **Thresholds per operation class**, declared by each test:
   - Bulk (reparse, rules upgrade, delete): linear, cost ratio ≤ 12× at k=8.
   - Incremental (append parse, one retrieval page): constant in session/corpus size, ratio ≤ 2×.
7. **Retrieval pagination:** `sessions` moves to a keyset cursor on its sort key `(last_activity_at, id)` and read outline to one on `(ordinal, id)`. Both return `has_more` instead of an exact `count(*)` total. API, CLI, MCP and web callers change together.
8. **Release tier:** `flopwire bench acceptance` writes JSON. Each release commits its result under `docs/perf/`. `scripts/acceptance.sh` diffs against the previous record and flags >20% regressions. Still manual, on the reference laptop.
9. **Device side mirrors the server:** SQLite `EXPLAIN QUERY PLAN` checks for hot local queries, N vs 8N on local index apply via a counting driver hook, heap/alloc bounds on scan, chunk and parse.

Out of scope for round 1: web bundle budgets (admin-only console), HTTP load tests, nightly hosted benchmarks.

## Known violations the guards will catch

| # | Path | Problem | Guard |
|---|------|---------|-------|
| 1 | `internal/ingest/reparse.go:47`, `parse.go:112`, `sink.go:337`, `digest.go:72` | `nextRefresh` full scan per source; `parseAttempt` in `sameMeta` rewrites every row; full digest recount per 500-row batch, ≈(N/500)·N | scaling (bulk), query count |
| 2 | `internal/ingest/reparse_redaction.go:16-70` | `source_id=$1` must include superseded rows, but every `messages.source_id` index is partial (`NOT superseded`): a messages seq scan per 64-row batch; `c.id::text>$2` defeats the PK and `OR EXISTS` forces a conversations seq scan per batch | plan, scaling (bulk) |
| 3 | `migrations/001_schema.sql:238,281,299`, `store/deletion.go:259`, `ingest/rules.go:451,456` | no index on `messages.superseded_by`, `conversations.source_id`, or an unfiltered `messages.source_id` (FK `ON DELETE SET NULL` and the deletes scan) | plan, scaling (bulk) |
| 4 | `internal/ingest/reader.go:69`, `parse.go:642`, `digest.go:150` | append loads the whole manifest and whole `redacted_lines`; count query per failed tool id | scaling (constant), query count |
| 5 | `internal/retrieval/tools.go:84,401,761,771` | no `messages(ts)` index for grep's `ORDER BY m.ts`; `count(*)` + `OFFSET` | plan, scaling (constant) |
| 6 | `internal/devicesync/syncer.go:392`, `upload.go:190` | one SQLite query per chunk in `store.known` | query count |
| 7 | `internal/localindex/apply.go:137,189`, `agent/agent.go:544` | row-at-a-time writes, full digest recount, linear `removeTarget` | scaling (bulk), plan |

## PR sequence

1. **Harness** (`internal/perfguard` or inside `pgtest`): table-stats snapshot/delta, query-count tracer, `AssertNoSeqScan(t, db, query, args...)`, `AssertScaling(t, class, run func(n int))`, synthetic transcript generator. Self-tests only.
2. **FK indexes** (#3): plan tests + migration edit + delete scaling test.
3. **Rules upgrade** (#2): plan + scaling + fix.
4. **Reparse/digest** (#1): scaling + query count + fix.
5. **Append parse** (#4): constant-class scaling + fix.
6. **Retrieval keyset** (#5): plan + constant-class scaling + API change.
7. **Device** (#6, #7): SQLite plan checks, counting hook, alloc bounds, fixes.
8. **Acceptance recording**: JSON output, `docs/perf/`, diff in `scripts/acceptance.sh`, release-checklist update.

PRs 2–7 depend only on PR 1 and can run in parallel per `agent-workflow.md`.

Every PR, including this plan, gets an independent reviewer subagent (reviewer rules in `agent-workflow.md`) before it lands. Merge only after the review fixes are in and CI is green.
