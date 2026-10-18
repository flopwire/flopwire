# Parallel sync design

This note gives the reasons behind the two-worker upload design. For the
settings and diagnostics, see [docs/parallel-sync.md](../docs/parallel-sync.md).

## Why only network waits overlap

Upload time on a distant server is mostly request latency. Two workers that
overlap network waits recover most of that time. Parallel capture would also
need parallel scan buffers, descriptor ownership and spool bookkeeping. The
design keeps capture, exports, metadata writes and acknowledgments serialized
behind one `Store` and one `Spool`. No SQLite transaction or database
connection is held across network I/O. Adding a worker does not add a sync
database or a SQLite writer connection.

## One operation per source path

The scheduler owns job transitions. Workers receive immutable work snapshots
and return results; they do not change shared queue state. A path is waiting,
ready, running or retrying. A running path keeps at most one coalesced newer
notification. The next operation on that path starts only after the current
one completes. This keeps generations, chunk boundaries and acknowledgments
the same as with one worker.

The server enforces the same rule. Its admission owner keys each upload by the
authenticated device ID and the literal source path. File ID, generation,
agent, storage kind and session do not distinguish two requests at one path.
Ownership lasts until the flush, chunk unlock, connection release and parse
notification finish. A conflicting path gets a retryable `source_busy` 429
that does not include the path.

## Server admission

After API authentication, the server reserves a global slot and a device slot
before it decodes the ingest header or takes an ingest database connection.
It releases each slot once, after the handler stops work, also on malformed
input, cancellation and disconnect. A rejected flush does not read its body.

The global slot count comes from the configured PostgreSQL pool. The server
reserves the retrieval bound, the effective parse workers and fixed headroom
for other operations, and admits at most eight flushes from the remainder.
Servers that share one process and pool share one admission owner. The owner
is process-local; it is not cluster-wide admission.

A server with a per-device limit of one refuses a second flush with
`flush_in_progress`. With a limit of two it uses `device_busy`. Global
contention returns `server_busy` 503. All carry `Retry-After`.

## Capability negotiation

The collector reads the upload limit from the authenticated
`GET /v1/sync/capabilities` response. It never infers concurrency from the
binary version and never provokes a 429 to discover it. A 404 cannot tell an
old server from a wrong proxy route, so the collector uses one worker and
reports the decision. Authentication, pin, network and malformed-response
failures stay visible; they do not read as an older server. A
`flush_in_progress` refusal clamps the collector to one worker until a later
capability check succeeds.

## Pressure and stale completions

A cooldown or a permanent stop advances the scheduler's gate epoch. A
completion dispatched before that epoch keeps its durable acknowledgment but
cannot clear the newer backoff or stop. A hook flush promotes a path but
cannot bypass a `Retry-After` deadline.

## Shared chunks

Shared chunks stay content addressed. A request does not wait on a chunk lock
while it holds other chunk locks. When another request owns a chunk, the
server returns a partial result, and the client retries. The client counts
no-progress responses per source and generation across scheduler turns, and
fails the source after the third.
