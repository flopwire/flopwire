# Notes

Design records, decisions and research. User-facing docs live in
[`../docs`](../docs) and the top-level README. Where a note and the code
disagree, the code and `punch-list.md` win.

| Note | What it holds |
|---|---|
| [`agent-workflow.md`](agent-workflow.md) | How builder and reviewer agents were run for the rebuild: worktrees, review-then-fix, final pass, rules. |
| [`punch-list.md`](punch-list.md) | Pre-landing decisions D1–D21, the verified bug list, the wave plan and its outcomes, and the work left before the first team. The newest decision record. |
| [`local-search/README.md`](local-search/README.md) | The retrieval redesign spec: capture, data model, device agent, sync, server, search, operations. The rationale behind the architecture. |
| [`local-search/ftsidx.py`](local-search/ftsidx.py) | Prototype FTS5 sidecar used to measure the local index before the Go build. |
| [`local-search/shape.py`](local-search/shape.py) | Byte accounting over sampled transcripts, used to size the text caps. |
| [`message-bus/README.md`](message-bus/README.md) | Ways to push a message into a running Claude Code or Codex session (inbox socket, `codex queue`), verified 2026-09-28. Not used in v1, which delivers through hooks; kept for a later opt-in wake. |
| [`message-bus/plan.md`](message-bus/plan.md) | The v1 message bus design and build plan: decisions B1–B8, the agent interface and syntax, hook delivery, per-harness packaging, build sequence with PR numbers, and an "As built" section (2026-10-03) on where the build diverged. Built except opencode. Supersedes spec section 9. |
| [`message-bus/probes-2026-10-01.md`](message-bus/probes-2026-10-01.md) | Live tests behind the plan: hook delivery on Claude Code, Codex and Devin, the opencode plugin, Claude cloud and Devin cloud. |
| [`message-bus/cloud-2026-10-03.md`](message-bus/cloud-2026-10-03.md) | Vendor cloud delivery (#63): the Claude and Devin request and response shapes the adapters use, sending out of a Devin cloud session, and the live checks of discovery, queueing and push delivery. |
| [`message-bus/exchange-capture/README.md`](message-bus/exchange-capture/README.md) | A real Claude Code ↔ Codex exchange (own user, local-only): `sessions`, `peers`, `send`, both hook deliveries, the reply and `inbox`, captured for the homepage fixtures (#55), with timeline, findings and the substitutions made. Findings 1 (quiet commits) and 2 (`--repo` across worktrees) were fixed later in #103 and #100. |
| [`message-bus/field-observation-2026-10-05.md`](message-bus/field-observation-2026-10-05.md) | Review of a real 29 h Codex session's Flopwire use, corrected 2026-10-06 against the code and `bus.db`: two working coordination exchanges; the `peers`/`agent.sock` split; the `flopwire/flopwire` path misread (#162); the repo-filtered grep timeout's real cause (the trigram scan runs before the repo predicate); an inform to an idle session that was never read, and the sender told nothing; the 1 h `LiveCap` drop for open idle sessions. |
| [`message-bus/cc_send.py`](message-bus/cc_send.py), [`message-bus/codex_rpc.py`](message-bus/codex_rpc.py) | The probe scripts behind the message-bus note. |
| [`launch-readiness/README.md`](launch-readiness/README.md) | Where the CASS-era launch-gate work landed on the fresh schema. |
| [`launch-readiness/punch-list.md`](launch-readiness/punch-list.md) | The P0/P1/P2 launch gate carried over from the CASS era. The spec and `punch-list.md` override it. |
| [`launch-readiness/state-machine-walks.md`](launch-readiness/state-machine-walks.md) | Concrete-state walks for deletion, backup and restore, quotas and collection. |
| [`agent-ux.md`](agent-ux.md) | The agent UX review of the retrieval tools and MCP server (2026-09-30), with later dated evaluations such as read's `messages_before`/`messages_after`. |
| [`redaction.md`](redaction.md) | What is redacted, where, and how masks keep byte addresses stable. |
| [`reparse.md`](reparse.md) | Versioned reparse on the server: parser versions, staleness and the background worker. |
| [`perf-guards.md`](perf-guards.md) | The CI performance guards decided 2026-09-30: query plans, scale and query-count tests. |
| [`reference-repos.md`](reference-repos.md) | Repositories studied or ported from, with licenses and what was taken. |
| [`mvp_scope.md`](mvp_scope.md) | Historical: the CASS-era MVP scope. The architecture it describes is gone; the product boundaries (one org, invited accounts, raw retention) still hold. |
