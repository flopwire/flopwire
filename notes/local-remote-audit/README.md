# Local and remote boundary audit

Audited revision: `883a467f427be88f05a58c2751a08f1cb30e6d86`, 2026-10-14.
The excerpt correction is separate in PR #267. This audit changes no product
behavior and does not switch existing devices to minimal storage.

## Judgment

The excerpt defect was a small implementation error at a consequential
boundary. The broader audit found other paths that confuse content retention,
operational metadata, request authority, or completion. A wholesale backend
rewrite is not justified by this evidence. Explicit contracts and a small
sequence of fixes are justified before making minimal local storage the
connected default.

Standalone full indexing remains supported. Connected content reads use the
server's authority; choosing to retain fewer local message rows should not
silently change those shared answers. Local offline search is deliberately a
full-index capability, not a promise of sync-only mode.

## Method and limits

Four Sol reviewers inspected retrieval, mutations, storage/delegation, and inbox
scope. The primary reviewer checked material source claims, reconciled them
against existing documented behavior, and assembled the state traces below.
The raw material was implementation, SQLite state, HTTP request arguments,
control-socket requests, and synthetic observations, not previous summaries.

The replay uses isolated SQLite databases, existing synthetic fixtures, a real
Unix socket for delegation, and fake remote interfaces where identified. It
makes no production requests. Observational tests assert the current defect;
a passing replay confirms its presence, not that the product is correct.
They are kept outside the normal test suite. Run from the repository:

```sh
python3 notes/local-remote-audit/replay.py
```

The script uses Go overlays; it does not edit package source files. Go and
Python are required. The archived fixtures depend on test helpers at the
audited revision. For each fix, convert its desired invariant into a normal
regression that fails before the fix and passes afterward.

This was a source audit plus bounded reproductions, not a deployed-system
penetration test or an exhaustive proof. Server/device transitions, archive
generation changes, and delegated upload completion still need the integration
checks listed below. No new server authorization bypass was demonstrated.

## Current operation boundaries

| Operation | Standalone / explicit local | Connected shared | Minimal-storage consequence |
|---|---|---|---|
| Search, grep, read, sessions | Full SQLite message index | Reader-authorized HTTP; visibility checks; errors propagate | Shared content reads work without local rows |
| Explicit local content query | Local authority | Explicit scope remains local | Sync-only correctly returns unavailable |
| Raw evidence | Local bytes when present; authorized archive fallback when absent | Server archive and redaction checks | Fallback identity is insufficiently verified |
| Shared repository filter | Filesystem and retained placement facts resolve repo identity | Expanded identity goes to server | Sync-only reader drops retained placement facts |
| Message send / peers / inbox | Local bus | Server bus, no network-error fallback | No intrinsic message-index dependency found |
| Hook delivery / receipts | Durable device inbox | Durable device inbox, receipts to server | Inbox lacks authority partitioning on transitions |
| Discovery and upload | Discovery only | Scheduler, spool, server acknowledgments | Metadata survives without message rows |
| One-shot agent run | Local pass | Fresh process drains uploads | Delegated pass loses mode and drain contracts |
| Redaction | Local sidecar/index | Server first, then this device | Errors report partial outcome; no rollback |
| Policy | User rules plus cached admin floor | Fetches server rules and caches them | Cache errors and authority transitions are ambiguous |

Sources include `cmd/flopwire/retrieve.go`, `internal/retrieval/local/raw.go`,
`internal/devicebus/local.go`, `internal/agent/control.go`,
`cmd/flopwire/agent.go`, `cmd/flopwire/redact.go`, and
`internal/agent/policy.go`.

## Findings and acceptance checks

### 1. Shared repository scope depends on a full local index

**Reproduced; P2.** `localRepoDirs` in `cmd/flopwire/retrieve.go:154` opens the
index read-only and silently returns no metadata on error. That open rejects
sync-only at `internal/localindex/store.go:391`, although sync-only retains
placements. Deleted worktrees therefore lose their recorded main checkout and
remote during shared query expansion. The server does not resolve an absolute
path into repository identity (`internal/retrieval/repo.go:54`). Full versus
minimal retention changes the shared query's scope.

