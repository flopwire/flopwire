# Boundary audit qualification

This record distinguishes executable contracts from the historical findings
in [README.md](README.md). Remediation is tracked in PRs #278–282; exact tested
revisions, run results and review records belong in those PRs.

## Contracts

- Standalone supports a full local index. Connected collection defaults to
  minimal storage, with explicit full storage available.
- Retention does not change authorized shared result IDs or repository scope.
- Inbox bodies and receipts belong to a normalized server and device identity.
  Standalone has a separate inbox. Returning to an authority resumes its state.
- Connected collection uses valid same-authority saved rules offline. Unknown
  rules pause collection and uploads without deleting retained data or jobs.
- A local raw address must never silently select an unrelated archive record.
- Fresh and delegated one-shot completion include discovery, placement recovery,
  indexing and the requested upload completion stage.
- Automatic storage selection must not silently discard retained local content.

## Concrete traces

### Stale raw address

```text
t0 local = {generation: 1, inode: I, indexed bytes: A, offset: O}
   server = {generation: 0, bytes: A}
t1 file = {inode: I, bytes: B, size: unchanged}; local index still describes A
   server = {generation: 1, bytes: B}
t2 old local read checks inode and current local generation, returns B at O
t3 file removed; old archive fallback requests latest (-1), also returns B
```

The local and server generation numbers are independent. Replacing `-1` with
the local number cannot establish record identity. The bounded correction
rejects unverifiable local reads and requires an explicit server address for
archived evidence. Same-file reads also check the indexed watermark. A racy
timestamp requires verification against a persisted hash covering the entire
requested range; an uncovered range is unavailable.

### Delegated completion before recovery

```text
t0 placement = {deleted worktree, recovery pending}; upload queue = {}
t1 delegated pass discovers transcript, cannot yet offer it under path rules
t2 old delegated pass drains empty upload queue and returns success
t3 recovery resolves main checkout; later sweep queues the transcript
```

A recovery barrier reproduces this ordering. Fresh one-shot execution waits
for recovery and a second sweep. Delegated execution must do the same before
declaring upload completion.

### Defaults must survive restart

```text
t0 config = {environment credential, no explicit mode}; index = absent
t1 connected default creates sync-only index
t2 restart sees existing index
```

An implementation that treats index existence as a request for full storage
changes modes at t2. Mode selection must express the operation's default and
explicit choice separately from a guard against destructive index conversion.

### Persisted upload queue changes destination

```text
t0 connection = A; queue = {captured bytes: X, acknowledged: false}
   index = shared SQLite; spool = shared directory
t1 stop agent; remove source; change connection to B
t2 old startup opens the same queue and spool, installs B's credentials
t3 B's valid allow policy accepts the source; queue flush sends X to B
```

An isolated replay used actual `startSyncFrom`, scheduler, syncer, SQLite,
spool and authenticated HTTP conformance servers. It observed 1,020,000
historical bytes sent to B. A separate deny-policy case sent none. Policy
enforcement is not queue ownership: B allowing new collection does not assign
it A's historical queue. Persisted acknowledgments and known chunk hashes
also belong to the authority that produced them.

The correction must establish queue ownership before dispatch, preserve state
on mismatch, and avoid implicitly assigning a legacy ownerless queue. This
reproduction is not a claim that a production server accepted a cross-authority
upload.

### Refused startup must precede retention migration

```text
t0 index = {full, retained messages}; upload queue = {owner: A}
   requested startup = {server: B, explicit sync-only}
t1 old ordering rebuilds the full index into minimal storage
t2 queue owner check rejects B; startup fails after removing local messages
```

The owner check must execute under the index writer lock before migration.
An unlocked preliminary check is insufficient: another writer could establish
an owner between the check and acquiring the lock. Full CLI regressions verify
retained content, mode, queue state and FTS file hashes after refusal, including
ownerless legacy state.

## Independent review and limitations

Claude reviewed the first implementation in two read-only consultations. The
root reviewer verified the findings against source and reproduced the material
ones. Those reviews led to hash-verified append reads, an unconditional sweep
after competing background recovery, and authority validation before index
migration. Review opinions are supplementary; the regressions and real service
tests are the acceptance evidence.

