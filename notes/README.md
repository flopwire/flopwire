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
| [`message-bus/README.md`](message-bus/README.md) | Verified ways to deliver a message into a running Claude Code or Codex session (spec section 9). Not built yet. |
| [`message-bus/plan.md`](message-bus/plan.md) | The v1 message bus design and build plan: decisions B1–B8, the agent interface and syntax, hook delivery, per-harness packaging, build sequence. Supersedes spec section 9. Not built yet. |
| [`message-bus/probes-2026-10-01.md`](message-bus/probes-2026-10-01.md) | Live tests behind the plan: hook delivery on Claude Code, Codex and Devin, the opencode plugin, Claude cloud and Devin cloud. |
| [`message-bus/exchange-capture/README.md`](message-bus/exchange-capture/README.md) | A real Claude Code ↔ Codex exchange (own user, local-only): `sessions`, `peers`, `send`, both hook deliveries, the reply and `inbox`, captured for the homepage fixtures (#55), with timeline, findings and the substitutions made. |
| [`message-bus/cc_send.py`](message-bus/cc_send.py), [`message-bus/codex_rpc.py`](message-bus/codex_rpc.py) | The probe scripts behind the message-bus note. |
| [`launch-readiness/README.md`](launch-readiness/README.md) | Where the CASS-era launch-gate work landed on the fresh schema. |
| [`launch-readiness/punch-list.md`](launch-readiness/punch-list.md) | The P0/P1/P2 launch gate carried over from the CASS era. The spec and `punch-list.md` override it. |
| [`launch-readiness/state-machine-walks.md`](launch-readiness/state-machine-walks.md) | Concrete-state walks for deletion, backup and restore, quotas and collection. |
| [`reference-repos.md`](reference-repos.md) | Repositories studied or ported from, with licenses and what was taken. |
| [`mvp_scope.md`](mvp_scope.md) | Historical: the CASS-era MVP scope. The architecture it describes is gone; the product boundaries (one org, invited accounts, raw retention) still hold. |
