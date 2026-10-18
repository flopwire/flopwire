# Pilot results: 2026-10-13

Record of 2026-10-13. The raw result files (JSON and gzipped records) and the
runner scripts were not retained; the tables below are the complete summary.

The pilot validates exact retrieval and local message transport. It does
not establish a product winner. Every applicable retrieval query returned
the two expected sessions in all 30 measured runs. Both messaging products
passed all 30 sequential delivery checks. Both recovered an unread message
after a graceful service restart using separate recovery identities.

See the [run instructions](competitive-benchmark-run-2026-10-13.md) to reproduce the pilot and the
[specification](competitive-benchmark-spec-2026-10-13.md) for the larger comparison.
The runner scripts were not retained. Gas Town is excluded.

## Retrieval

Cells show p50 / p95 milliseconds. The percentile estimate is exploratory
with 30 samples. Every supported cell passed 30/30 attribution checks;
there were no measured command failures, missing sessions or extra sessions.

| Case | Flopwire CLI | AgentsView CLI | Langfuse HTTP API | `rg` CLI baseline |
|---|---:|---:|---:|---:|
| Rare identifier | 30 / 44 | 80 / 157 | 19 / 32 | 11 / 14 |
| Exact path | 29 / 54 | 81 / 165 | 19 / 31 | 13 / 15 |
| Tool output | 28 / 35 | 80 / 122 | 17 / 31 | 12 / 15 |
| Unicode identifier | 28 / 53 | 81 / 114 | 16 / 36 | 12 / 31 |
| Literal phrase | 28 / 70 | 86 / 165 | 18 / 28 | 11 / 19 |
| Regex | 29 / 59 | 78 / 122 | Unsupported by the tested token-search API | 12 / 15 |

<details>
<summary>Retrieval interfaces and indexing times</summary>

The interfaces perform different work. Flopwire and AgentsView timings
include a fresh CLI process and formatted output. Langfuse timings use
direct HTTP requests to a persistent service in Docker. AgentsView uses
its default literal scan, with semantic indexing disabled; Flopwire uses
literal or regex `grep`; Langfuse uses its `matches` token/phrase operator.
This is an interface comparison, not a comparison of database query speed.

Initial Flopwire indexing took 311ms. AgentsView's one-shot sync command
took 686ms, including 559ms reported for sync. Langfuse accepted the OTLP
batches in 1,068ms; that measures HTTP ingestion, not time until every span
was queryable. Docker image downloads and service initialization are
excluded from those values.

`rg` was fastest on this small corpus. The next comparison must test larger history and the effort
required to obtain readable context and provenance. This pilot provides
no evidence of an indexing advantage at scale.

</details>

## Messaging

Both products used fresh local state and synthetic identities. CLI
invocations were randomized between products for each of 30 iterations.
The recipient was retrieved immediately after the sender returned.

| Measurement | Flopwire p50 / p95 ms | Agent Mail p50 / p95 ms |
|---|---:|---:|
| Send acceptance | 23 / 74 | 36 / 116 |
| Recipient retrieval command | 22 / 42 | 14 / 19 |
| Send through recipient hook output | 50 / 96 | Not measured |
| Send through recipient inbox retrieval | Not measured | 51 / 130 |
| Exact ID and body checks | 30/30 | 30/30 |
| Graceful restart recovery | Pass, one unread-message case | Pass, one unread-message case |

Flopwire's recipient hook printed the wrapper and confirmed delivery. An
immediate second hook printed no duplicate ID. Agent Mail's inbox returned
the expected ID and full body; the client then acknowledged it. These are
different recipient interfaces. No model ran, so the experiment says
nothing about when an agent reads, understands or replies to a message.
Agent Mail's duplicate check covers one inbox response; it does not claim
that an acknowledged message disappears from later history queries.

During initial runner qualification, an extra recovery message after 30
sends was refused by Flopwire with `session_rate`: its default limit is
30 sends per hour per session. The qualification record, not retained,
held that refusal. The final current-main run uses separate synthetic identities for
recovery and passes both cases without changing the product's limit.

