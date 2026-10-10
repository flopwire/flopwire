# Release checklist

CI runs `go test -race ./...`, the web tests, the build and the perf
release gate (section 5). The other checks on this page need real
transcripts, real agent CLIs or two machines, so CI cannot run them. Run
every check on a release candidate before you tag it, except the ones
marked optional. Record the measured numbers in the release PR. See [Releases](releases.md)
for the release-please workflow and bot setup. The [v0.1.0 qualification
record](releases/v0.1.0-readiness.md) separates observed evidence from pending
gates and owner-deferred scope.

All corpus checks read the harness directories (`~/.claude`, `~/.codex`,
`~/.local/share/devin`) read-only. Their output can quote real transcript
text. Do not commit it.

## 1. Server test environment

The corpus tests for sync, ingest and retrieval need Postgres and MinIO.

1. Start the development stack:

   ```sh
   docker compose -f compose.yaml -f compose.dev.yaml up -d postgres minio
   ```

2. Export the test variables. Use the passwords from `.env`:

   ```sh
   export FLOPWIRE_TEST_DATABASE_URL='postgres://flopwire:<password>@127.0.0.1:55432/flopwire?sslmode=disable'
   export FLOPWIRE_TEST_S3_ENDPOINT=127.0.0.1:59000
   export FLOPWIRE_TEST_S3_ACCESS_KEY=flopwire
   export FLOPWIRE_TEST_S3_SECRET_KEY='<minio password>'
   ```

## 2. Parser corpus checks

Each check parses every transcript of one harness on this machine.

| Check | Command | Bar |
|---|---|---|
| Claude parse and verify | `FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/claude` | No parse errors. `TestCorpusVerify` passes: every row's byte range holds its line, and a mid-file resume matches a full parse. |
| Codex parse and resume | `FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/codex` | No parse errors. `TestCorpusResume` passes. |
| Devin parse | `FLOPWIRE_CORPUS=1 go test -run Corpus -v ./internal/transcript/devin` | No parse errors. The test copies `sessions.db` first. |

## 3. Local index and agent corpus checks

| Check | Command | Bar |
|---|---|---|
| Full local index | `FLOPWIRE_CORPUS=1 FLOPWIRE_CORPUS_DB=/tmp/flopwire-corpus/index.db go test -run TestCorpus -timeout 3h -v ./internal/localindex/` | Passes. Note rows, time and index size. |
| Local retrieval over that index | `FLOPWIRE_CORPUS=1 FLOPWIRE_CORPUS_DB=/tmp/flopwire-corpus/index.db go test -run TestCorpus -v ./internal/retrieval/local/` | Passes. |
| Append latency | `FLOPWIRE_CORPUS=1 go test -run TestCorpusAppendLatency -v ./internal/agent/` | An appended line is findable in about 2 seconds. |
| Placement | `FLOPWIRE_CORPUS=1 go test -run TestCorpusPlacement -v ./internal/agent/` | Passes. Set `FLOPWIRE_CORPUS_VERBOSE=1` to list sessions not placed by `cwd` or `remote`. |

## 4. Sync, ingest and server retrieval corpus checks

These need section 1.

| Check | Command | Bar |
|---|---|---|
| Chunking dry run | `FLOPWIRE_CORPUS=1 go test -run Corpus ./internal/devicesync` | Passes. |
| Ingest | `FLOPWIRE_CORPUS=1 go test -run Corpus -timeout 2h ./internal/ingest` | Passes with 0 parse failures. `FLOPWIRE_CORPUS_DAYS` sets the window (default 7). |
| Ingest with server path rules | `FLOPWIRE_CORPUS=1 FLOPWIRE_CORPUS_DAYS=1 FLOPWIRE_CORPUS_RULES='deny ~/Code/<a repo>*' FLOPWIRE_CORPUS_UNPLACEABLE=exclude go test -run Corpus -timeout 2h ./internal/ingest` | 0 stored conversations covered by the rules. 0 parse failures. |
| Server retrieval latency | `FLOPWIRE_CORPUS=1 go test -run Corpus -timeout 2h ./internal/retrieval` | Passes. Note the slowest queries. |

## 5. Performance

### 5a. CI release gate (required)

