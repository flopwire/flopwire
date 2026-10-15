# Local daemon work diagnostics

Run `flopwire agent diagnostics` to inspect the running local daemon. Add
`--json` to save a snapshot. This command uses the local control socket. It does
not query the server, scan transcripts, read the index, or refresh permissions.
The separate `flopwire diagnostics` command reports extraction coverage.

## Measure an interval

1. Wait for the startup queue to drain.
2. Save a snapshot:

   ```sh
   flopwire agent diagnostics --json > before.json
   ```

3. Leave the daemon running for at least two sweep intervals. The default sweep
   interval is 45 seconds. Record whether other agent sessions remain active.
4. Save another snapshot:

   ```sh
   flopwire agent diagnostics --json > after.json
   ```

5. Check that `pid` and `started_at` match between snapshots.
6. Calculate CPU use as `100 * Δprocess_cpu_ns / Δuptime_ns`. A value of 100
   means one CPU core. Calculate allocation rate from
   `Δheap_allocated_bytes_total / elapsed_seconds`.
7. Compare operation call counts and `total_wall_ns` between snapshots.
8. Repeat the interval while one session produces an update.

Call an interval quiet only when other sessions, startup work, and upload
recovery are inactive. These counters do not establish upload acknowledgment
or server indexing latency; use sync coverage and separate delivery observations
for those measurements.

## Interpret the counters

Counters are cumulative. Operation counters start when the agent or scheduler
is constructed. CPU, allocation, and GC totals cover the process lifetime.
Counters are not persisted or reset by reads. Concurrent snapshots are
approximate; an operation that crosses the interval contributes its full
elapsed time when it completes.

| Operation | Measured work |
| --- | --- |
| `index_target` | Index one transcript or companion, including its lock waits |
| `fast_lane` | Recheck likely active files and poll harness stores |
| `watcher_event` | Handle a filesystem event, including any directory scan |
| `store_poll` | Attempt a configured harness-store poll, including lock waits and skipped polls |
| `idle_trim` | Release SQLite caches and force Go garbage collection after an idle check |
| `sweep` | Full discovery pass, including policy refresh and reconciliation |
| `discover_all` | Source discovery, including calls outside sweeps |
| `authorize_capture` | Fresh permission acquisition before capture/upload |
| `refresh_from_sweep` | Scope refresh requested by a sweep, including lock waits |
| `refresh_from_authorization` | Scope refresh requested by authorization, including lock waits |
| `refresh_cowork_lock_wait` | Wait for the scope-refresh serialization locks |
| `refresh_cowork` | Desktop Code and Cowork scope refresh after lock acquisition |
| `captured_claude_children` | Retained child-evidence lookup inside scope refresh |
| `sync.turn` | Scheduled source turn, including source-operation lease acquisition |
| `sync.no_work_probe` | Read-only proof that an ordinary file is fully settled |
| `sync.authorize` | Scheduler's authorization callback |
| `sync.capture_upload` | Capture/upload operation, including I/O and network waits |

`sync.settled_turns` counts successful no-work shortcuts. The `sync` section is
absent when the configured sync implementation does not expose these counters.
Each operation reports calls, completed calls, in-flight calls, total wall time,
and the maximum completed-call wall time since construction.

Wall timings overlap and include waits. Do not add them to obtain CPU time.
For example, authorization includes scope refresh, and scope refresh includes
the historical lookup. The maximum is a lifetime maximum, not an interval
percentile. Trigger-specific refresh counts need not sum to all refreshes:
initialization and other control paths can also refresh scope.

Frequent authorization refreshes with substantial historical-lookup wall time
suggest repeated evidence work. Long lock-wait time suggests serialization.
Idle trimming calls `debug.FreeOSMemory`, which forces a Go GC cycle. GC totals
can therefore rise even without allocation pressure. Compare `idle_trim` calls
and wall time with GC and CPU deltas before attributing activity to allocations.

High process CPU and allocation deltas with modest I/O waits suggest computation
or garbage collection. These are hypotheses; confirm CPU attribution with a
profile before changing behavior.

## CPU profiles

The existing `flopwire agent run --cpuprofile PATH` flag records a Go CPU profile.
Use a controlled run with a separate home/index or a planned daemon restart.
Stop the run gracefully so the profile is finalized. Do not start a second writer
on the live index. Inspect the profile with `go tool pprof PATH`.

Work diagnostics contain fixed operation names and numeric/timing fields. They
contain no transcript text, source paths, session IDs, or credentials. There is
no new telemetry endpoint or persistent metrics database.
