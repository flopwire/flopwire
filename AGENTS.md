# Repository instructions

flopwire is one Go binary: team server, device agent, CLI and MCP server
for agent messaging and transcript search. `web/` is the TypeScript admin
console. `plugins/` holds the Claude Code, Codex and opencode plugins.

## Commands

```sh
pnpm --dir web install --frozen-lockfile   # once
make build      # builds bin/flopwire from the committed console files
make web        # rebuilds the console after changes under web/, then commit internal/webapp/dist
make test       # go test -race ./..., web tests, script tests
make e2e        # end-to-end; required for auth, ingest, storage, search, deletion changes
```

## Rules

- Start each bounded change in a fresh worktree on a new branch. Never edit
  or commit from the main checkout. Preserve unrelated work.
- Never commit real transcripts or logs. Fixtures under `testdata/` are
  synthetic.
- Harness data is read-only: `~/.claude`, `~/.codex`, `~/.local/share/devin`.
  Open harness SQLite files with `?mode=ro`.
- A test of a size or memory bound uses sizes just past the bound, never
  gigabytes. Run it against unfixed code under `GOMEMLIMIT=2GiB` first.
- Every markdown file is gold or a log: current truth only, no superseded or
  historical notes. See [Documents](CONTRIBUTING.md#documents).
- Conventional commit subjects (`fix:`, `feat:`, `perf:`); release-please
  owns versions and the changelog.
- After changing `web/`, run `make web` and commit `internal/webapp/dist`; CI checks it.

[CONTRIBUTING.md](CONTRIBUTING.md) has setup, tests and the pull request
policy. [docs/architecture.md](docs/architecture.md) maps the packages.
Design records are under [notes/](notes/README.md).