The `perf-gate` check on the release-please PR runs the A/B bench on a
synthetic corpus ([docs/perf](perf/README.md#release-gate)). It compares
the PR head with the latest release tag, or with
`docs/perf/nightly-baseline` when that pin is newer.

1. Wait for the `perf-gate` check on the release PR.
2. Read the "Perf release gate" comment on the PR. It holds the table and
   the verdict.
3. If the verdict is `CLEAN`, go to the next section.
4. If the verdict is `REGRESSED`, fix each regressed metric on `main`, or
   accept the regression: move the pin in a PR that explains it (see
   [Accept a regression](perf/README.md#accept-a-regression)). Then rerun
   the gate.
5. If the verdict is `BASELINE_FAILED`, the baseline cannot run under the
   new harness, and nothing was compared. Compare the two builds by hand,
   then move the pin (see [docs/perf](perf/README.md#synthetic-corpus)).
6. Read each open `perf-regression` and `perf-baseline-broken` issue from
   the nightly run. Close each one, or explain it in the release PR.

Do not tag a release while `perf-gate` fails.

### 5b. Local acceptance (optional)

The local run is a reality check on the real corpus. It is optional. Run
it when the release changes the parsers, the indexing or the redaction:
the real corpus has shapes that the synthetic corpus does not.

`scripts/acceptance.sh` builds the binary and runs `flopwire bench acceptance`
in four parts, plus the FAD parity sample. It writes a JSON record of the
run and compares it with the previous release's record. It takes 10 to 15
minutes. Stop other heavy work on the machine first; load changes the index time.

The reference laptop is the machine that made the most recent record in
[docs/perf](perf/README.md). Its `machine` fields name it. Run the set on
that machine. Timings from two machines are not comparable. If
docs/perf has no record yet, the first run sets the reference laptop.

1. Run the whole set on the reference laptop. Set the release version:

   ```sh
   FLOPWIRE_VERSION=v0.1.0 scripts/acceptance.sh /tmp/flopwire-acceptance
   ```

2. To run one part, use the bench command directly:

   ```sh
   flopwire bench acceptance --scratch /tmp/flopwire-acceptance --only index
   ```

3. Read the pass/fail table.
4. Read the comparison table after it. It compares this run with the most
   recent record in [docs/perf](perf/README.md). It flags each metric that
   grew by more than 20% and by more than its minimum change as
   `REGRESSED`.
5. If the comparison warns that the machine or the corpus differs, do not
   treat its regressions as real. Rerun the previous release on this
   machine to confirm a regression.
6. Copy `/tmp/flopwire-acceptance/acceptance-record.json` to
   `docs/perf/<version>-<YYYY-MM-DD>.json`. The run prints the name.
7. Commit the record in the release PR.
8. Paste the comparison table into the release PR description. Explain
   each `REGRESSED` metric, or fix it before the release.

When you run the set, these bars apply:

| Check | Bar in the table | Accepted today |
|---|---|---|
| a. full index wall | < 5 min | About 7.5 min on the reference laptop (10.2GB index, baseline `main-9e4193d`). Whole tool output is indexed (D3). Record the number; a large regression blocks. |
| a. full index peak RSS | < 600MB | Must pass. |
| a. idle agent after 60s | < 120MB anonymous | About 129MB footprint (176MB RSS) on the reference laptop, baseline `main-9e4193d`. Accepted. A `REGRESSED` flag from `bench compare` blocks. The `main-9e4193d` record holds total RSS under `idle.rss`, so a comparison with it shows a drop of about 27% that is not real (docs/perf/README.md). |
| a. no-change sweep CPU | < 1s | Must pass. |
| b. live line findable | p95 < 2s | Must pass. |
| c. FAD 0.3.1 parity sample | 0 parse errors; mismatches documented | Must pass. Needs `cargo` for `tools/fad-dump`. Set `AGENTSVIEW_SRC` for the second oracle. |
| d. query set expected hits | all | Must pass. |
| d. query set latency (warm) | < 200ms each | 8 of 28 run over, max about 351ms (`error-common-term`), baseline `main-9e4193d`. Accepted. A query flagged `REGRESSED` by `bench compare` blocks. |
| d. hit addresses round-trip through `read` | all | Must pass. |

## 6. Two real agent sessions

Check that a real agent can use the MCP tools.

1. Register the MCP server for both CLIs. See
   [search.md](search.md#register-the-mcp-server).
2. Run a Claude Code session:

   ```sh
   claude -p 'Use flopwire_grep to find the last session that mentions "backoff", then flopwire_read the first hit. Report the address you read.'
   ```

3. Run a Codex session:

   ```sh
   codex exec 'Use flopwire_grep to find the last session that mentions "backoff", then flopwire_read the first hit. Report the address you read.'
   ```

Bar, for each session:

- The agent calls `flopwire_grep` and `flopwire_read` with no tool error.
- The `read` output shows the text at the address the agent names.
- The grep footer names the calling session as left out (self-exclusion).

## 7. Two devices with real data

`scripts/e2e-sync.sh` runs the two-device setup on one machine against a
Compose server with TLS pinning. CI runs it without real data.

1. Start Docker Desktop.
2. Run the test with a copy of your 25 newest sessions per harness:

   ```sh
   FLOPWIRE_E2E_CORPUS=25 scripts/e2e-sync.sh
   ```

3. Read the scenario table at the end.

Bar: every scenario passes.

Then run the real two-laptop setup once:

1. Follow [two-laptop.md](two-laptop.md) on two machines.
2. Complete every step of its "Verify" section.
3. Stop the server for one minute, work in a session on laptop B, and
   start the server again.

Bar: every verify step passes, and laptop B's new lines reach the server
within 30 seconds of the restart.
