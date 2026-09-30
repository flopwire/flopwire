# Parser parity oracles

Test-time reference output for the transcript parsers in `internal/transcript`.
Every fixture here is synthetic. Never copy real transcript content into this
tree.

## Layout

- `home/`: a fake `$HOME` in each harness's native layout
  (`.claude/projects/...`, `.codex/sessions/...`). Parsers and oracles read the
  same tree.
- `seeds/*.sql`: SQL that builds SQLite stores under `home/`. The first line of
  each seed names its target, for example
  `-- target: .local/share/devin/cli/sessions.db`. Built databases are
  gitignored (`*.db`).
- `expected/<connector>__<source path, '/' -> '__'>.json`: FAD output for one
  source. Each file holds every conversation FAD emitted for that source, with
  FAD's full `NormalizedMessage` (`extra`, `snippets`, `invocations`). Source
  paths are relative to `home/`; other strings that held the home path read
  `$HOME`.

## Regenerate

```sh
scripts/regen-oracle.sh              # claude codex devin
scripts/regen-oracle.sh gemini       # other FAD connector slugs
```

The script needs `cargo` and `sqlite3`. It builds the seeds, builds
`tools/fad-dump` (FAD `=0.3.1`), and runs it under
`env -i HOME=testdata/oracle/home PATH=$PATH`, so no developer environment
variable changes connector discovery. Commit the changed `expected/` files
with the fixture change that caused them. `go test` never needs cargo.

`fad-dump` without `--out-dir` prints one JSON object per conversation.
`--agent SLUG` and `--source PATH` (relative to `--home`, or absolute) scope a
run.

## Use in tests

`internal/transcript/oracle` loads `expected/`, and `oracle.Assert` diffs a
parser's `Conversation` and `Message` output against one FAD conversation:
session id, workspace, and the (role, content, created_at) sequence. Each
agent passes `oracle.Rules`: a projection from message kinds to FAD roles and
named `Divergence`s that drop rows FAD and Flopwire treat differently. Every
divergence carries a reason in its `Name`.

## Known FAD limits

Use small fixtures for id-level parity. FAD 0.3.1:

- compacts `extra` (drops native ids) for Claude, Codex and Gemini files of
  32MiB or more;
- errors on Codex files over 100MiB and on a partial final line;
- gives Claude tool-result rows a synthesized `extra` without the line uuid;
- never reads Codex `session_meta.id` or `thread_spawn` (its external id is
  path-derived);
- emits only Devin's main chain (`sessions.main_chain_id`) and drops system
  nodes;
- ignores Gemini `toolCalls` and thoughts;
- folds tool calls into assistant text (`[Tool: Name - description]`) plus
  `invocations`, and drops thinking blocks.

## Second oracle: agentsview (not yet wired)

kenn-io/agentsview (MIT) is the planned differential oracle for per-session
counts, roles and native ids. It needs Go 1.27 (`go 1.27.0` in its go.mod),
and Flopwire stays on Go 1.26, so it is not built here. Its parsers are in
`internal/`, so another module cannot import them. The plan:

1. Add `tools/agentsview-oracle/` as its own Go module (`go 1.27`, built with
   `GOTOOLCHAIN=go1.27.x`), outside the root module's `./...`.
2. Vendor nothing. Build agentsview at a pinned commit (reference:
   563023de1d7b7f5af50ad5967c101341a44a2bfc) and run its parser over `home/`
   through a small `main` placed inside a temporary clone (it must live under
   the agentsview module to import `internal/parser`).
3. Write `expected-agentsview/<agent>__<source>.json` with per-session message
   counts, roles and native ids.
4. Add a loader beside `oracle.Load` that reuses `Rules` and `Divergence`.
   Known agentsview differences to encode as divergences: it merges Claude
   lines by `message.id`, drops system lines and fork branches, keeps no
   Codex `payload.id`, and walks only the Devin main chain.