```text
t0 filesystem = {old worktree: absent}
   metadata = {old worktree -> main checkout, remote}
   server = {same uploaded conversations in both cases}
t1 full read-only open succeeds -> expansion includes 3 repository mains
t2 sync-only read-only open fails -> expansion has only the old path
   shared request is narrower despite identical server data
```

Replay: `TestAuditSyncOnlyMetadataUnavailable` and
`TestAuditDeletedRepoNeedsPlacements`. The latter compares the resolved filter,
not a live server's result count.

Fix: expose retained placement metadata independently of message/FTS access.
Do not make explicit local content queries succeed on sync-only. Gate: run the
same shared query with a live checkout, a deleted worktree, and a remote clone
in full and sync-only modes; assert identical authorized result IDs and filters.

### 2. Delegated `--once` drops requested execution constraints

**Mode loss reproduced; upload completion source-traced; P2.**
`openAgentIndex` (`cmd/flopwire/agent.go:498`) sends only `Op: pass` and the
index path when a daemon holds the writer lock. Requested retention is absent.
The successful nil-store result returns before the fresh-process setup/drain
path. `Agent.Pass` (`internal/agent/control.go:456`) waits for local indexing
and commit, not remote upload acknowledgment.

```text
t0 daemon = {mode: full, index lock: held, upload queue: pending}
   caller = {once: true, sync-only: true, sync-wait: 1s}
t1 caller sends {op: pass, index: path}; constraints are absent
t2 daemon replies success after local commit
   caller exits success; mode remains full; remote completion is not checked
```

Replay `TestAuditOnceIgnoresRequestedRetention` proves both conflicting mode
requests are discarded using a real writer lock and synthetic socket responder.
It does not prove a real upload completed or failed. The upload-drain difference
follows from the early return and `Pass` implementation.

Fix: give delegated execution a contract that checks mode/connection constraints
and waits for the requested completion stage, or reject incompatible requests.
Gate: actual daemon with server blocked; fresh and delegated `--once --sync-wait`
must report equivalent completion or timeout. Conflicting `--sync-only` and
`--no-sync` must never silently succeed under another mode.

### 3. Durable inbox state lacks a server/device owner

**Reproduced at devicebus boundary; P1 before authority switching.**
The common config directory always supplies `bus.db`
(`cmd/flopwire/agent.go:329`). Rows have `origin=server/local` but no authority
namespace (`internal/devicebus/store.go:43`). `TakeWith` filters by session and
harness, not server. `sendAcks` loads old owed receipts and uses current Connect.

```text
t0 inbox = {M: queued, origin: server A, recipient: S}
t1 reopen same database as standalone -> Take(S) returns M
   Confirm(M) -> inbox[M] = {delivered, receipt: owed}
t2 reopen with server B -> sendAcks sends M's identifier to B
```

Replay: `TestAuditOldAuthorityScope`. This proves cached-body delivery and
receipt misrouting, not that B accepts that receipt. The same OS user and
recipient session are required. A successful new poll can remove old queued
rows, but standalone never polls and startup receipts can precede reconciliation.
Same-authority offline/restart delivery is intentional and must remain intact.
Ordinary same-server login is not proven to change the enrolled account.

Fix: bind durable queue/receipt state to normalized server plus enrolled device
identity, and define standalone transition handling. Preserve old authority
state for recovery rather than sending its receipts elsewhere. Gate: two actual
servers, blocked initial poll, queued/leased/delivered messages and owed receipts;
assert B sees no A identifier and hooks obey the selected scope. Repeat with
same-authority restart to prove no message or receipt loss.

### 4. Corrupt cached admin policy removes the offline floor

**Policy function reproduced; full boot not exercised; P2.**
`readAdminCache` returns empty policy on unreadable/invalid data
(`internal/agent/policy.go:324`). Startup preloads that result, and a failed
fetch keeps it. The documented last-known offline floor then becomes ordinary
user policy. A known sensitive placement can become allowed for local indexing.
Server-side policy enforcement remains a separate protection.

Replay: `TestAuditCorruptAdminCacheDropsFloor` calls actual `DevicePolicy` with
a valid deny cache and then truncated JSON; its decision changes deny to allow.

