# Performance records

Three runs track performance:

- **Nightly A/B** (primary drift signal). A GitHub Actions run on `main`
  compares a baseline binary with `main` on a synthetic corpus. It runs
  every night and opens an issue when `main` regressed.
- **Release gate** (required before a release). The same A/B bench runs
  on the release-please PR and posts its verdict on the PR. A regression
  fails the PR's `perf-gate` check.
- **Local acceptance** (optional reality check). The release manager can
  run it on the reference laptop over the real corpus. It is recommended
  when a release changes the parsers, indexing or redaction. A run commits
  its record here. The release checklist
  ([§5](../release-checklist.md#5-performance)) says when to make one.

## Nightly A/B

The workflow is `.github/workflows/perf-nightly.yml`. It runs at 06:23 UTC
and on manual dispatch, only in `flopwire/flopwire`, never on pull
requests. The bench steps are in `.github/workflows/perf-ab.yml`, which
the [release gate](#release-gate) also calls.

1. `scripts/perf-baseline.sh` picks binary A, the baseline:
   1. the `baseline` input of a manual run (a commit SHA or a `v*` tag;
      for that run only);
   2. else the newer of the latest release tag `v*` and the commit pinned
      in `docs/perf/nightly-baseline`. The pin wins only when the tag is
      its ancestor, so a release supersedes an older pin.

   The baseline is pinned: no nightly moves it. A baseline that followed
   the nightlies would hide drift below the threshold: 5% a night never
   trips a 20% rule, but five such nights against a pinned baseline do.
   A regression is reported every night until it is fixed or accepted.
2. Binary B is the commit under test. The workflow builds both.
3. `flopwire bench corpus` generates the synthetic corpus and checks that
   every file parses with no parse errors through the production parsers.
4. `flopwire bench ab` first reads the corpus's harness files once, so no run meets
   a cold file cache. Then it runs the index, fresh and queries parts three
   times per binary in ABBA blocks: A, B, B, A, A, B. Neither binary
   always runs first, so drift during the job does not favour one side.
   With an even run count each binary leads equally often; with an odd
   count A leads one pair more. Both binaries run on the same runner and
   the same corpus. The harness is B's for both runs; only the binary
   under test changes.
5. The step summary and the `perf-nightly` artifact hold every run's
   record, the median records `A.json` and `B.json`, and the comparison
   `ab.json` and `ab.md`.
6. When the verdict is `REGRESSED`, the run opens an issue labelled
   `perf-regression`, or comments on the open one. A later clean run
   comments on the issue and closes it.

### Release gate

The workflow is `.github/workflows/perf-release.yml`. It runs on every
pull request, but it benches only the release-please PR: a PR from the
branch `release-please--branches--main` of this repository. On every
other PR its `perf-gate` job passes at once.

On the release PR:

1. `perf-ab.yml` runs the bench above. Binary A is the baseline that
   `scripts/perf-baseline.sh` picks from the PR head: the latest release
   tag `v*`, or the pin in `docs/perf/nightly-baseline` when the pin is
   newer. Before the first release, that is the pin. Binary B is the PR
   head.
2. The `comment` job posts the comparison table and the verdict as one
   PR comment. Each push to the release branch updates the same comment.
3. `perf-gate` fails on `REGRESSED`, on `BASELINE_FAILED` and when the
   bench did not finish. It passes only on `CLEAN`.

A run takes about 17 minutes. A new push to the release branch cancels
the run in progress.

To release with a regression, accept it first: move the pin in a PR that
explains the regression (see [Accept a regression](#accept-a-regression)).
The pin is newer than the last release tag, so the gate then compares with
it. Then push to the release branch again, or close and reopen the release
PR, to rerun the gate.

### Verdict

A metric regresses when all of these are true:

- B's median grew by more than 20% over A's median.
- B's median grew by more than the metric's minimum change (see the table
  under [Format](#format)).
- Every B run is above every A run.

The last rule rejects runner noise: a median shift whose runs overlap is
shown as `noise (runs overlap)` and is not a regression. A check (expected
hits, read round-trips) regresses when it fails in any B run and passes in
every A run. A changed hit count is reported, not counted.

The `A spread` and `B spread` columns are (max - min) / median of one
binary's runs. They measure the noise of the runner. When A and B are the
same commit, the run is an A/A noise measurement.

### Accept a regression

To accept a deliberate regression, move the pin to the head of `main` in a
PR that explains the regression:

```sh
git rev-parse origin/main >docs/perf/nightly-baseline
```

After it merges, the next nightly compares against the new pin; when it
is clean it closes the issue. A release does the same implicitly: its tag
becomes the baseline. To try a baseline once without moving the pin, run
the workflow by hand:

```sh
gh workflow run perf-nightly.yml --ref main -f baseline=<sha or v* tag>
```

### Synthetic corpus

`internal/synthcorpus` writes a fake home with `.claude/projects`,
`.codex/sessions` and `.local/share/devin/cli/sessions.db`. Its shape
follows the reference corpus
([notes/local-search/README.md §2.1](../../notes/local-search/README.md))
at 1.5GB instead of 18GB:

| | Reference corpus | Synthetic, 1.5GB, seed 1 |
|---|---|---|
| Files | 13,728 | 1,006 |
| Codex / Claude bytes | 65% / 34% | 73% / 26% |
| Claude subagent files | 57% of Claude files | 57% |
| Files over 20MB | 130, 30% of bytes, largest 213MB | 15, 39% of bytes, largest 108MB |
| Line size p50 / p99 / max | 732B / 13KB / 1.2MB | 695B / 16KB / 1.2MB |
| Bytes in lines over 100KB | 77% | 70% |

The generator also writes `queries.yaml`, the query set. Each query
searches for a needle that the generator put into known messages, so the
expected hits are exact. The set covers a session id, a ranked phrase, an
exact path, repo, since and agent filters (with `max_hits`), a regex, a
subagent, a needle late in the largest file, Claude tool output, Devin and a
very common term.

Each synthetic repository's Codex sessions record their own git remote.
`--repo` treats every checkout that shares a remote as one repository, so
a remote shared by all twelve directories would make the repo filter match
all of them.

The output depends only on the generator version
(`synthcorpus.Version`) and the seed. Change the version whenever the
generated bytes change. The workflow regenerates the corpus on every run.
Generation and parse verification take less time than restoring a 1.5GB
cache, and a cache would use most of the repository's cache quota.

Make the corpus locally:

```sh
go build -o /tmp/flopwire ./cmd/flopwire
/tmp/flopwire bench corpus --out /tmp/synth --size 1.5GB --verify
/tmp/flopwire bench acceptance --scratch /tmp/synth-acc --home /tmp/synth --queries /tmp/synth/queries.yaml
```

Run the A/B comparison locally with two binaries:

```sh
/tmp/flopwire bench ab --a /tmp/flopwire-old --b /tmp/flopwire --home /tmp/synth \
  --scratch /tmp/synth-ab --out /tmp/synth-ab-out --runs 3
```

A baseline binary must accept the commands and flags that the harness
calls: `agent run`, `grep`, `search` and `read`. If a change renames one
of them, the baseline cannot run. The verdict is then `BASELINE_FAILED`,
not `REGRESSED`: nothing was compared, the summary shows A's error, and
the run opens an issue labelled `perf-baseline-broken`. Move the pin
(see [Accept a regression](#accept-a-regression)) to the commit that
changed the flag, and compare that commit with the old baseline by hand
first so the change does not hide a regression.

## Local acceptance records

Local acceptance is optional. The release gate covers drift on the
synthetic corpus. Make a record when a release changes the parsers,
indexing or redaction, since the real corpus has shapes that the synthetic
one does not.

### File name

`<version>-<YYYY-MM-DD>.json`, for example `v0.4.0-2026-10-01.json`. The
version is the release tag. The date is the UTC date of the run. `flopwire
bench acceptance --json` prints the name when it writes a record. A
baseline recorded before a release tag exists uses `main-<short sha>` as
the version, for example `main-9e4193d-2026-10-01.json`.

### Make a record

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

4. Commit the record in the release PR, or in a PR before it.
5. Paste the comparison table into the release PR description.

### Compare two records

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

### Format

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
| `idle.rss` | MB | 120 | 16 | Anonymous memory of the agent after 60 s idle on the built index: `RssAnon` on Linux, the physical footprint on macOS |
| `sweep.cpu_max` | ms | 1000 | 100 | Slowest no-change sweep CPU time |
| `fresh.p95` | ms | 2000 | 20 | p95 time until an appended line is findable |
| `query.<name>.warm` | ms | 200 | 20 | Warm CLI latency of each query in `testdata/acceptance/queries.yaml` |

`idle.rss` leaves out file-backed pages (the binary and the index files
the agent read), which come and go with the page cache: total RSS showed
a 12% change between two builds whose own memory was the same. Records
from before 2026-10-02 hold total RSS under this name, so a comparison
with one of them shows a drop that is not real. The bench's
raw results (`<scratch>/acceptance.json`) still hold total RSS as `idle_rss_mb`.

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
