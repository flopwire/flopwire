# Architecture

This page maps the system as built. The reasons behind it are in the spec,
[notes/local-search/README.md](../notes/local-search/README.md), and the
decisions D1–D21 in [notes/punch-list.md](../notes/punch-list.md). Where the
spec and this page differ, this page describes the code.

## Components

| Component | Where it runs | Code | Role |
|---|---|---|---|
| Parsers | device and server | `internal/transcript/{claude,codex,devin}` | Turn harness transcripts into conversations and messages with stable ids. The same code runs on both sides. |
| Device agent | each developer machine | `internal/agent`, `cmd/flopwire/agent.go` | Finds changed transcripts, places each session, applies path rules, indexes locally, hands sources to sync. |
| Local index | each developer machine | `internal/localindex` | SQLite database plus three FTS5 shard files. Derived; rebuildable from the transcripts. |
| Sync client | each developer machine | `internal/devicesync`, `internal/syncproto` | Content-defined chunking, spool, per-source retry, upload over the pinned TLS client. |
| Client config and transport | each developer machine | `internal/client` | `config.json`, credentials, fingerprint pinning, timeouts. |
| Server API | server | `internal/api`, `cmd/flopwire` (`serve`) | Authentication, sync endpoints, retrieval, admin, deletion. Always TLS unless a proxy terminates it. |
| Ingest | server | `internal/ingest` | Stores chunks and manifests, runs the parse queue, enforces admin path rules. |
| Postgres | server | `internal/store`, `migrations` | Identity, audit, sources, generations, chunk ledger, manifests, conversations, messages, tombstones, deletion jobs. |
| S3 (MinIO in Compose) | server | `internal/ingest/objects.go` | Content-addressed chunk objects, each a zstd frame of the chunk (addressed by the BLAKE3 of the uncompressed bytes). The raw evidence. |
| Retrieval | device and server | `internal/retrieval` (`local`, `regexq`, `grep`, `format`) | grep, search, sessions and read over the local index or Postgres. |
| CLI and MCP | anywhere | `cmd/flopwire/retrieve.go`, `cmd/flopwire/mcp.go` | The four tools. Local by default; `--server` for team search. |
| Message bus (server) | server | `internal/bus`, `internal/busproto`, `internal/api/bus.go` | Direct messages between agent sessions: presence, send, long poll, claim, receipts, peers, inbox, acceptance. Device agent, CLI and hooks are not built yet. |
| Web console | browser | `web/`, served at `/` by `internal/webapp` | Admin only: health, people and devices, policy, archive (deletion), audit. No corpus search. |
| Backup | server host | `internal/backup` | Coordinated Postgres dump plus chunk copy, verify, restore. |

## Local flow

```text
~/.claude/projects   ~/.codex/sessions   ~/.local/share/devin/cli/sessions.db
        |                    |                          |
        +--------- read only, never written ------------+
                             |
   change detection: 45s sweep (identity, size, ctime) + 500ms fast lane
                     + kqueue/inotify directory events + hook flush
                             |
   placement: cwd -> worktree root -> main checkout -> origin remote
                             |
   path rules (user + cached admin): allow | local | deny | unplaceable
          |                                   |
      deny: skip                        allow / local
                                             |
                          parsers (claude@N, codex@N, devin@N)
                                             |
            local index writer (one process, flock on index.db.lock)
                 index.db: sources, generations, conversations,
                           messages, placements, fts_queue
                 index.db-tok   FTS5 unicode61 (ranked search)
                 index.db-tri0/1 FTS5 trigram (grep)
                                             |
            allow only: devicesync  -> FastCDC chunks -> spool (if needed)
                                     -> POST /v1/sync/has, /v1/sync/flush
                                             |
   flopwire grep|search|sessions|read, flopwire mcp  (read-only opens, no lock)
```

## Server flow

