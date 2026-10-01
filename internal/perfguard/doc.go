// Package perfguard holds deterministic performance guards for Postgres
// code: cost in rows and blocks touched (table statistics deltas), a
// statement counter, plan assertions, and an N vs kN scaling assertion. It
// never measures wall-clock time. See notes/perf-guards.md.
//
// # Reading table statistics reliably
//
// Since PostgreSQL 15 each backend accumulates table statistics locally.
// When it goes idle it flushes them, but at most once per second; a
// deferred flush waits up to ten seconds. A reader on another connection
// therefore misses recent work, and pg_stat_force_next_flush() run on the
// reader flushes only the reader.
//
// TakeSnapshot runs pg_stat_force_next_flush() on every connection of the
// pool that did the work. On each, the function sets a flag; when that
// backend finishes the statement and goes idle, PostgresMain calls
// pgstat_report_stat, which sees the flag, forces the flush and waits for
// the shared-memory locks (nowait = false), all before it sends
// ReadyForQuery (src/backend/tcop/postgres.c and
// src/backend/utils/activity/pgstat.c, REL_17_6). So when the call
// returns to the client the counts are in shared memory, with no timing
// window. TakeSnapshot then reads the views on one pool connection after
// pg_stat_clear_snapshot().
//
// Closing the pool instead (backend exit flushes) also works, but only
// with polling: Pool.Close returns once the client side closes, and the
// exit flush runs later in the server. The reader would have to wait
// until every pid leaves pg_stat_activity (that entry is cleared by an
// on_shmem_exit hook, after the before_shmem_exit stats flush). The
// in-band flush needs no polling and keeps the pool usable, so one
// fixture can be measured repeatedly.
//
// Before reading, TakeSnapshot checks pg_stat_activity: every client
// backend of the database must have been flushed, or must exit (a pool
// connection destroyed during the work flushes on exit). It refuses to
// read while a pool connection stays acquired or another connection stays
// open, because those backends cannot be flushed in-band; a separate
// pgx.Conn the work used is passed to TakeSnapshot to be flushed. Parallel query workers flush
// when they exit, which can be after the leader returns, so NewPool turns
// parallel query off.
//
// # Noise
//
// pg_stat_user_tables and pg_statio_user_tables exclude system catalogs
// and TOAST tables as relations, so catalog lookups, planning and DDL
// bookkeeping do not count. Autovacuum and autoanalyze touch user table
// blocks; Measure disables them on every table in the database first (per
// table, because autovacuum is a server-wide setting). Each pgtest
// database is private to one test, and the views are per database, so
// parallel packages on one cluster do not mix counts. Rows touched is the
// gated metric; blocks are reported for diagnosis.
//
// Allocation bounds: the race detector adds allocations, so a bound on
// testing.AllocsPerRun should skip or use its own limit when Race is true.
package perfguard
