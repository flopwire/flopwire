# Notes

Design records, decisions and research. User-facing docs live in
[`../docs`](../docs) and the top-level README. Where a note and the code
disagree, the code wins.

| Note | What it holds |
|---|---|
| [`local-search/README.md`](local-search/README.md) | The retrieval spec: corpus evidence, capture, data model, parsers, device agent, sync, server, search, operations and the acceptance bar. The rationale behind the architecture. |
| [`local-search/ftsidx.py`](local-search/ftsidx.py) | Prototype FTS5 sidecar used to measure the local index before the Go build. |
| [`local-search/shape.py`](local-search/shape.py) | Byte accounting over sampled transcripts, used to size the text caps. |
| [`message-bus/plan.md`](message-bus/plan.md) | The v1 message bus design: decisions B1–B8 and accepted risks, why hooks and not the Claude socket or `codex queue` (verified, not used), the agent interface and syntax, hook delivery, per-harness packaging and vendor cloud. |
| [`message-bus/cc_send.py`](message-bus/cc_send.py), [`message-bus/codex_rpc.py`](message-bus/codex_rpc.py) | Probe scripts for the Claude inbox socket and the Codex daemon (plan section 2). |
| [`agent-ux.md`](agent-ux.md) | The agent UX review of the retrieval tools and MCP server (2026-09-30), with later dated evaluations such as read's `messages_before`/`messages_after`. |
| [`agent-workflow-comparison.md`](agent-workflow-comparison.md) | Comparison of Flopwire with AgentsView, MCP Agent Mail Rust, Entire, SpecStory and Langfuse: the connected-workflow table, the feature table, workflow pass criteria, and Flopwire's behaviour for those workflows. |
| [`excerpt-authority.md`](excerpt-authority.md) | The contract for transcript excerpts attached to messages delivered through device hooks: who resolves a reference and what a failed read yields. |
| [`redaction.md`](redaction.md) | What is redacted, where, and how masks keep byte addresses stable. |
| [`reparse.md`](reparse.md) | Versioned reparse on the server: parser versions, staleness and the background worker. |
| [`parallel-sync-design.md`](parallel-sync-design.md) | Why two upload workers overlap only network waits: per-path ownership, server admission, capability negotiation and pressure epochs. |
| [`perf-guards.md`](perf-guards.md) | The CI performance guards: query plans, scale and query-count tests on PRs, the A/B bench gate on releases and nightly. |
| [`reference-repos.md`](reference-repos.md) | Repositories studied or ported from, with licenses and what was taken. |
| [`log/`](log/) | Dated records: rollouts, qualification runs, recovery plans, probes, audits and perf experiments. Each file records one event and is never edited, except [`log/probe-runs.md`](log/probe-runs.md), which `flopwire probe --notes` appends to. |

Dated records in `log/` that other files cite:

| Log | What it holds |
|---|---|
| [`log/punch-list-2026-09-30.md`](log/punch-list-2026-09-30.md) | Pre-landing decisions D1–D21, the verified bug list, the wave plan and its outcomes for the retrieval rebuild (#111). |
| [`log/probes-2026-10-01.md`](log/probes-2026-10-01.md) | Live tests behind the message-bus plan: hook delivery on Claude Code, Codex and Devin, the opencode plugin, Claude cloud and Devin cloud. |
| [`log/cloud-2026-10-03.md`](log/cloud-2026-10-03.md) | Vendor cloud delivery (#63): the Claude and Devin request and response shapes the adapters use, sending out of a Devin cloud session, and the live checks of discovery, queueing and push delivery. |
| [`log/hook-caps-2026-10-03.md`](log/hook-caps-2026-10-03.md) | Measured hook payload caps per harness, behind the delivery size limits in `busproto` and `busrender`. |
| [`log/field-observation-2026-10-05.md`](log/field-observation-2026-10-05.md) | Review of a real 29 h Codex session's Flopwire use, corrected 2026-10-06 against the code and `bus.db`. |
| [`log/local-remote-audit/`](log/local-remote-audit/README.md) | Local and remote boundary audit at revision `883a467`, 2026-10-14, with its replay script and repros. |