```text
device --TLS (pinned self-signed | ACME | operator proxy)--> flopwire serve
   |
   POST /v1/sync/has    which chunk hashes are missing
   POST /v1/sync/flush  header + chunk bodies + provisional tail (<= 80MB)
   |
   ingest: tombstone check -> stream chunks to S3 (chunks/<2 hex>/<blake3>)
           -> one transaction: commit chunk reservations, append manifest
              entries, replace the provisional tail, bump parse request
   |
   parse queue (4 workers, durable in source_parse_state)
     -> rebuild the changed bytes from S3 chunks + tail
     -> parsers -> admin path-rule gate
          covered: refuse the source (no rows, tombstone, chunks released)
          allowed: upsert conversations and messages, supersede vanished rows
   |
   Postgres messages: text (lz4 TOAST), tsv (GIN), trigram (GIN)
   |
   GET /v1/grep|search|sessions|read|raw  (budgeted read-only transactions)
   |
   flopwire <tool> --server, flopwire mcp --server, admin console (no search)
```

## Data model

The same model exists in SQLite on the device and in Postgres on the
server. The device holds only its own sources.

| Entity | Meaning |
|---|---|
| Source | One transcript file or store on one device: `(device, path, file_id)`. It has an agent, a storage kind (JSONL append, document, SQLite export, companion), a parser version, an optional parent (subagent, companion) and an optional `previous` (a Codex rollout renamed into `archived_sessions/`, D20). |
| Generation | One version of a source. Appends extend the current generation. A rewrite, truncation or file-identity change starts a new one. |
| Chunk | Content-addressed bytes, keyed by BLAKE3-256. Stored once in S3, whatever the number of sources and users that hold it. |
| Manifest entry | `(source, generation, ordinal) -> (chunk, byte offset)`. Concatenating a generation's chunks in order rebuilds it. The unfinished end of a file is a provisional tail, kept in Postgres until it closes. |
| Conversation | One harness session on one device: agent, session id, user, device, cwd, repo, title, times, and a parent link and depth for subagents. |
| Message | One record a parser emitted: kind, role, tool name and call id, `is_error`, time, text, native id and part, ordinal, and provenance (generation, line, byte offset and length). |

Message kinds:

| Kind | Holds |
|---|---|
| `user`, `assistant` | Prompts and replies |
| `tool_call`, `tool_result` | Tool invocations and their output. Every tool result gets a row, empty or not, with `is_error` from the exit code (D19). |
| `thinking` | Visible reasoning |
| `system` | Harness system records |
| `agent_message` | Messages between Codex agents (D6) |
| `injected` | CLAUDE.md, AGENTS.md and system-reminder text injected into user turns (D6). Hidden unless a query names it. |

Row rules:

- Ids are stable. Claude rows are `uuid#<block index>`, and the ordinal
  comes from the file position (`offset<<12 | block`) (D16). A parser
  version change re-parses from the original bytes and replaces rows only
  when that succeeds.
- A message absent from a new generation is marked `superseded`, never
  deleted. A changed text inserts a new version and supersedes the old.
  Search returns live rows unless `--include-superseded`.
- `on_active_path=false` marks rows off the chosen branch (Devin alternate
  copies, P1). Search leaves them out unless `--include-branches`.
- A transcript file the harness deletes keeps its rows live and
  searchable (D1). A deleted Devin session is still superseded.
- Tool text is stored whole, with a safety bound of about 1MB per row
  (head, tail and error lines).
- `user_id` and `device_id` come from the uploading credential, never
  from transcript content.

Placements exist only on the device (`placements` table): for each
session, its cwd, worktree root, main checkout, normalized remote, the
method that placed it (`cwd`, `worktree`, `folder`, `remote`, `branch`,
`worktree-add`, `commit`, `unplaceable`) and any ambiguous candidates.
They survive a local reindex.

## Local index