The starting local checkout was behind `origin/main`. Those earlier
records are described under [Earlier qualification records](#earlier-qualification-records).
The tables above use a complete rerun against current main. Timing changes
between the two runs also reflect changing laptop load; they are not an
A/B measurement of the intervening Flopwire fixes.

## Next experiment

Test larger history, relevance and citation quality, append freshness, and
real-harness message visibility. Compare equivalent CLI, HTTP or MCP paths.
Measure import effort and verified agent outcomes. Keep realistic send rates
and distinguish rate-limit refusals from delivery failures.

<details>
<summary>Machine, versions and corpus</summary>

## Setup

| Item | Configuration |
|---|---|
| Machine | Apple M1 Pro, 16GiB RAM, macOS arm64; shared development laptop |
| Flopwire | Current main `f83db684e912ea77c08ac02264996f55cf1f4993`, built at benchmark commit `5c60e900b3c67f7870f360489f65fa9cfa8ab10f` |
| AgentsView | Released `v0.45.0`, commit `b9d0a207f51ff27b84adbaae2a91b344247079b3` |
| Langfuse | Self-hosted `4.58.0`; six-service Docker Compose stack; Docker VM has 8GiB RAM |
| MCP Agent Mail | Released Rust CLI/server `v0.3.38` |
| Corpus | 48 sessions: 24 Claude, 24 Codex; 960 canonical text messages; about 2.5MB native JSONL |
| Query repetitions | One warm-up, then 30 measured calls per case; main product/query order randomized with seed 1 |
| Baseline | `rg` over the same generated native JSONL, in the same randomized schedule |

Flopwire and AgentsView ingested native harness files. Langfuse received
the same canonical message text as OTLP observation outputs, with session
and source-message IDs on every span. This custom adapter tests content
storage and retrieval. It does not test Langfuse's normal coding-agent
capture integration or historical transcript import effort.

Count checks found 48 Flopwire conversations and 1,008 message rows, which
include the 48 additional tool calls. AgentsView stored 48 sessions and
960 message rows. Langfuse returned 960 observations across 48 sessions;
the source-message IDs exactly matched the canonical set. These checks
validate counts and identity coverage, not complete parser fidelity.

Each query has a marker in one Claude session and one Codex session. The
remaining sessions provide distractors. A separate absent-marker check
returned no results from all three products.

</details>

## Evidence and reproduction

The result files were not retained in the repository. As produced, they were:
`metadata.json` (image digests, source commits and binary hashes),
`corpus-labels.json` (ground truth and source hashes),
`capture-validation.json` (the count checks) and `messaging-summary.json`
(all 60 measured transport samples), plus three raw records:

- `retrieval.json.gz`: 690 measured samples, 23 warm-ups and one unsupported
  case, including the `rg` baseline.
- `messaging.json.gz`: 60 measured samples and both successful recovery cases.
- `restart-recovery.json`: the two unread-message cases extracted from the
  messaging record.

Follow the [runner instructions](competitive-benchmark-run-2026-10-13.md) to
repeat the experiment with fresh state. The runner scripts were not retained.

All temporary services were stopped after validation. The local scratch
directory retains source corpora, logs and isolated test credentials for
inspection. No real transcripts were uploaded or searched, and no live
agent sessions were contacted.

## Earlier qualification records

These records qualified the runners before the final current-main run.
Flopwire was built from the initial local checkout at
`8023e172ecbd7e794e732a3c57b62b4c4b994797`. That checkout was behind
`origin/main`. The dated report's primary tables use the later complete
rerun against main `f83db684e912ea77c08ac02264996f55cf1f4993`.

The earlier retrieval run measured the three products together and `rg`
separately. All applicable exact queries passed. Its extra recovery send
after 30 messages hit Flopwire's default hourly session allowance. That
failure was recorded in `messaging.json.gz`, which was not retained. The subsequent isolated recovery
run passed for both products.

The raw records, not retained, held the earlier measurements and their
limits. They were not a controlled A/B comparison with the current-main records.
