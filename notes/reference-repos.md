# Reference repositories

Repos studied while building the retrieval redesign. None is vendored. Clone one into `~/Code` when you need it again. Ported code carries a header with the source path and pinned commit, and upstream notices live in `third_party/<name>/`.

| Repo | License | Language | Used for |
|---|---|---|---|
| [kenn-io/agentsview](https://github.com/kenn-io/agentsview) | MIT | Go | Ported: line reader, Claude subagent and persisted tool-result resolution, Codex call_id pairing, Devin main-chain walk. Its binary is the second parity oracle. Not vendored: its model is lossy (merges lines by message.id, drops fork branches). |
| [entireio/cli](https://github.com/entireio/cli) | MIT | Go | Ported: Claude token dedupe by message.id, apply_patch path extraction, Codex compacted sanitising. Fixtures. |
| [codecast-sh/codecast](https://github.com/codecast-sh/codecast) | MIT | TypeScript | Codex subagent edge cases: thread_spawn vs codex-exec review children vs forks. |
| [google/codesearch](https://github.com/google/codesearch) | BSD-3 | Go | Ported: `index/regexp.go` RegexpQuery, the regex→trigram planner behind `find`/`grep` (`internal/retrieval/regexq`). |
| [gitleaks/gitleaks](https://github.com/gitleaks/gitleaks) | MIT | Go | Ported: vendor secret token shapes for `internal/redact` (notes/redaction.md). agentsview `internal/secrets` and entireio/cli `redact` were surveyed for the same. |
| [specstoryai/getspecstory](https://github.com/specstoryai/getspecstory) | Apache-2.0 | Go | Reference for format coverage: 15 providers (Claude, Codex, Gemini, Cursor, Copilot, OpenCode, Pi, Qwen, Droid, Grok and others) in `specstory-cli/pkg/providers/*`. The widest Go coverage; use it for wave 2 harnesses. Apache needs a NOTICE if code is ported. |
| [neilberkman/ccrider](https://github.com/neilberkman/ccrider) | MIT | Go | Reference: small per-format parsers (Claude, Codex, Copilot, OpenCode, Pi, Amp, Antigravity) in `pkg/*sessions`; keeps parentUuid and isSidechain; modernc SQLite with FTS5. |
| [sourcegraph/zoekt](https://github.com/sourcegraph/zoekt) | Apache-2.0 | Go | Reference for query budgets (`MaxWallTime`, flush reasons) behind decision D8. |
| [nicosuave/memex](https://github.com/nicosuave/memex) | — | — | Surveyed during the search UX research; nothing taken. |
| [ConfabulousDev/confab](https://github.com/ConfabulousDev/confab) | — | — | Surveyed during the search UX research; nothing taken. |

Not to copy from: CASS and mcp_agent_mail (license rider), and anything AGPL (trufflehog, claude-squad, grafana loki). FAD (franken-agent-detection 0.3.1, MIT) is a test-time parity oracle only; see `tools/fad-dump`.
