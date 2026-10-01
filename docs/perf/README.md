# Acceptance records

Each release commits the record of its local acceptance run here. The
release checklist ([§5](../release-checklist.md#5-local-acceptance)) says
when to make one.

## File name

`<version>-<YYYY-MM-DD>.json`, for example `v0.4.0-2026-10-01.json`. The
version is the release tag. The date is the UTC date of the run. `flopwire
bench acceptance --json` prints the name when it writes a record.

## Make a record

1. Run the acceptance set on the reference laptop. Set the version of the
   release:

   ```sh
   FLOPWIRE_VERSION=v0.4.0 scripts/acceptance.sh /tmp/flopwire-acceptance
   ```

2. Read the comparison table at the end.
3. Copy the record to this directory under the name the run printed:

   ```sh
   cp /tmp/flopwire-acceptance/acceptance-record.json docs/perf/v0.4.0-2026-10-01.json
   ```

4. Commit the record in the release PR.
5. Paste the comparison table into the release PR description.

## Compare two records

```sh
flopwire bench compare docs/perf/v0.3.0-2026-09-01.json docs/perf/v0.4.0-2026-10-01.json
flopwire bench compare docs/perf /tmp/flopwire-acceptance/acceptance-record.json
```

When the first argument is a directory, the command uses the record in it
with the newest `recorded_at`. A metric is flagged `REGRESSED` when it
grew by more than 20% and by more than its minimum change (below).
`--threshold` changes the 20%. The command exits 0
unless `--strict` is set; with `--strict` it exits 1 on any regression or
any `FAIL`.

Timings depend on the machine and on the size of the corpus. The command
warns when the OS, architecture, CPU model, core count or RAM differ, or
when the corpus size changed by more than the threshold. Do not treat a
regression across machines as real. Run both builds on one machine to
confirm it.

## Format

```json
{
  "flopwire": {"version": "v0.4.0", "commit": "<git sha>", "dirty": false},
  "recorded_at": "2026-10-01T09:30:00Z",
  "machine": {"os": "darwin", "arch": "arm64", "cpu_model": "Apple M1 Pro", "cores": 10, "ram_bytes": 17179869184},
  "corpus": {"files": 4210, "bytes": 9100000000},
  "metrics": [
    {"name": "index.wall", "value": 450.2, "unit": "s", "limit": 300, "result": "FAIL"}
  ],
  "checks": [
    {"name": "query.error", "detail": "20 hits, reads 5/5", "result": "PASS"}
  ]
}
```

Every metric is lower-is-better and passes when `value < limit`.
A query whose command fails on its first run has no
`query.<name>.warm` metric; its `query.<name>` check fails.

| Metric | Unit | Limit | Min change | What it measures |
|---|---|---|---|---|
| `index.wall` | s | 300 | 10 | Wall time of the full local index of the corpus |
| `index.peak_rss` | MB | 600 | 16 | Peak RSS of that index run |
| `idle.rss` | MB | 120 | 16 | RSS of the agent after 60 s idle on the built index |
| `sweep.cpu_max` | ms | 1000 | 100 | Slowest no-change sweep CPU time |
| `fresh.p95` | ms | 2000 | 20 | p95 time until an appended line is findable |
| `query.<name>.warm` | ms | 200 | 20 | Warm CLI latency of each query in `testdata/acceptance/queries.yaml` |

The minimum change is the absolute growth a metric needs before it can
count as a regression. Small numbers vary from run to run; without the
floor, 0 to 5 ms of sweep CPU or 40 to 49 ms for a query would be
flagged.

Checks have no number to compare: `query.<name>` (expected hits and
address round-trips) and `oracle.<agent>` (parser parity sample, 0 parse
errors).

The record is committed to a public repository, so it holds no host
name, user name, path or transcript text. A failed check records a short
fixed reason, such as `query command: exit status 1` or `missing session
<id>`. The full error output goes to the terminal and to
`<scratch>/acceptance.json` only.

`corpus` counts the transcripts the index part reads: every `.jsonl` under
the Claude projects root, every Codex rollout, and the Devin store.
