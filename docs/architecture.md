# Architecture

This page states where each part runs, the rules that cross components, and
the limits callers can rely on. The reasons behind it are in the spec,
[notes/local-search/README.md](../notes/local-search/README.md), and the
decisions D1–D21 in [notes/log/punch-list-2026-09-30.md](../notes/log/punch-list-2026-09-30.md). Where the
spec and this page differ, this page describes the code. For the package
layout, run `go list ./...` and read each package's doc comment.

## Components

| Component | Where it runs | Code | Role |
|---|---|---|---|
| Parsers | device and server | `internal/transcript` | Turn harness transcripts into conversations and messages with stable ids. The same code runs on both sides. |
| Device agent | each developer machine | `internal/agent` | Finds changed transcripts, places each session, applies path rules, indexes locally, hands sources to sync. |
| Local index | each developer machine | `internal/localindex` | SQLite database plus FTS5 shard files. Derived; rebuildable from the transcripts. |
| Sync client | each developer machine | `internal/devicesync` | Content-defined chunking, spool, per-source retry, upload over the pinned TLS client. |
| Server API | server | `internal/api` | Authentication, sync endpoints, retrieval, admin, deletion. TLS unless a proxy terminates it (or `--tls off`, loopback only). |
| Ingest | server | `internal/ingest` | Stores chunks and manifests, runs the parse queue, enforces admin path rules. |
| Postgres | server | `internal/store`, `migrations` | Identity, audit, sources, generations, chunk ledger, manifests, conversations, messages, tombstones, deletion jobs. |
| S3 (MinIO in Compose) | server | `internal/ingest` | Content-addressed chunk objects (BLAKE3 of the uncompressed bytes, stored as zstd frames). The raw evidence. |
| Retrieval | device and server | `internal/retrieval` | grep, search, sessions, read and commit evidence over the local index or Postgres. |
| CLI and MCP | anywhere | `cmd/flopwire` | Five retrieval tools, including `trace` (MCP `flopwire_change`). Shared after enrollment; local before enrollment or with `--local`. |
| Message bus (server) | server | `internal/bus` | Presence, send, long poll, claim, receipts, peers, inbox, acceptance, retained threads. |
| Message bus (device) | each developer machine | `internal/devicebus` | Local inbox, presence, the long poll, claims and receipts; routing between the device's own sessions with no server. `flopwire hook` delivers. |
| Web console | browser | `web/`, served at `/` | Admins: health, people and devices, policy, archive (deletion), audit. Every member: the Messaging page (held messages, accept, revoke). No corpus search. |
| Backup | server host | `internal/backup` | Coordinated Postgres dump plus chunk copy, verify, restore. |

## Data invariants

The same model exists in SQLite on the device and in Postgres on the
server. The device holds only its own sources.

- Message ids are stable across re-parses (D16). A parser version change
  re-parses from the original bytes and replaces rows only when that
  succeeds.
- A message absent from a new generation is marked `superseded`, never
  deleted. A transcript file the harness deletes keeps its rows live and
  searchable (D1).
- Every tool result gets a row, empty or not, with `is_error` from the exit
  code (D19). `injected` rows (CLAUDE.md, AGENTS.md, system reminders) are
  hidden unless a query names the kind (D6).
- `user_id` and `device_id` come from the uploading credential, never from
  transcript content.
- The uploaded offset advances only on server acknowledgement. One failing
  source delays only itself (D15). A provisional tail seals after 5 minutes
  with no change (D21).

## Local index

