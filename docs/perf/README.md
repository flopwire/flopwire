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
with the newest `recorded_at`. A metric that grew by more than 20% is
flagged `REGRESSED`. `--threshold` changes the 20%. The command exits 0
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

| Metric | Unit | Limit | What it measures |
|---|---|---|---|
| `index.wall` | s | 300 | Wall time of the full local index of the corpus |
| `index.peak_rss` | MB | 600 | Peak RSS of that index run |
| `idle.rss` | MB | 120 | RSS of the agent after 60 s idle on the built index |
| `sweep.cpu_max` | ms | 1000 | Slowest no-change sweep CPU time |
| `fresh.p95` | ms | 2000 | p95 time until an appended line is findable |
| `query.<name>.warm` | ms | 200 | Warm CLI latency of each query in `testdata/acceptance/queries.yaml` |

Checks have no number to compare: `query.<name>` (expected hits and
address round-trips) and `oracle.<agent>` (parser parity sample, 0 parse
errors).

`corpus` counts the transcripts the index part reads: every `.jsonl` under
the Claude projects root, every Codex rollout, and the Devin store.
