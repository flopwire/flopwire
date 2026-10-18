# Parallel sync

By default the device agent uploads one source at a time. Two upload
workers let the agent overlap the network waits of two sources. Use them
when uploads are slow because of network latency, for example during a
large first upload to a distant server.

## Settings

| Setting | Where | Values | Default |
|---|---|---|---|
| `--sync-workers` | `flopwire agent run` | `1` or `2` | `1` |
| `--flush-per-device` | `flopwire serve` | `1` or `2` | `1` |

Both settings must be `2` for a device to upload with two workers.

1. Start the server with `flopwire serve --flush-per-device=2`.
2. Start the collector with `flopwire agent run --sync-workers=2`.
3. Run `flopwire agent status`.
4. Confirm that the status shows `upload workers: 2`.

The server must reserve at least two global upload slots in its PostgreSQL
pool. The server computes its global upload slots from the pool size, after
it reserves connections for retrieval, parse workers and other operations.
It admits at most eight uploads at a time. A pool with no room for an
upload slot stops the server at startup.

## What two workers change

- Two sources upload at the same time. Only the network waits overlap.
- Capture, exports and local state changes stay serialized.
- A source path has one operation at a time. The server also refuses a
  second upload of the same device path (`source_busy`).
- Scoped Code and Cowork uploads keep their exclusive authorization leases,
  so they can serialize uploads.
- The agent keeps one sync database and one spool for all workers.

Worker count does not set a memory limit. It does not prove that the
agent covers all history.

## How the agent selects one worker

The collector asks the server for its upload limit over its pinned,
authenticated connection (`GET /v1/sync/capabilities`). The agent uses two
workers only when it requests two and the server permits two. It selects
one worker in these cases:

- The server reports one worker.
- The server has no capability endpoint.
- The capability check fails. Serial uploads continue, status reports the
  error, and the agent retries the check.
- The server refuses an upload with `flush_in_progress` (the refusal of a
  server that permits one upload per device). The agent stays at one
  worker until a later capability check succeeds.

A current authentication or TLS-pin failure stops uploads. It does not
select one worker.

A server with `--flush-per-device=2` refuses a third upload from one device
with `device_busy`. It refuses an upload above its global limit with
`server_busy` (HTTP 503). Each refusal carries `Retry-After`. The agent
backs off and retries. A hook does not bypass the backoff.

## Read the diagnostics

`flopwire agent status` shows these lines:

| Line | Means |
|---|---|
| `upload workers: N` | The effective upload concurrency. |
| `upload capability check: ERR (using one worker; retrying)` | The capability check failed. Serial uploads continue, and the check retries. |

The JSON fields are `upload_workers` and `concurrency_error`.

Empty queues do not prove complete history coverage. See
[coverage reporting](coverage-reporting.md). For other status lines, see
[Check the agent](agent.md#check-the-agent).

The design rationale is in
[notes/parallel-sync-design.md](../notes/parallel-sync-design.md).