One writer (D12): the agent holds an exclusive flock on `index.db.lock`, and
readers open read-only without a lock. The index is derived and has no
migrations: a schema version mismatch drops the index tables and re-parses
every transcript, keeping placements. Files:
[agent.md](agent.md#local-search-files).

## Path rules and placement

Decision D18. Syntax and matching: [path-rules.md](path-rules.md).

- The agent is primary: it applies user rules, admin rules and
  `unplaceable` before it indexes and before every hand-over to sync. The
  most restrictive rule wins; admin rules are floors. A local `deny` never
  deletes server copies.
- The server is a floor: at parse time it refuses a session its admin rules
  cover (no rows, source tombstoned). It cannot resolve worktrees or
  symlinks, so it can miss what the agent catches.
- An admin rule change hides newly covered conversations at once; an admin
  purges them (`flopwire admin policy purge`) or the server does after 7
  days. A later rule change that no longer covers one restores it.

## Deletion and tombstones

Decision D9. Deletion is manual, except the path-rule purge above.

1. The owner (`DELETE /v1/conversations/{id}`) or an admin
   (`DELETE /v1/admin/conversations/{id}`) requests a delete. The server
   answers `202` with a job.
2. In the request transaction, the server writes a tombstone for
   `(user, agent, session)`, which covers that session on every device of
   the owner, and for every subagent session below it. It deletes the
   conversations and their messages, tombstones sources nothing else
   references, and drops their provisional tails.
3. A worker purges the chunks no manifest still references. A job fails
   after five attempts and waits for `POST /v1/admin/deletions/{id}/retry`.
4. A later flush to a tombstoned source is acknowledged and dropped. A
   re-upload of the session from any of the owner's devices stays deleted.
5. Backups leave out chunks referenced only by tombstoned sources.

A harness deleting its file deletes nothing (D1). A backup copies what
exists at that moment: a hidden session, or raw evidence not yet refused by
its parse, can be in it.

## Backup and restore

`flopwire backup` reads one Postgres snapshot for both the dump and the
chunk list, verifies each copied object and writes `manifest.json` last.
Uploads continue; physical purges wait. `flopwire restore` needs empty
targets. Durable state is identity, audit, policy, sources, generations,
chunk ledger, manifests, provisional tails, tombstones and the S3 objects;
conversation and message rows are rebuilt by replaying manifests through
the parsers. Procedure: [runbook.md](runbook.md#recover).

## Query budgets and caps

Decision D8.

| Limit | Local | Server |
|---|---|---|
| Time budget | 10s default, `--timeout` up to 60s | Same; each call runs in a read-only transaction with a matching `statement_timeout` |
| Out of budget | grep and search retain partial hits and explain the incomplete query. CLI exits 2; MCP marks empty incomplete answers as errors and preserves useful partial answers. Resumable grep reports progress in `scan`. | Same for grep and search (HTTP `200` with `truncated` and `reason`); `sessions`, `read` and raw reads fail with `504` |
| Byte budget | 1GiB of text verified per grep | 200,000 candidate rows per grep |
| Ranked search | Ranks the newest 20,000 matches of a very common term, with the note `ranked newest 20k of N matches — add terms or filters` (D2) | Ranks in Postgres (`ts_rank_cd`); no 20k cap |
| Unindexable regex | Scans the newest 20,000 rows that pass the filters | Refused with `400` unless scoped to one session: add a literal of 3 or more letters or digits |
| Concurrency | MCP runs up to 8 calls at once and honours `notifications/cancelled` | 16 retrieval requests in flight, then `429`; other statements stop after 5 minutes |

"Newest" means message time everywhere (D7).

## Self-exclusion

Decision D4. An agent searching the index would find its own call within a
second. `grep`, `search` and `sessions` leave out the calling session and
its subagents when detection finds it by exact evidence, first match wins:

1. Over MCP, the Codex thread id in the call's `_meta` (`threadId`, or
   `thread_id` in `x-codex-turn-metadata`). It counts only when Codex
   launched the server: the nearest harness among the server's ancestor
   processes is a `codex` process. Any other client's `_meta` is ignored,
   since it could name any Codex thread.
2. `FLOPWIRE_SESSION_ID` (with optional `FLOPWIRE_AGENT`).
3. An ancestor process's Claude Code session file,
   `<Claude config dir>/sessions/<pid>.json`.
4. An ancestor `devin` process whose pid exactly one Devin
   `session_locks/<session>.lock` names.
5. An ancestor `codex` process: `CODEX_THREAD_ID` (set for its shell
   commands), else exactly one rollout file it holds open.
6. `CLAUDE_CODE_SESSION_ID`, or `CODEX_THREAD_ID`, when the walk found no
   harness; neither when both are set.

There is no guess from the working directory or recent activity. MCP
calls always apply it; the CLI only when stdin is not a terminal. The
output names the excluded session; `--include-self` turns it off. The
message bus uses the same detection to name the sender; `send` and `inbox`
refuse to run without one.

## Message bus

User-facing contract: [messaging.md](messaging.md). Design:
[notes/message-bus/plan.md](../notes/message-bus/plan.md).

The CLI and MCP tools reach the server only through the device agent's
control socket. The server (or, with no server, the device agent) sets every
envelope field. `read` is set only by the device that indexes the
recipient's transcript. Bus operations commit with their audit event;
bodies are never in the audit log.

## TLS

| Mode (`flopwire serve --tls`, `FLOPWIRE_TLS`) | Behaviour |
|---|---|
| `auto` (default) | `acme` with a domain, else `self-signed`. |
| `self-signed` | ECDSA P-256 certificate made on first start in `FLOPWIRE_TLS_DIR`. Devices pin its SHA-256 fingerprint. |
| `acme` | Let's Encrypt via TLS-ALPN-01 on public port 443. Devices use the system trust store. |
| `proxy` | Plain HTTP behind the operator's TLS proxy. Logs a warning banner. |
| `off` | Plain HTTP on loopback only. For tests. |

- The fingerprint rides in the invite string (in the URL fragment) and is
  saved by `claim`, `bootstrap --fingerprint` or `login --fingerprint`.
- Every client connection checks the pin before it sends a credential. The
  pin replaces chain and hostname checks.
- The client refuses `http://` to any host that is not loopback, and all
  `http://` once a pin is saved.
- A pin mismatch is permanent: the agent stops all uploads and keeps the
  queue. When the server's TLS directory is lost, each device runs
  `flopwire login --fingerprint <new>`, and the running agent resumes.

## Trust boundaries

- The device agent decides what leaves a machine. Path rules decide which
  sessions leave; recognized secrets are redacted on the device before
  upload and again on the server.
- The server trusts only the credential for identity. A device credential
  uploads and reads; a service credential only uploads; admin routes need
  a human admin's login session, never a device token.
- TLS with a pinned fingerprint, ACME, or the operator's proxy protects
  transit. No credential is sent before the pin check.
- The server and host administrators see plaintext. Nothing is encrypted
  at rest by flopwire.
- Every member reads the whole corpus. Every hit and raw read carries its
  user, device, harness, session and repo (D10).
- Accepting a message sender, revoking one, and reading held messages
  need the person's own login session, never a device token; accepting
  also needs the person's password, so an agent that reads the saved
  session cannot accept for its human (it can still list held previews
  and revoke). An accepted sender's agents can direct the
  recipient's agents within each session's permissions; the
  information-only rule for their messages is guidance to the model.
- Retrieval and admin reads are audited with the query and result ids.
  Security-sensitive operations fail closed when audit fails.
- Message rows and indexes are derived and can be rebuilt; S3 chunks and
  manifests are the evidence.

## Schema

`migrations/*.sql` hold the whole schema. The server applies pending files
in one transaction under an advisory lock and records each file's checksum
in `flopwire_schema_migrations`. It refuses a database whose ledger has
edited, missing or unknown entries, and a database with application tables
but no ledger. Applied files and ledger checksums are immutable;
schema changes use additive migrations that preserve durable data.
