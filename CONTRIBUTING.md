# Contributing

## Set up

Install Go 1.26.6 or later, Node.js, pnpm, Python 3 and Docker. Then run:

```sh
pnpm --dir web install --frozen-lockfile
make test
```

## Build and test

```sh
make build      # builds the console into internal/webapp/dist, then the binary
make test       # go test -race ./..., web tests, release-tag script test
make e2e
make e2e-sync   # two devices against a Compose server; see docs/two-laptop.md
```

A plain `go build ./cmd/flopwire` also works. It embeds the committed console
files under `internal/webapp/dist`. After changing `web/`, run `make web` and
commit the result; CI fails when the committed files differ from a fresh
build.

Postgres and S3 integration tests run when these variables are set. Each test
creates and drops its own database:

```sh
docker compose -f compose.yaml -f compose.dev.yaml up -d postgres minio
export FLOPWIRE_TEST_DATABASE_URL='postgres://flopwire:<password>@127.0.0.1:55432/flopwire?sslmode=disable'
export FLOPWIRE_TEST_S3_ENDPOINT=127.0.0.1:59000 FLOPWIRE_TEST_S3_ACCESS_KEY=flopwire FLOPWIRE_TEST_S3_SECRET_KEY='<minio password>'
go test -race ./...
```

Tests with `FLOPWIRE_CORPUS=1` read this machine's real transcripts. See
[docs/dev/manual-checks.md](docs/dev/manual-checks.md) and
[docs/release-checklist.md](docs/release-checklist.md).

## Make a change

Fork the repository, or create a branch if you have write access. Create
each bounded change in a fresh worktree on its own branch from current
`origin/main`. Keep one reviewable pull request per change. Do not combine
unrelated refactors with protocol changes.

The admin console under `web/` is embedded from the committed build in
`internal/webapp/dist`. After any change under `web/`, run `make web` and
commit the result. CI fails when the committed build is stale.

Keep raw user traces out of commits and test logs. Add sanitized synthetic
fixtures under `testdata`. Preserve authenticated provenance in every new
ingestion path. Keep raw objects and Postgres as the only durable backup
contract.

Add tests for changed behavior. Use the race detector for Go code. Build the
web application with strict TypeScript. Run `make e2e` for changes to
authentication, ingestion, storage, search, or deletion.

## Pull requests

Use a conventional title with a concrete description, such as
`fix(search): report incomplete scans`. The title becomes the squash commit
subject and drives release-please.

Every pull request has two sections:

- **What changes and why:** the concrete problem, the resulting behavior, and
  why the approach fits, written for a reviewer who has not seen the
  conversation.
- **Validation:** the tested commit, the commands and results, and material
  unverified behavior. Distinguish local checks from GitHub CI.

Visual changes add a **Before and after** section with captures from the
running app. Main requires an up-to-date branch and passing `test`,
`e2e-sync` and `perf-gate` checks. Merges are squash merges.

The full policy, including stacked pull requests, evidence, visual captures,
merge and worktree cleanup, is in
[docs/dev/pull-requests.md](docs/dev/pull-requests.md).

## Documents

Every markdown file is gold or a log. A gold document states current truth
only. Overwrite or delete outdated text. Never annotate text as superseded,
historical or legacy, and never refer to earlier plans; git is the history.
A log is named `<subject>-YYYY-MM-DD.md`, records events as of its date, and is
edited only to remove private data. Dated records live under `notes/log/`.

## Releases

Use conventional commit subjects: `fix:` and `perf:` for patches, `feat:`
for features, and `!` or `BREAKING CHANGE:` for breaking changes.
Release-please manages the release PR, changelog, version manifest, tag,
and GitHub release. Do not bump the manifest or write changelog entries
in feature PRs. See [Releases](docs/releases.md).