- Files: `index.db`, `index.db-tok`, `index.db-tri0`, `index.db-tri1`,
  and `index.db.lock`. Default location `<user cache dir>/flopwire/`;
  `FLOPWIRE_INDEX` or `--db` overrides it.
- Shards: `fts_tok` uses the unicode61 tokenizer with `_-./` as token
  characters, so paths and flags stay whole; it serves ranked `search`.
  `fts_tri` is a trigram table split over two files by message id; it
  serves `grep`. Both are contentless and index the whole stored text (D3).
- One writer (D12). The agent holds an exclusive flock on
  `index.db.lock`. A second `agent run` fails with
  `agent already running (pid N)`. `agent run --once` asks the running
  agent for a pass over the control socket.
- Readers open read-only (`query_only`), take no lock and never replay
  the queue (L3).
- Writes reach the shards through `fts_queue`, a sequence log. Each shard
  records the last sequence it applied and replays the rest at open. A
  shard that cannot catch up, or a lost shard file, is rebuilt from
  `messages` (L2, L4).
- Schema version bump means reindex. On a version mismatch the next
  writing open drops the index tables and re-parses every transcript.
  Placements are kept. A reader on a stale index gets an error saying the
  agent rebuilds it on its next start. There are no migrations of the
  local index.
- After a first pass of more than 200 sources, the agent re-executes
  itself once to return the bulk-load memory, keeping the lock.

## Sync

| Aspect | Behaviour |
|---|---|
| Chunking | FastCDC: 128KB minimum, 512KB average, 4MB maximum. Unchanged prefixes of rewritten documents dedupe. |
| Request size | At most 4MB of compressed chunk bodies per request (`MaxRequestBytes`). Bodies travel as zstd frames; the server decodes each (refusing output past 16MB), checks its BLAKE3, and stores the frame as the object. Tails travel and rest uncompressed. The server accepts up to 80MB per flush and scales its read deadline with the body size. |
| Has check | When a request would carry more than 1MB of chunks the server was never seen to hold, the client asks `/v1/sync/has` first. |
| Cadence | Append-only files: 300ms debounce, 2s maximum wait. Documents and exports: 2.5s, 10s maximum. A hook flush sends at once. |
| Watermark | The uploaded offset advances only on server acknowledgement. For append-only files the file itself is the queue. |
| Spool | `<config dir>/flopwire/spool`, 1GiB cap (`--spool-cap`). It holds only bytes the source may destroy: intermediate versions of rewritten documents, and chunks of a file about to be rewritten. When full, captures of rewritten sources pause; append-only sources continue. Orphan spool files are cleaned at start (S6). |
| Salvage | Unacknowledged versions are salvaged into the spool, including those whose chunks the server already holds (S1). |
| Gaps | Bytes that are gone before upload are recorded as a gap: the generation is cut back and the archive shows the hole. The local index still has the text. |
| Retry | Per source (D15): one failing source delays only itself. Backoff from 1s to 30s, per source and for the server. A hook flush resets both. Failing sources show in `flopwire agent status`. |
| Seal | A provisional tail seals after 5 minutes with no change, for files and Devin exports alike (D21). |
| Pin error | A fingerprint mismatch is permanent. The scheduler stops all uploads, keeps the queue, and shows the error in `agent status` until `flopwire login --fingerprint` re-pins or the agent restarts. |

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
- Every client connection (admin commands, `enroll`, `--server` queries,
  the raw fallback, device sync, the admin-rules fetch) checks the pin
  before it sends a credential. The pin replaces chain and hostname checks.
- The client refuses `http://` to any host that is not loopback, and all
  `http://` once a pin is saved.
- Re-pin: when the server's TLS directory is lost, the server has a new
  identity. Each device runs `flopwire login --fingerprint <new>`; the
  running agent picks up the new pin and resumes uploads.
- Client timeouts: 10s dial and handshake, 75s for response headers, and a
  per-request deadline of 90s plus one second per 64KB of body.

