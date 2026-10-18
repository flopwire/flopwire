# Launch readiness

Historical launch gate from the CASS-era PR stack (#2 to #8), carried onto
the fresh schema. This is a record of that stack, not the current release gate.
`notes/local-search/README.md` (§11.3) decides what survived.

- `punch-list.md`: P0/P1/P2 gate items and the confirmed decisions behind them.
- `state-machine-walks.md`: concrete-state walks for deletion, backup and
  restore, quotas, and collection.

Where the salvaged work landed:

| Gate item | Old commits | Now |
|---|---|---|
| Migration ledger | #8 `7bcbe33`, `91e1724`, `b9e6387` | B1a: `internal/store/migrate.go` |
| Transactional identity, audit fail-closed, rate limits, rotation, admin login scope, bootstrap by CLI | #6 `031cf6f`, `4d2cbae`, `4d2e741`, `d4eb6a1`, `4c3ee87`, `3d9ad07`; #8 `62d5a64`, `4bbf5fe`, `cb16d94` | B1b: `internal/api`, `internal/store`, `internal/client`, `cmd/flopwire`, `SECURITY.md` |
| Mandatory exclusions | #8 `004879b` | B1b: `internal/pathpolicy` (device agent); server enforcement of the admin path rules at parse time (`internal/ingest/rules.go`) and on rule change: hide, restore, purge on confirmation or after 7 days (`internal/ingest/hidden.go`) |
| Readiness, deletion jobs, backup/restore, non-root runtime | #3 `8a3fc34`, `1f6d8fc`, `4120fba`, `9c39cfd`; #8 `1a64874` | B1c: `internal/store/deletion.go`, `internal/store/chunks.go`, `internal/backup`, `Dockerfile`, `scripts/validate-fresh-compose.sh` |
| Raw-object ledger | #6 `d4eb6a1`, `4c3ee87` | B1a schema (`chunks.state`), B1c reconciler |
| Admin console lifecycle | #5 (minus `209193f`) | B1d: `web/` |
| Release tags and version injection | #4 `validate-release-tag*`, `source-release.yml`, `aa35add` | B1d |
| Git provenance | #7 `internal/provenance/git.go` | B1d (wiring in B2) |

Open at the end of B1 (historical status): search, raw reads, and their audit
events (B3); quota enforcement for chunk uploads (B2/B3); endpoint concurrency limits (B3);
member offboarding (undecided).
