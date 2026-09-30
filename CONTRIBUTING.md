# Contributing

## Set up

Install Go, Node.js, pnpm, and Docker. Then run:

```sh
pnpm --dir web install --frozen-lockfile
make test
```

## Change discipline

Keep raw user traces out of commits and test logs. Add sanitized synthetic
fixtures under `testdata`. Preserve authenticated provenance in every new
ingestion path. Keep raw objects and Postgres as the only durable backup
contract.

Add tests with each change. Use the race detector for Go code. Build the web
application with strict TypeScript. Run `make e2e` for changes to authentication,
ingestion, storage, search, or deletion.

## Pull requests

Explain the user-visible behavior, security impact, migration impact, and
verification performed. Keep changes reviewable. Do not combine unrelated
refactors with protocol changes.

## Releases

Use conventional commit subjects: `fix:` and `perf:` for patches, `feat:`
for features, and `!` or `BREAKING CHANGE:` for breaking changes.
Release-please manages the release PR, changelog, version manifest, tag,
and GitHub release. Do not bump the manifest or write changelog entries
in feature PRs. See [Releases](docs/releases.md).