## Path rules and placement

Decision D18. Details and syntax: [agent.md](agent.md#keep-sessions-out-with-path-rules).

- **Agent (primary).** The agent places each session (cwd, git worktree
  root, main checkout, origin remote, with fallbacks for deleted worktrees)
  and decides with the user rules, the admin rules and the `unplaceable`
  setting before it indexes and before every hand-over to sync. The most
  restrictive rule wins; admin rules are floors. A new local `deny` purges
  matching rows from the local index; it never deletes server copies.
- **Server (floor).** At parse time the server decides each session with
  the admin rules only, from what the transcript recorded: cwd, the Codex
  remote, the Claude project folder name, and the home directory the device
  reports. A covered session is refused: no rows, the source is tombstoned
  and its unshared chunks are released. The device learns of it on its next
  flush of that source (`flopwire agent status`). The server cannot resolve
  worktrees or symlinks, so it can miss what the agent catches.
- **Rule change on the server.** Changing `path_rules` or `unplaceable`
  bumps a rules version. The parse queue's sweep re-checks every stored
  conversation. One the new rules cover is hidden, with its subagents and
  the same session on the owner's other devices: retrieval leaves it out
  at once. An admin previews and confirms the purge
  (`flopwire admin policy preview|purge`, `GET /v1/admin/policy/hidden`,
  `POST /v1/admin/policy/hidden/purge`), or the sweep purges it after 7
  days, through the deletion path below. A later rule change that no
  longer covers it restores it; a purge re-checks the current rules first.
  New uploads to a hidden session are stored hidden, so a restore is
  whole. Every hide, restore and purge is audited (`conversation.hidden`,
  `conversation.restored`, `conversation.purged`).
- **Home directory.** Each flush carries the device's home, Claude
  projects and Codex directories; the server uses them for `~` in admin
  rules. Before a device reports them, the server infers the home from the
  transcript path, which is wrong when `CLAUDE_CONFIG_DIR` or `CODEX_HOME`
  lies outside the home.

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

A harness deleting its file deletes nothing (D1). A local `deny` rule
deletes only local rows. The one automatic purge is of sessions an admin
path-rule change hid, after 7 days.

A backup copies what exists at that moment: a hidden session, or raw
evidence uploaded but not yet refused by its parse, can be in it.

## Backup and restore

- `flopwire backup` takes one Postgres snapshot, dumps the database from it,
  lists the committed chunks from it, and copies and verifies each object
  (BLAKE3). It writes `manifest.json` last. It holds a shared purge lock:
  uploads continue, deletions take effect at once, and physical purges
  wait.
- `flopwire restore` needs empty Postgres and S3 targets, re-verifies every
  object and checks the restored chunk inventory.
- Durable state: identity, audit, policy, sources, generations, chunk
  ledger, manifests, provisional tails, tombstones, and the S3 objects.
  Conversation and message rows are derived and can be rebuilt by replaying
  manifests through the parsers.

## Query budgets and caps

Decision D8.

| Limit | Local | Server |
|---|---|---|
| Time budget | 10s default, `--timeout` up to 60s | Same; each call runs in a read-only transaction with a matching `statement_timeout` |
| Out of budget | grep and search return partial hits plus one line saying what was checked; never an error | Same for grep and search; `sessions`, `read` and raw reads fail with `504` |
| Byte budget | 1GiB of text verified per grep | 200,000 candidate rows per grep |
| Ranked search | Ranks the newest 20,000 matches of a very common term, with the note `ranked newest 20k of N matches — add terms or filters` (D2) | Ranks in Postgres (`ts_rank_cd`); no 20k cap |
| Unindexable regex | Scans the newest 20,000 rows that pass the filters | Refused with `400`: add a literal of 3 or more letters or digits |
| Concurrency | MCP runs up to 8 calls at once and honours `notifications/cancelled` | 16 retrieval requests in flight, then `429`; the pool has 16 + parse workers + 24 connections; other statements stop after 5 minutes |

"Newest" means message time everywhere (D7).

## Self-exclusion

Decision D4. An agent searching the index would find its own call within a
second. `grep`, `search` and `sessions` leave out the calling session and
its subagents when detection finds it by exact evidence, first match wins:

1. `FLOPWIRE_SESSION_ID` (with optional `FLOPWIRE_AGENT`).
2. An ancestor process's Claude Code session file, `~/.claude/sessions/<pid>.json`.
3. An ancestor `codex` process holding exactly one rollout file open.
4. `CLAUDE_CODE_SESSION_ID`.

MCP calls always apply it. The CLI applies it only when stdin is not a
terminal. The output names the excluded session. `--include-self` turns it
off. Code: `internal/retrieval/local/caller.go`.

## Message bus

Design: [notes/message-bus/plan.md](../notes/message-bus/plan.md). Only
the server side is built. Routes and wire types are in `internal/busproto`.

- **Presence.** Each device holds one long poll (`POST /v1/bus/poll`, up to
  25 s). The request carries every live session on the device (id, agent,
  repo, branch, busy) and replaces what the server held. A session is live
  for 75 s after the poll that reported it. A session id that is another
  person's, uploaded or in their presence, is not recorded.
- **Send.** The sending session must be live on the calling device or
  uploaded from it. `to` is a session id prefix (4+ characters, unique) or
  `@user`. The server sets the envelope (session, person, agent, repo,
  `own` or `teammate`, thread, time), redacts the body (4,000-byte cap) and
  sets a 24-hour expiry. A message from another person is held until the
  recipient accepts the sender (`/v1/bus/accepts`, login session only).
- **Limits.** A reply to a `done` message, more than 8 messages per thread
  per hour, 30 sends per session per hour (120 per device and 300 per
  person), the same body to the same recipient within 10 minutes, and 50
  undelivered messages per recipient are refused. A refused message is stored as `refused`.
- **Delivery.** The poll answers the device's whole deliverable set:
  messages to its sessions, and `@user` messages it may claim. A claim is
  atomic. An ack sets `delivered_at`. `read_at` is not set yet.
- **Audit.** Send, claim, ack, accept and revoke commit with their audit
  event (`bus.send`, `bus.claim`, `bus.deliver`, `bus.accept`,
  `bus.revoke`). Peers, inbox and polls that return messages are audited
  like other reads. Bodies are never in the audit log.
- **Expiry.** A sweep each minute marks undelivered messages past their
  expiry `expired` and drops presence a day old. The recipient's inbox
  lists an expired message from another person only while the recipient
  accepts that person, so a held message never reaches it.

## Trust boundaries

- The device agent decides what leaves a machine. Path rules are the only
  filter; nothing is redacted.
- The server trusts only the credential for identity. A device credential
  uploads and reads; a service credential only uploads; admin routes need
  a human admin's login session, never a device token.
- TLS with a pinned fingerprint, ACME, or the operator's proxy protects
  transit. No credential is sent before the pin check.
- The server and host administrators see plaintext. Nothing is encrypted
  at rest by flopwire.
- Every member reads the whole corpus. Every hit and raw read carries its
  user, device, harness, session and repo (D10).
- Retrieval and admin reads are audited with the query and result ids.
  Security-sensitive operations fail closed when audit fails.
- Message rows and indexes are derived and can be rebuilt; S3 chunks and
  manifests are the evidence.

## Schema

`migrations/*.sql` hold the whole schema. The server applies pending files
in one transaction under an advisory lock and records each file's checksum
in `flopwire_schema_migrations`. It refuses a database whose ledger has
edited, missing or unknown entries, and a database with application tables
but no ledger. The product is pre-release, so the migration files are
edited in place: a database made by an earlier pre-release build is
refused. Back it up and start from an empty database.
