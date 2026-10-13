# Competitive benchmark

## Decision and scope

Use this benchmark to decide which Flopwire improvements help developers
recover previous work and coordinate existing coding sessions. The first
pilot qualifies the measurement methods. A later experiment measures agent
task outcomes. Publish comparisons only after repeated, comparable runs.

Gas Town is excluded at the user's request.

The initial pilot includes Flopwire, MCP Agent Mail, AgentsView and Langfuse.
Entire, SpecStory, CASS, LangSmith and Braintrust remain candidates for the
next round. Compare products within the workflows they support. A missing
workflow is a capability gap; it is not a failed query or a zero score.

## Candidate qualification

Record the release, deployment mode, license, capture requirements, API
access, maintenance and usage evidence for every candidate. Keep stars,
vendor usage claims and independent deployment evidence in separate fields.
Stars and funding establish attention and backing; they do not measure
active usage or technical performance.

| Candidate | Relevant workflow | Evidence and documentation |
|---|---|---|
| MCP Agent Mail | Discover agents, exchange messages, retrieve mail history, reserve files | [Rust release and source](https://github.com/Dicklesworthstone/mcp_agent_mail_rust); [product](https://www.mcpagentmail.com/) |
| AgentsView | Capture existing coding transcripts, search messages and tools, expose history to agents | [Product](https://www.agentsview.io/); [configuration](https://www.agentsview.io/docs/configuration/) |
| Entire | Connect commits to sessions and recover development intent | [Product](https://entire.io/); [company announcement](https://entire.io/blog/hello-entire-world) |
| SpecStory | Capture coding sessions and search synced history | [Cloud documentation](https://docs.specstory.com/cloud/overview) |
| CASS | Search local and remotely collected coding sessions | [Source and releases](https://github.com/Dicklesworthstone/coding_agent_session_search) |
| Langfuse | Store and query observations and conversation sessions | [Full-text search](https://langfuse.com/docs/observability/features/full-text-search); [coding-agent capture](https://langfuse.com/coding-agents); [adopter references](https://langfuse.com/users) |
| LangSmith | Store, inspect, query and evaluate agent traces | [Observability](https://www.langchain.com/langsmith/observability); [customer evidence](https://www.langchain.com/customers); [CLI](https://github.com/langchain-ai/langsmith-cli) |
| Braintrust | Store and search traces, inspect tool spans, create evaluations | [Tracing](https://www.braintrust.dev/learn/tracing/v0); [customer evidence](https://www.braintrust.dev/customers) |

These references were checked on 2026-10-13. Advertised features remain
unverified until exercised in the selected release. Phoenix is a reserve
candidate for an additional self-hosted trace comparison.

## Three experiments

### 1. Retrieval and capture

Run two tracks. In the content track, each product receives the same
canonical text with session and source identifiers. In the capture track,
each product uses its normal integration with a coding tool. Report any
adapter required to import historical transcripts. Record conversion time
separately from index time and query time.

The pilot uses a deterministic small corpus with known answers. Include
Claude and Codex messages, tool output, exact paths, punctuation, Unicode,
multiline text and irrelevant sessions. Labels come from corpus generation,
independently of any product parser. Keep the labels outside searchable
content. Persist source hashes and counts.

| Metric | Definition |
|---|---|
| Session recall | Distinct relevant sessions returned / distinct relevant sessions in the labels |
| Session precision | Distinct relevant sessions returned / distinct sessions returned |
| Evidence recall | Relevant source messages returned / relevant source messages in the labels |
| Ranking quality | Recall@5, reciprocal rank and nDCG@10 over human-labeled decision queries |
| Citation accuracy | Citations that resolve to the claimed source and support the answer / citations checked |
| Capture fidelity | Expected source content and relationships retained, by message kind and format |
| Freshness | Time from a completed source append to the first query that finds its marker |
| Latency | End-to-end query p50 and p95, including CLI startup or HTTP transport as identified |
| Resources | Initial index wall time, peak memory, steady memory, idle CPU, index bytes, bytes transferred |

Pilot needle queries measure exact retrieval, not semantic relevance. Use
rare identifiers and paths alongside decision questions in the larger run.
Examples: "Why did we add retry jitter?", "Which session changed the
pagination contract?", and "What failed before the final fix?"

Run a second scale tier with the existing deterministic
`flopwire bench corpus` generator. Use 1.5GB and a larger corpus if hardware
permits. The small pilot cannot substantiate scale claims.

### 2. Communication

Compare Flopwire and MCP Agent Mail with isolated test identities. Use
manual copy/paste as a human-effort baseline in the later agent experiment.
Measure the CLI, MCP and hook paths separately when they differ.

| Metric | Definition |
|---|---|
| Acceptance latency | Sender invocation to a successful receipt |
| Inbox visibility | Sender invocation to recipient retrieval of the exact message ID and body |
| Hook output latency | Sender invocation to message output by the recipient hook |
| Model visibility | Sender invocation to the harness recording the message in model input |
| Reply latency | Sender invocation to a correct recipient reply |
| Delivery correctness | Missing, duplicate, corrupt and misrouted messages / messages sent |
| Restart recovery | Exact expected messages retrievable after service restart |
| Discovery correctness | Expected active sessions found, stale sessions shown and wrong owners selected |

Inbox retrieval and hook output provide transport evidence. Neither proves
model visibility. Measure model visibility in a real harness transcript.
Keep an accepted receipt, a durable message and an agent reply separate.

Start with 30 sequential messages. The larger run uses 1, 10 and 30
concurrent senders. Inject receiver restart, sender restart, network loss,
unconfirmed delivery and an idle recipient. State whether the product
wakes idle sessions. Record polling frequency and retries; they directly
affect latency and resource use.

### 3. Agent task outcomes

Use the same model version, reasoning setting, repository snapshot,
instructions, task budget and agent count within each comparison. Add
cross-harness trials as a separate experiment. Product-specific integration
instructions should explain the interface without revealing task answers.

| Task | Withheld information | Verification |
|---|---|---|
| Recover a decision | Previous session explains why retries need jitter | Correct implementation and supporting source citation |
| Hand off a change | A prior agent left a partial fix and a failed approach | Hidden tests pass; failed approach is not repeated |
| Communicate an API change | Backend changes cursor pagination while frontend works | Contract tests pass without human relaying the change |
| Reconcile overlapping work | Two agents need the same files | Integrated result passes tests; discarded work is counted |
| Find and ask the owner | Commit and history identify the session responsible | Correct owner contacted; answer used in a passing dependent change |

Record verified success, elapsed time, human interventions, tool calls,
input/output/cached tokens, model cost, messages, duplicate edits and merge
failures. Cost per successful task includes spending on failed attempts.
Separate model inference, product charges and infrastructure costs.

Use hidden deterministic tests for code outcomes. Review rationale and
citation quality blind to the product. An LLM judge may assist only after
calibration against human ratings. The pilot does not run this experiment.

## Fair measurement

1. Pin product releases and record binary checksums or container digests.
2. Start each product with fresh data and isolated configuration.
3. Record the machine, OS, architecture and deployment boundary.
4. Validate imported sessions and content before measuring queries.
5. Run correctness checks on every response, including measured runs.
6. Warm each query once. Keep that sample separate.
7. Randomize product and query order with a recorded seed.
8. Retain every measured sample and command failure.
9. Stop only the processes started by this experiment.

Use 30 repetitions per query for the pilot. Label p95 exploratory at this
sample size. For the larger run, use at least 100 samples and repeat the
whole experiment on three fresh indexes. For agent outcomes, start with
10 tasks and 5 repeats per product; report task-level paired differences
and uncertainty. Choose a larger sample after observing variance.

Report warm-process and fresh-process retrieval separately. A first query
is not a verified cold-cache query. Do not flush the user's filesystem
cache. Use a dedicated runner for controlled cold-cache measurements.

Do not rank a native macOS CLI against a service in Docker Desktop by
server speed. Label both as end-to-end local deployment measurements.
If a common query can be expressed only with different semantics, describe
the difference and retain a separate capability-specific result.

Set generous result limits for exact recall queries. Ranking queries use a
fixed top-k. Count distinct sessions and message identities; duplicate hits
must not inflate recall. Keep substring, token, phrase, regex and semantic
queries in separate categories. A regex unsupported by a trace API is
`unsupported`, not a false negative.

## Pilot deliverables

- A reproducible synthetic corpus and independent labels.
- Version, checksum and deployment records.
- Exact commands and per-invocation timings.
- Retrieval correctness and messaging transport evidence.
- A dated results note that states failures and unmeasured capabilities.
- A follow-up plan for realistic history and real-agent trials.

Extend the existing [performance framework](../../docs/perf/README.md)
where its corpus or checks fit. Keep competitor-specific import and query
adapters outside Flopwire's production code. The pilot must not mutate
the user's existing indexes or message network.
