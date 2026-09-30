# Agent workflow

How the retrieval rebuild was built and reviewed (2026-09-29/30), kept so later sessions run agents the same way. Work is tracked in GitHub issues (milestone "first team"). `punch-list.md` is the record of what landed in #111.

## Pattern

1. One builder agent per stream, each in its own git worktree, one PR per stream against the integration branch (or `main`).
2. One independent reviewer agent per PR. It looks for real defects, proves each with a failing test, fixes the confirmed ones on the PR branch, reports the rest, and comments on the PR. It never merges.
3. Merge on green CI. `test` and `e2e-sync` are required checks on `main`.
4. Before a large landing, one more independent pass over every review-fix commit and conflict resolution since the previous pass. Its own fixes get a second reviewer.
5. Product decisions go to the human with a recommendation. An agent that cannot wait makes the conservative choice and flags it.

## Builder rules

- Work only in the assigned worktree. Never touch another worktree or the main checkout. Never use `git stash`.
- The work order is the issue or list you were given. Do only your items. A small edit to another stream's file is allowed; name it in the PR body under "Cross-stream edits".
- Pre-release: no backward compatibility, no migrations of old data, no legacy handling of Flopwire's own formats. Edit the fresh schema in place, change wire formats and flags freely, force a local reindex when needed. Harness formats on disk (Claude, Codex, Devin) still need full coverage.
- Harness data is read-only: `~/.claude`, `~/.codex`, `~/.local/share/devin`. Open SQLite with `?mode=ro`. Never commit real transcript content; fixtures are synthetic or sanitized.
- Go 1.26, no cgo. Every bug fix gets a regression test that fails before the fix. Small direct fixes; no new abstractions unless the fix needs them.
- `go test -race ./...` must pass before the PR opens, with Postgres and MinIO (see `internal/pgtest` for the environment variables). Run commands in the foreground with timeouts under 10 minutes. Scratch files go in the session scratch directory, never in the repo.
- Where a real-corpus check exists (`FLOPWIRE_CORPUS=1`), run it and report the numbers.
- Commits: conventional-commit style, one logical change each, ending with the `Claude-Session` trailer. PR body: summary, each item with what changed and its test, test results, cross-stream edits, deferred items.
- If an item is wrong, already fixed, or much larger than described, say so rather than forcing it. A product decision the brief does not settle: take the conservative option, flag it under "Decisions", keep going.
- Deleting files: one `rm` per command, a fully written literal absolute path inside your own scratch directory or worktree. No wildcards, no shell variables, no loops, no `cd X && rm`. Never delete harness data, `~/.flopwire`, or another agent's directory.
- Memory safety in tests: a test of a size or memory bound declares sizes just past the bound, never gigabytes. Run a regression test against unfixed code under `GOMEMLIMIT=2GiB`. One such run reached 53GB.
- Final report under 400 words: PR URL, items done and deferred, test summary, corpus numbers, decisions flagged, and any contract other streams must know (exported names, schema, behaviour).

## Reviewer rules

- Review one PR: `git diff <base>...HEAD` in a clean worktree on the PR branch. Read the PR body first.
- Find real defects the PR introduces or fails to fix: correctness, data loss, races, crash and restart holes, security, an item claimed done that is not, tests that do not prove what they claim. Skip style.
- Verify every finding by reading the full code path, and prove it with a focused failing test where cheap. Mark each CONFIRMED or PLAUSIBLE.
- Fix every CONFIRMED finding on the PR branch with a regression test that fails first, in small commits, inside the PR's scope. Report PLAUSIBLE findings; do not fix them.
- `go test -race ./...` must pass. Push, then comment on the PR: each finding and its fix commit.
- Never merge. Never touch other worktrees. Foreground commands, timeouts under 10 minutes.
- The deleting and memory-safety rules above apply.
- Final report under 300 words: findings one line each with what you did, test status, and anything the merger must know.