Queue ownership means normalized server URL plus enrolled device identity.
Without a device ID, a token fingerprint is the conservative fallback. Switching
between those identity forms, or replacing a token without an enrolled identity,
does not implicitly adopt a queue. This contract does not identify a replacement
server instance behind the same URL and device identity.

The user chose to preserve ownerless legacy upload state and pause uploads
until ownership is explicitly assigned. The conservative implementation refuses
connected agent startup, including collection and messaging; it is not a running
agent with only its uploader paused. This change adds no assignment command
and performs no live migration. A fresh `--db` starts a separate queue; it does
not resume the old queue. Inactive legacy spool files remain on disk outside
the new queue's active spool accounting.

Automatic mode transitions refuse to discard retained local messages. Explicit
full storage remains available. Automatic minimal mode watches credential
removal and restarts into standalone mode; an explicitly selected minimal mode
retains its existing documented connection behavior.
If standalone collection adds message rows, reconnecting in automatic mode
requires an explicit retention choice before those rows can be removed.

Local raw evidence deliberately reports unavailable when neither a stable file
tuple nor a persisted hash covering the requested range establishes identity.
A digest-covered record can remain available after append. An uncovered range
may wait for a stable sweep, whose default interval is 45 seconds; continued
writes can delay availability. Explicit server-addressed raw reads remain
available under server authorization.

## Acceptance evidence

| Contract | Executable check | Scope of evidence |
|---|---|---|
| Shared result parity | `TestRetainedRepoMetadataRealServerResultParity` | Real API/Postgres, authenticated reader, literal expected IDs, unrelated and hidden negatives, full/minimal with main, deleted worktree and clone |
| Inbox isolation and return | `TestAuthorityInboxAcrossRealServers` | Two independent API databases, pending body and owed receipt, standalone and another-device isolation, blocked B poll, return to A |
| Policy authority | `TestPolicyAuthorityTransitionPreservesRowsUntilOwnFetch` | Actual indexing/purge with HTTP policy responders; A allow/B deny and inverse, offline unknown state, return to saved A policy |
| Upload queue ownership | `sync_authority_integration_test.go` | Real startup/scheduler/SQLite/spool with authenticated HTTP conformance servers; no foreign HTTP, exact retained bytes on return, full CLI refusal before migration |
| One-shot completion | `once_recovery_test.go`, `agent_once_discovery_test.go` | Deterministic recovery barriers, real daemon loop, full/minimal discovery through scheduler, fresh/delegated acknowledgment and timeout; synthetic upload transport |
| Raw identity | `TestLocalRawRefusesRewrittenArchive` and `raw_identity_test.go` | Real PostgreSQL/object archive with two generations, same-inode rewrite, missing source, verified append, deterministic same-timestamp mutation |
| Default and transition behavior | `synconly_test.go`, `credwatch_test.go`, `mode_test.go` | Explicit override, restart stability, retained-content refusal, empty MCP-created index, credential removal back to standalone |
| Cross-process behavior | `make e2e-sync` | Connected-default storage SQL/FTS checks, shared read, offline full storage, crash recovery, rotation, backup/restore, and excerpts in both retention modes |

One-shot completion does not newly promise that every discovered source parsed
successfully. Per-source parser failures remain separately reported; an empty
upload queue alone is not proof of complete extraction.

The first combined run exposed two fixture assumptions: implicit full storage
in local-search scenarios, and replacing the crash test's server URL after its
queue already existed. Fixtures now request full storage where they test local
search and keep a stable pinned proxy endpoint for crash injection. Neither
queue ownership checks nor crash assertions were bypassed.

The same run exposed the automatic-minimal credential-removal regression.
A focused run reproduced it; the corrected watcher is covered by the actual
full-to-minimal-to-standalone lifecycle. Review also produced a second recovery
regression that fails when the daemon's background recovery wins before the
delegated pass. These failures are retained in PR evidence rather than hidden
by retrying the same tests until they pass.