Fix: represent valid, absent, and unreadable policy states separately. Retain a
recoverable last-known policy and define which collection may proceed without
it. Gate: corrupt/missing/unwritable cache plus offline boot, while observing
actual local rows and queued uploads. A first enrollment with no prior policy
must have an explicit rule rather than accidentally inheriting error behavior.

### 5. Cached admin policy lacks server ownership

**Source-traced; P2.** `admin-path-rules.json` has policy and timestamp but no
server identity (`internal/agent/policy.go:319`;
`cmd/flopwire/agent.go:102`). Server-switch login clears login state, not this
cache. An offline restart under B can preload A's restrictions; standalone may
also retain them. Whether standalone should keep the last administrator's floor
is a product choice, but the implementation cannot distinguish the authorities.

Fix and gate: bind cached policy to its authority; test A-denies/B-allows and
A-allows/B-denies with B's fetch unavailable. Check actual indexing/purge/upload
behavior. Do not simply delete restrictions and treat unknown policy as allow.

### 6. Local raw fallback does not establish record identity

**Missing verification reproduced with fake archive; P2.**
`internal/retrieval/local/raw.go:62` asks the server for latest generation (`-1`)
using the local indexed record's offsets. Local and server generation counters
are intentionally independent. Same path/file identity does not prove the
latest generation contains the same record at those offsets after a rewrite.
`RawByPath` discards response attribution headers. Returned bytes are exposed as
the raw evidence for the original address without checking correspondence.

Replay: `TestAuditRawGenerationMismatch` substitutes unrelated archive bytes
and observes acceptance for the unchanged indexed address. This demonstrates
missing validation, not an actual server rewrite timeline. The archive fake is
not evidence that a particular production rewrite has occurred.

Fix: establish a content/record identity across archive and index, or return an
explicit evidence-unavailable/mismatch result. Do not substitute the local
generation number as if it were the server's. Gate: real same-inode rewrite,
lagging local index, missing local source, and two archived generations; require
the correct record or a clear error, never unrelated bytes.

## Intentional differences and migration decisions

- Connected reads fail closed; explicit local scope can read retained local
  material. This is not an automatic fallback or a newly found authorization bug.
- Connected redaction attempts the server before local work. Server failure
  returns an error without local mutation; server success/local failure also
  returns an error after printing the server outcome. Independent attempts or a
  durable retry would be a new contract. Other devices' local indexes do not
  automatically inherit an owner's server redaction under the current model.
- A different-server login does not restart an already connected agent. Its
  original server remains pinned until restart, explicitly documented in
  `docs/agent.md:306`. It is a usability/transition risk, not hidden retargeting.
- Full -> sync-only -> full rebuilds from available native transcripts, not a
  server download. Local history whose native files were already removed is
  not restored. Decide migration/backup behavior before changing defaults.
- An empty upload queue is not proof of complete discovery or extraction.
  Per-source parse errors are logged/counted; a zero exit is not an existing
  guarantee that every source parsed successfully. No new completion promise is
  inferred from status labels.

## Redesign sequence

1. Land the bounded excerpt authority correction after final review/checks.
2. Separate placement/operational metadata access from optional message indexes.
3. Bind inboxes, receipts, and policy caches to explicit authority identities;
   specify transition behavior and preserve same-authority offline recovery.
4. Make delegated execution carry and enforce its completion/mode contract.
5. Establish verifiable archive record identity for raw evidence.
6. Qualify the minimal-storage default and any migration only after these
   contracts pass the mode/authority matrix.

The common design is explicit scope at the operation boundary: authority,
retention capability, evidence identity, and completion stage are different
inputs. Centralize scope selection and contract tests; do not hide deliberate
local/shared differences behind an automatic fallback.

## Evidence independent of reviewer opinion

Existing focused routing, sync-only, rotation, redaction and deletion tests
passed in the audit. The replay captures current counterexamples. The next fixes
need red/green regressions and the real two-device/two-server cases above.
Run the same operations under standalone full, connected full, connected
sync-only, offline, denied, corrupt metadata, and identity transition conditions.
Use barriers for races, literal expected content/result IDs, SQL storage checks,
and captured destination requests. PR #267's E2E is evidence for excerpts only;
it does not certify the other boundaries in this report.
