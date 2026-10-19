# Flopwire retrieval: transcript capture, indexing, and search

The design of Flopwire's parser library, device agent, server ingest and
search. Code comments cite its sections as "spec §N".

## 1. Summary

Flopwire keeps its durable core (identity, devices, credentials, the S3 raw
archive, backup, audit, policy) and uses its own parser, its own message
index, and its own device agent for retrieval, in place of CASS.

Why: CASS is a search-server design. On an 18GB corpus it holds 55GB on
disk, reaches 2 to 8GB of memory on incremental runs, has a load governor
that ignores memory, and freezes the first version it sees of any message
that is later rewritten. Only 2% of the corpus is prose and under 10% is
text anyone searches for. A 157-line SQLite FTS5 prototype indexed the whole
corpus in 162 seconds at a 253MB peak.

The parts:

- A **Go parser library**, Flopwire's own, that turns every supported
  transcript format into one message model with stable ids, versions,
  provenance, and subagent links. It covers Claude Code (including Claude
  Desktop's Cowork and Code-tab sessions, which store native Claude
  transcripts), Codex, Devin, and opencode. Format knowledge is ported by
  hand from permissively licensed projects (`franken_agent_detection`
  0.3.1, agentsview, Entire, Codecast, google/codesearch; section 5.1),
  each ported file headed with its source and pinned commit. There is no
  runtime sidecar: formats without a native parser are not ingested. FAD
  and agentsview run only in tests, as parity oracles.
- A **device agent** that tails sources with a stat gate, keeps a local
  SQLite index for instant offline search, and ships content-defined chunks
  plus manifests to the server within seconds of activity.
- A **server** that stores chunks in S3, manifests and message rows in
  Postgres, and serves ranked and exact search with context and raw-byte
  retrieval.
- **No semantic search.** Agents get BM25 ranking, exact substring and
  regex search, filters, and raw-byte reads.
- A **message bus** across harnesses, designed in
  [`../message-bus/plan.md`](../message-bus/plan.md).

The same code serves one person on two laptops, with the server on one of
them ([`docs/two-laptop.md`](../../docs/two-laptop.md)), and a team.
Redaction (section 6.6) masks secrets in every byte that leaves a device.

## 2. Evidence

### 2.1 The corpus

Measured 2026-09-28 on a 16GB MacBook running 15 to 20 concurrent agent
sessions.

| | |
|---|---|
| Raw transcripts | 18.2GB, 13,728 files |
| Codex CLI | 11.9GB, 6,980 files |
| Claude Code | 6.2GB, 6,687 files, of which 3,814 are subagent files |
| Devin (agentboard JSONL mirror) | 79MB |
| Other harnesses | under 50MB combined |
| Files over 20MB | 130, holding 5.4GB; largest 213MB |
| Bytes in lines over 100KB | 77% |

Byte shares from a stratified 42-file sample:

| Content | Share |
|---|---|
| Duplicate logging (Codex `event_msg` mirrors of `response_item`; Claude `toolUseResult` mirrors `tool_result`) | 35% |
| Tool outputs | 22% |
| Base64 images | 15% |
| Envelopes, metadata, token counts | 16% |
| Codex `compacted` records (12 to 35 item `replacement_history` plus `retained_context`, 70 to 100KB per line, 7 to 21 per file) | 5% |
| Reasoning signatures, encrypted content | 5% |
| User and assistant prose | 2% |

Searchable text after capping tool input and output at 4KB and dropping
duplicates, images, compaction replays, and signatures: 1.76GB (9.7%). Tool
output is 58% of that; prose alone is about 0.4GB.

What the harnesses write, measured on five busy sessions each:

- **Claude Code:** one JSONL line per content block. An assistant message
  spans 1 to 11 lines sharing `message.id` (thinking, text, tool_use),
  written once each when the block completes. No streaming deltas. 13 to
  24 lines per active minute, peaks of 70 to 77, median gap between lines
  0.02 to 0.26s, bursts separated by tool-run pauses (tool_use to
  tool_result p50 0.7s, p90 40s). Line sizes p50 732B, p99 13KB, max 1.2MB.
  About 45% of lines are not conversation (`attachment`, `mode`,
  `permission-mode`, `last-prompt`, `ai-title`, `queue-operation`, and
  similar). Large tool outputs are written whole to
  `<session>/tool-results/` (613MB here: 961 `.txt`, 981 `.jpg`, 272
  `.pdf`); the JSONL holds only a 2KB `<persisted-output>` preview.
- **Codex:** 27 to 38 lines per active minute, peaks to 86. 28% are
  `event_msg/item_completed`, 30% are `token_count` and
  `token_usage_record`. Only 46% of `item_completed` records share an id
  with a `response_item`; the rest are UI items (`CommandExecution`,
  `FileChange`, `Extension`, `ContextCompaction`) with no response
  counterpart. `event_msg` is written before its `response_item`, 2 to
  200ms earlier. No streaming deltas. Every line carries a top-level,
  monotonically increasing `ordinal`, and every `response_item` has a
  `payload.id` (prefixes `ctc`, `rs_`, `fc_`, `fco`, `msg`, `ams`).

A line is finer than a turn and is the finest durable unit any harness
writes. Hooks (Claude `PostToolUse`, `Stop`, `UserPromptSubmit`; Codex
equivalents) fire at the same moment the line lands and carry no extra
content. They are flush signals, not a content source.

### 2.2 CASS on this corpus

CASS ingests everything into a canonical SQLite archive, then builds a
separate BM25 index. Search reads only the lexical index, with no fallback
to raw files. On this corpus its data directory held 55GB: a 39GB
content-addressed raw mirror, a 6.7GB database, 7.4GB of live index and
1.8GB of stale index generations. Incremental indexing reached 2.1 to
4.6GB of memory and starved every agent session on the machine; its
responsiveness governor reads load average and Linux pressure-stall info
only, so on macOS it is blind to swap. Its raw mirror uses fixed-size
chunks, so one inserted message shifts every later boundary and nothing
dedupes across versions.

CASS matches re-read messages by array position and keeps the first
version it saw of any message whose content later changed. Streamed and
later-filled messages are frozen at whatever state was first indexed.

### 2.3 How other products do it

Mosaic, Entire, Codecast, Lore, claude-mem, agent-session-index and Mem0
were surveyed. None full-text indexes raw tool output at scale, and nobody
publishes numbers near 18GB. The common pattern is to store the raw
transcript as a blob, index a reduced form, and keep a pointer back.
Codecast's live capture (byte-offset watermarks per source, a 300ms
debounce with a 2s maximum wait, a disk retry queue deduplicated by
message id) is closest to Flopwire's.

### 2.4 Prototype

`ftsidx.py` in this directory: 157 lines, standard library, SQLite FTS5.
Streams each JSONL file from a saved byte offset, extracts prose plus capped
tool text, inserts into a contentless FTS5 table with a provenance row per
message, recovers snippets by seeking into the source file.

| Run | Wall | Peak RSS | Result |
|---|---|---|---|
| Full corpus, 18.2GB | 162s | 253MB | 1.62M rows, 654MB index |
| Last 24h of changes, 82 files, 116MB | 0.85s | 43MB | |
| No-change pass over 13.7k files | 5.2s wall, 0.24s CPU | 27MB | |
| Query, warm | 1 to 2ms | 18MB | |

The 253MB peak is parsing single 10MB image lines; skipping them before
parsing brings it under 100MB. `shape.py` produced the byte breakdown.

## 3. Architecture

```text
harness files on each device (never copied on the device)
  -> device agent: stat-gate sweep + FS events
       -> parser library -> local SQLite index (FTS5 + compressed text)   [local search, ~1s]
       -> content-defined chunker -> upload queue (watermark; spool only for bytes the source may destroy)
  -> server API (authenticated, idempotent)
       -> S3: finalized content-addressed chunks                           [raw evidence, only backup needed]
       -> Postgres: manifests per file generation, provisional tail bytes
       -> parser library over the changed bytes -> Postgres message rows   [team search, ~3 to 10s]
  -> CLI / MCP: local index by default, server on request
  -> message bus: MCP send/inbox + per-harness hook delivery
```

Roles:

- **S3** holds every version of every transcript as deduplicated,
  finalized chunks. It never holds a provisional tail. It is read only to
  rebuild Postgres and to fetch raw bytes on demand. It is the one thing
  that must be backed up, together with the identity tables.
- **Postgres** holds identity, manifests (the map from a file generation to
  its chunks), the provisional tail bytes of each open generation, and
  message rows with the full-text index. Message rows are derived and
  rebuildable from S3.
- **Local SQLite** on each device holds the same message model for that
  device's own sources. Derived, rebuildable from the local files in
  minutes, and it keeps the extracted text so search survives the harness
  deleting a transcript.
- **No mirrors.** The harness files and S3 are the only copies of raw bytes.
  Reconstruction of a file from chunks happens transiently, in memory or a
  temp path, only while parsing or serving a raw-byte request.

The server may live anywhere reachable: a VPS, a home machine, or one of
the laptops. The device agent never assumes it is up.

## 4. Data model

One model in SQLite locally and Postgres centrally. The Postgres schema
is [`migrations/`](../../migrations/) (`001_schema.sql` onward); the
SQLite schema is `internal/localindex/schema.go`, with integer row ids,
unix-millisecond times and JSON text in place of `jsonb`.

### 4.1 Sources and generations

- `sources`: one row per file (or database, or directory) a parser reads,
  unique on (device, path, file identity). It records the agent, the
  path as seen on the device, the file identity (device + inode), a
  session key read from content for rewrite-prone formats, the storage
  kind (`jsonl_append`, `json_doc`, `sqlite`, `dir`, `markdown`), and the
  parser name and version.
- `generations`: one row per version of a source, with its size, change
  time at capture, and whether a provisional tail is still open.
- `chunks`: finalized content-addressed chunks, keyed by BLAKE3 hash, each
  stored once in S3.
- `manifest_entries`: the ordered finalized chunks of a generation, with
  each chunk's cumulative byte offset.
- `provisional_tails`: the bytes past a generation's last finalized
  boundary, under the chunker maximum and typically kilobytes.

A generation is a version of a file. For append-only files most
generations are extended in place: finalized entries are appended and the
provisional tail row is overwritten on each flush. A rewrite, truncation,
or file-identity change starts a new generation. Any generation is
reconstructible by concatenating its chunks in ordinal order, then its
provisional tail if one exists. S3 holds only finalized chunks, so an
object is never overwritten or left as garbage from an abandoned tail. On
the device, the tail is re-read from the source file; it enters the local
spool only when the source may destroy it before acknowledgement (section
6.5).

### 4.2 Conversations and messages

- `conversations`: one row per harness session, unique on (device,
  agent, session id). The user comes from the uploader's credential,
  never from content. The cwd is resolved to a git root once, when the
  conversation is first seen. `parent_conversation_id`,
  `spawned_by_message_id` and `depth` link subagents (section 4.3).
- `messages`: one row per record the parser emits. Each row carries the
  harness-native id where there is one (Claude line uuid, Codex
  `payload.id`, Devin `chat_message.message_id`, opencode part id), an
  ordinal derived from the locator, a kind (`user`, `assistant`,
  `tool_call`, `tool_result`, `thinking`, `system`, and a few more), tool
  name and call id, the capped extracted text, a hash of the uncapped
  text for change detection, a version, the supersession and active-path
  flags below, Codex `codex-events@1` enrichment, and provenance: source
  generation, line number, byte offset and length, or a locator for
  non-JSONL storage, plus the parser that wrote it. Postgres keeps a
  `tsvector` with a GIN index for ranked search and a trigram index for
  `grep`; SQLite keeps the two FTS5 tables of section 6.3.

Rules:

- **Kinds.** Every record the parser emits is a message row, including tool
  calls and tool results. Tool text is stored whole (decision D3), with a
  safety bound of about 1MB per row: head 768KB, tail 192KB, plus up to
  64KB of lines matching error, exception, `path:line`, or exit-code
  patterns from the middle. On the 2026-09-29 corpus only 19 of 1.28M tool
  rows exceed it. Caps are per-kind settings, not architecture.
- **Skip on extraction:** Codex `event_msg` (except the `codex-events@1`
  enrichment in section 5.4), `turn_context`, `token_count`, `compacted`
  items whose hash was already seen in the session; Claude `toolUseResult`
  mirrors, `attachment`, `mode`, `permission-mode` and other
  non-conversation records; base64 and data URLs; images; thinking
  signatures and encrypted reasoning; file-history snapshots. Index the
  compaction summary message once. Parsers skip unknown record types and
  never fail on them; section 5.4 lists nine
  that appeared recently in Claude transcripts.
- **Identity.** Upsert by `native_id` where the format has one. Where no
  id exists, key on (source, locator). Positional index is never a key. Claude rows keep the
  per-line `uuid`; lines sharing `message.id` are not merged.
- **Keep everything.** Every record the parser understands becomes a row,
  including branches, retries, and history a harness later drops. Two
  per-row facts decide what default search shows:
  - `superseded`: the row is absent from the latest generation of its
    source (rewrite, truncation, deletion, a deleted Devin session), or a
    newer version replaced it.
  - `on_active_path`: set only from a pointer the harness itself writes.
    Devin `sessions.main_chain_id` is the only one. Null for Claude,
    Codex, opencode, and every harness without such a pointer.
- **Default search filter:** `not superseded and on_active_path is not
  false`. `include_superseded` drops the first condition;
  `include_branches` drops the second. Both are filters on search, find,
  and context, not separate stores.
- **No branch classifier.** Flopwire does not infer an active path where the
  harness records none. Claude's `parentUuid` tree has no pointer, and a
  naive leaf walk misclassifies parallel tool fan-out: of 412 off-path
  lines in 60 files, 205 were tool_results and 21 tool_uses from fan-out,
  and real rewinds accounted for 2 prompts.
- **Versions.** Latest live plus superseded finals. A new version whose
  text has the previous version as a strict prefix replaces it in place
  (streaming growth, tool results filled in later). Otherwise the previous
  row is marked superseded and a new row becomes live.
- **Deletions.** A message absent from a new generation is marked
  superseded with `superseded_in_generation`, never deleted. Claude's
  compaction cleanup, Codex migration rollback, Devin session deletion,
  and opencode reverts all drop history from the source; the index keeps
  it.
- **Ownership.** The index stores the compressed extracted text, so a
  superseded row and a deleted source both remain searchable and
  renderable. Ranking never reads the text; it is decompressed only for
  the hits shown.

Size estimate for the measured corpus: 1.62M rows, about 200MB of fixed
columns, 0.5 to 0.7GB compressed text, 0.6 to 1GB GIN index, under 100MB
of manifests: **1.5 to 2.5GB of Postgres** and 12 to 14GB of deduplicated
chunks in S3, against 55GB for CASS.

### 4.3 Subagents

Subagent transcripts are ordinary sources with a parent link. FAD
implements none of this linkage beyond Claude's path-derived parent, so
the rules below come from the files themselves and from agentsview,
Codecast, and agentboard's `subagentLogs.ts`:

- **Claude Code:** discovery is recursive:
  `<session-id>/subagents/agent-<agentId>.jsonl` and
  `<session-id>/subagents/workflows/<runId>/agent-<agentId>.jsonl` (1,238
  workflow files here). The parent link comes from the sidecar
  `agent-<agentId>.meta.json` (`toolUseId`, `spawnDepth`, `parentAgentId`,
  `agentType`, `description`), which is uploaded as a companion file.
  Fallback: the parent's tool_result carries `toolUseResult.agentId`. The
  agentId is on the tool_result, not on the Agent tool call.
- **Codex:** flat rollout whose `session_meta.source.subagent.thread_spawn`
  names `parent_thread_id`, `depth`, and `agent_path`. Distinguish
  `thread_spawn` children from `codex exec` review children and from forks
  (Codecast documents the cases). `state_5.sqlite` `thread_spawn_edges` is
  a cross-check, not a source.
- **Devin:** a `run_subagent` subagent writes its nodes into the same
  `sessions.db`, on roots of their own (`parent_node_id` null), when it
  finishes. The `subagent_heads` table is empty and unused.
- **opencode:** a subagent is its own session row whose `parent_id` names
  the parent session; the parent's `task` tool part that spawned it is
  `spawned_by_message_id`.

The parser sets `parent_conversation_id`, `spawned_by_message_id`, and
`depth`. Arrival order does not matter: a child that lands before its
parent stores the parent's native id and resolves when the parent arrives.
Search includes subagent rows by default with a filter to exclude them.
The context view can descend from the parent's tool call into the child
transcript and back to the tool result.

## 5. Parser library and format coverage

### 5.1 Borrowing

Flopwire writes its own Go parsers on the section 4 model. Format knowledge is
ported by hand, never vendored. Each ported file carries a header naming
the source repository, path, and pinned commit. Upstream MIT and BSD
notices are kept in `third_party/<name>/LICENSE` and `NOTICE`. This is a
reading of the license texts, not legal advice.

| Source | License | What Flopwire takes | How |
|---|---|---|---|
| `franken-agent-detection` (FAD) 0.3.1, git `c06d1cb` | MIT, no rider | Connector format knowledge for every harness; test-time oracle (section 5.2) | Port. 0.3.1, not 0.3.0: it fixes Claude `queued_command` prompts, Codex integrity and partial-read handling, and capped reads |
| kenn-io/agentsview (Go) | MIT | Long-line reader; Claude subagent and persisted tool-result resolution; Codex `call_id` pairing and checkpoint/anchor logic; Devin `main_chain_id` walk; secret rules (section 6.6); second oracle | Reference and port; do not vendor |
| entireio/cli | MIT | Claude token dedupe by `message.id`; `apply_patch` path extraction; Codex `compacted` sanitising; fixtures | Port |
| codecast-sh/codecast (TS) | MIT | Codex subagent edge cases: `thread_spawn` vs `codex exec` review children vs forks | Port |
| google/codesearch | BSD-3 | `index/regexp.go` `RegexpQuery`, the regex-to-trigram planner for `find` (section 8) | Port |
| CASS; mcp_agent_mail | MIT with OpenAI/Anthropic rider | Nothing | Copy nothing, including fixtures. The rider forbids derivative works reaching those companies and defines analysis as use. CASS's connectors are thin wrappers over FAD anyway |
| trufflehog, claude-squad, grafana loki | AGPL | Nothing | Do not copy |

Why not vendor agentsview: a spike carried 27.6k lines into the tree for a
model that does not fit. It merges Claude lines by `message.id` and keeps
59% of assistant uuids, drops system lines and fork branches, keeps none of
18,688 Codex `payload.id` values, and reads only Devin's main chain (29% of
nodes). The relevant files change about 10 times a week upstream, and its
roadmap (issue #1352) converges on Flopwire's server, not on a library.

FAD's own model is positional (`idx` after filtering and sorting), with
tool calls flattened into content text and native ids reaching `extra`
inconsistently. It records no byte offsets, line numbers or versions, and
collapses supersession. The Go port keeps FAD's format knowledge and
discards its message model in favour of section 4.

Libraries: `modernc.org/sqlite` (no cgo; FTS5 `trigram` and
`contentless_delete` verified), `zeebo/blake3`,
`PlakarKorp/go-cdc-chunkers` (FastCDC), `klauspost/compress/zstd`, and the
`pgx` v5 and `minio-go` v7.

### 5.2 Coverage strategy and parity oracles

Native parsers exist for Claude Code, Codex, Devin, and opencode. A format
without a native parser is not ingested; there is no runtime sidecar.
Native parsers are what give offsets, stable ids, tailing, and versioning;
a sidecar's positional rows would need re-ingesting anyway. A new harness
gets a native parser when there are users and data to test against.

Parity oracles run only in tests (`internal/transcript/oracle`):

- **fad-dump.** `tools/fad-dump` pinned to FAD 0.3.1 and extended to emit
  `extra` and `invocations`. `testdata/fad/home/` holds each agent's
  native layout (SQLite as `.sql` seeds); `scripts/regen-oracle.sh`
  regenerates expected JSON under `env -i HOME=...`, the Go parsers read
  the same home, and the tests assert that discovered sources, session
  and workspace, and the (role, content, created_at) sequence match.
- **agentsview.** Its binary and its MIT parser testdata as a second
  differential oracle: per-session counts, roles, and ids.
- **Real corpus.** Both oracles also run against a sampled slice of the
  real corpus. Every divergence is documented per agent.

Oracle limits, which is why id-level parity uses small fixtures:

- FAD compacts `extra` and drops native ids for Claude and Codex files of
  32MiB or more.
- FAD's Codex connector errors on files over 100MiB (it drops them whole)
  and on a partial final line, and skips `archived_sessions/`.
- FAD Claude tool-result rows carry a synthesized `extra` without the line
  uuid.
- FAD never reads Codex `session_meta.id` or `thread_spawn`.
- agentsview's model differs (see 5.1), so it checks counts and roles, not
  row identity.

### 5.3 Storage kinds and the append-only assumption

The byte-offset watermark is the fast path, not an invariant. Both Claude
Code and Codex rewrite transcript files in specific cases.

| Connector | Storage | Append-only | Rewrite or truncate cases | Strategy | Present here |
|---|---|---|---|---|---|
| Claude Code | JSONL per session, plus `subagents/agent-*.jsonl` and `subagents/workflows/<runId>/agent-*.jsonl` with `.meta.json` sidecars; `tool-results/` companion files | mostly | Failed streaming message removed in place. Compaction cleanup rewrites the file, moves title/mode records to the top, drops pre-boundary history. About 2% of transcripts got a new inode mid-session. `/compact` itself appends a `compact_boundary` line. Cleanup deletes files over `cleanupPeriodDays` (30 by default) at startup, which has orphaned 1,015 of 2,219 `tool-results/` files here. Resume and `--continue` append. | offset + rewrite check; companion files whole | 6.4GB JSONL, 613MB `tool-results/` |
| Codex CLI | JSONL rollout; top-level `ordinal` per line, `payload.id` per `response_item` | mostly | Live sessions only append (`compacted`, `turn_aborted`). The 0.158 binary has a legacy-rollout migration with rollback, a compression worker, and a truncation path; 124 finished rollouts were replaced whole. Archiving moves files to `archived_sessions/`, which Flopwire follows (FAD skips it). `thread_history_1.sqlite` (2.8GB parsed projection with `item_id`, `rollout_ordinal`, `next_rollout_byte_offset`) and `state_5.sqlite` (`threads`, `thread_spawn_edges`) are cross-checks, not sources. | offset + rewrite check; follow renames | 11GB |
| Devin | SQLite `message_nodes`, autoincrement `row_id`, branching tree; `created_at` in epoch seconds | no | Revert changes `sessions.main_chain_id` with no new rows. Whole sessions are deleted (`sqlite_sequence` at 338k against about 40k live rows; 64 sessions gone). `tool_call_state` rows update in place. | rowid watermark + `main_chain_id` check + session-set diff (vanished sessions become superseded) + `tool_call_state` re-read | 652MB db |
| opencode | SQLite `opencode.db`; session, message and part ids | no | part rows overwritten while streaming; revert deletes | session `time_updated` re-read, upsert by id | not measured |

### 5.4 Parser rules

**Claude Code.**

- Subagent discovery and parent links as in section 4.3.
- `tool-results/`: every file is a companion file of its session and is
  archived to S3, including the 1,015 orphans whose parent JSONL was
  cleaned up. A `.txt` file replaces the 2KB `<persisted-output>` preview
  as the text of its tool_result row, under the normal tool cap applied to
  the full file. Binaries (jpg, pdf, png, docx) are archived, not indexed.
- Line types without a `uuid` that are not conversation are ignored:
  `atis-latch`, `bridge-session`, `pr-link`, `agent-name`,
  `artifact-autoreact-ledger`, `file-history-delta`, `frame-link`,
  `cost-state`, `artifact-comment-monitor`, and any type not yet seen.
- Token usage is deduplicated by `message.id` (Entire's rule); rows stay
  per line.

**Codex.**

- `response_item` is canonical for every row. It is present in every
  rollout version on disk, while the tool vocabulary changed three times
  (`exec_command` in March, `exec` code mode in August, `exec` plus events
  in September).
- `event_msg/item_completed` `CommandExecution` and `FileChange` records
  exist only in 0.15x rollouts (`FileChange` since June). A separate
  versioned sub-parser, `codex-events@1`, reads them as best-effort
  enrichment of the enclosing `exec` call row: `commands[]{cmd, cwd,
  exit_code, duration}`, `changed_paths[]`, and `is_error` set when any
  exit code is nonzero. Events pair with calls by position within
  `turn_id`; 96% land inside exactly one open `exec`.
- Unpaired events (4%: user shell commands, or events written before
  their `response_item`) become their own tool_result rows with
  `tool_name = shell`.
- Event output text is not indexed; it is already in
  `custom_tool_call_output`. `Reasoning`, `AgentMessage`, and
  `UserMessage` mirrors are skipped.
- Enrichment failure degrades the filter fields only; the rows from
  `response_item` are unaffected.

**Devin.**

- Index every message node. Upsert by `chat_message.message_id`, which
  collapses about 10k compaction copies of chain nodes.
- `on_active_path` is true for nodes on `sessions.main_chain_id`, false
  otherwise: about 4k nodes of compacted-away history and about 14k of
  retries, reverts, and orphaned pre-compaction trees.
- A deleted session's rows become superseded; they are never dropped.

## 6. Device agent

One `flopwire` process per device. Replaces the CASS watcher at tens of
megabytes.

### 6.1 Change detection

- **Sweep** every 30 to 60 seconds: `stat` every tracked file. Cost is one
  syscall per file; 13.7k files took 0.25s CPU and 27MB. Hash and parse
  only files whose tuple moved.
- **Gate tuple:** file identity (device + inode), size, change time.
  Change time is the inode ctime, which no user-space writer can preserve
  (verified: a same-size rewrite with mtime restored still moves ctime).
  Never gate on mtime alone: any writer can restore it. The device agent
  runs on macOS and Linux.
- **Racy rule** (from git): a file whose change time equals the previous
  sweep's timestamp is re-hashed regardless.
- **Identity for rewrite-prone documents:** path + inode + a session id
  read from the content. Never a fingerprint of the file head: a
  rewritten document can change its head. (Filebeat and Vector fingerprint
  the head and would break here.)
- **Directory events** (kqueue on macOS, inotify on Linux) on a bounded
  set of active directories wake the agent when a file is created. Files
  are never watched one by one. A fast lane re-stats files that changed in
  the last few minutes, and Devin's `sessions.db` and `-wal`, every 500ms.
  The sweep is the floor and the source of truth.
- **Hooks as flush signals:** every harness hook runs `flopwire hook`,
  which asks the agent to flush the affected source at once.
- **SQLite sources** are polled by rowid or session updated-stamp. Devin
  also checks `main_chain_id`, diffs the session set to detect deleted
  sessions, and re-reads `tool_call_state` rows, which update in place.
  opencode re-reads sessions whose `time_updated` moved.

### 6.2 Watermarks and rewrite handling

Per source, fsync'd in the local SQLite: identity tuple, indexed byte
offset, uploaded byte offset, line count, hash of the last indexed line,
hash of the first 4KB, session key.

Re-index the whole file, as a new generation, when any of: the inode
changed or the file disappeared and reappeared; size is below the indexed
offset; the bytes just before the offset do not hash to the saved last
line; the first-4KB hash changed. Size alone is not enough: an in-place
edit followed by appends leaves the file larger. On re-index, upsert by
native id and mark vanished rows superseded (section 4.2). Hold the file
descriptor open while tailing so an unlinked file stays readable until
drained. Never drop long lines; stream them (Vector's 100KB cap and Fluent
Bit's 32KB buffer would lose the 1.2MB lines in this corpus). The line
reader reports the byte offset and length of every line, including lines
far larger than its buffer, and treats a final line without a newline as
incomplete rather than as an error (agentsview's reader is the reference).

### 6.3 Local index

The section 4 model in SQLite (`modernc.org/sqlite`) with two FTS5 tables:
a token table (`contentless_delete`, unicode61 tokenizer with `_-./` as
token characters so paths and flags stay whole; consider `detail=column`
to halve the index) for ranked search, and a `trigram` table for `find`
(section 8). Text is stored zstd-compressed per row. Rebuildable from local
files at any time. Query via CLI and MCP with ranked, exact-substring, and
filtered modes.

The local index holds the owner's own data and is never redacted
(section 6.6).

### 6.4 Chunking and upload

- **Content-defined chunking** (FastCDC via `go-cdc-chunkers`, BLAKE3
  hashes), not fixed 4MB offsets: 128KB minimum, 512KB average, 4MB
  maximum. Content-addressed by hash. After a rewrite
  only chunks whose content changed have new hashes; the unchanged prefix
  dedupes exactly. Append-only files degrade to "the new chunk is at the
  end".
- **Provisional tail.** Bytes past the last finalized boundary form an open
  tail. Each flush uploads finalized chunks that closed since the last
  flush plus the current tail bytes. The server stores finalized chunks in
  S3 and the tail in Postgres `provisional_tails`, overwriting it on each
  flush; S3 never holds a tail. When the tail crosses a boundary its
  finalized part gets a stable hash, goes to S3 and the manifest, and the
  new tail is re-sent. Worst case per flush is one chunk under the maximum
  size; typical is kilobytes.
- **Flush cadence, append-only files:** trailing-edge debounce of 300ms
  with a 2s maximum wait, on line completion, plus immediate
  flush on hook. No quiet window. Active sessions reach the server in
  seconds.
- **Flush cadence, rewritten documents and SQLite exports:** debounce
  2.5s with a 10s maximum wait. A mid-stream flush uploads a version that existed; the
  prefix rule collapses it when the final arrives.
- **Upload:** one authenticated request per flush carrying new chunks and
  the manifest delta. Idempotent on (source, generation, byte range) and
  on message native id, so a retry after a partial acknowledgement is safe.
- **Uploaded watermark** advances only on server acknowledgement.

### 6.5 Offline behaviour

Server down is a normal state, not an error: with the first server on a
laptop, the other laptop is often disconnected. The watermark is the
queue. For append-only files nothing is copied while the source exists; on
reconnect the agent ships from the uploaded offset.
A spool holds only un-acknowledged chunks the source may destroy:

- intermediate versions of rewritten documents (small deltas, CDC dedupes
  the shared prefix);
- chunks already hashed from an append-only file that a rewrite or
  deletion is about to remove (the open file descriptor covers files being
  tailed; Claude's cleanup runs at startup on a 30-day age, so the window
  is days);
- rows extracted from SQLite sources that may vacuum or delete.

The spool has a size cap that blocks with a loud warning rather than
evicting oldest (Fluent Bit's default evicts and loses data). Local search
is unaffected throughout. If raw bytes are lost anyway, the local index
still holds the extracted text rows; the archive records the gap.

### 6.6 Redaction

[`../redaction.md`](../redaction.md) holds the rules and the mechanism.
In short: the device masks known secret shapes in every byte it uploads,
before chunking, and the server masks again when it parses or serves
archived bytes. Masking is length-preserving, so every byte offset means
the same thing on the device, in the archive and in the local index. Each
marker carries the rule name and, except for password-class rules, a
short hash of the original, so two hits of the same secret still
correlate. Rules are versioned; a rule change does not rewrite history.
The owner's local index is not redacted: a masked secret there would only
stop the owner finding where they pasted it. False negatives are
expected, and the product copy says so plainly.

## 7. Server

### 7.1 Ingest

Order per flush: store finalized chunks in S3 (skip hashes already
present), commit the manifest delta and the provisional tail row in one
Postgres transaction, then parse. Evidence is durable before any derived
work; a parse failure is retried without re-upload. Complete lines in the
provisional tail are parsed like any other bytes, so team search does not
wait for a chunk boundary. For append-only
generations, parse only the new bytes plus enough tail context to complete
a line. For a new generation of a rewritten document, stream its chunks
through the parser and reconcile against existing rows by native id.
Reconstructed files are transient.

Latency, source write to searchable: local about 1s; team about 3 to 10s
(debounce, one round trip, S3 put, manifest commit, delta parse).

### 7.2 Storage

S3-compatible object store for finalized chunks (MinIO in Compose; any
S3). Postgres for identity, sources, generations, chunks, manifests,
provisional tails, conversations, messages. Message text is a `text`
column with lz4 TOAST compression and a low TOAST threshold, so most rows
compress. The GIN index is uncompressed.

Backup: S3 plus the identity, sources, generations, chunks, manifest, and
provisional tail tables. Everything else is derived. Rebuild = replay
manifests (and tails) through the parser.

### 7.3 Retrieval API

The server answers the retrieval tools of section 8 from Postgres, in the
shapes of `internal/retrieval/format`; the local index answers the same
calls from SQLite. Every hit carries its conversation, message, kind,
timestamp and provenance (path, generation, line, offset). Reading
neighbouring rows uses the message ordinal and needs no object store.
Reading raw bytes maps the range to chunks by manifest arithmetic, fetches
one or two objects (or reads the provisional tail), and returns the
bytes; this serves uncapped tool output and exact provenance. Locally,
raw bytes come from the source file.

## 8. Search for agents

The main clients are agents, so the query surface follows how agents
already search: exact strings and regexes, file paths, identifiers, error
text, then reading a few lines of context. That is `grep` with
provenance, plus a ranked mode for "where did we deal with X". There are
no embeddings.

The tools are the same in the CLI and over MCP; usage is in
[`docs/search.md`](../../docs/search.md):

- **`grep`**: exact substring or regex. Backed by a trigram index in both
  stores (SQLite FTS5 `trigram` tokenizer; Postgres `pg_trgm` GIN). For
  regexes, a port of google/codesearch's `RegexpQuery` (`index/regexp.go`)
  plans the pattern into an AND/OR query over trigrams; that query selects
  candidate rows, and Go's `regexp` verifies each candidate. A pattern
  that yields no trigrams (for example `.*`) falls back to a scan bounded
  by the other filters. The trigram index costs roughly 3x the token
  index, accepted because this is the mode agents use most.
- **`search`**: ranked BM25 over prose and capped tool text, for the "I
  don't know the exact string" case.
- **`sessions`**: lists or globs sessions.
- **`read`**: widens a hit into its conversation, or reads raw bytes for
  an uncapped tool output. Every printed address
  (`SESSION/ORDINAL[:LINE]`) round-trips through `read`.

Filters everywhere: agent, repo (resolved git root), device, user, kind,
time range, subagents, `include_superseded`, `include_branches` (section
4.2 defines the default), and self.

**Self-session exclusion.** An agent's own `grep` call, and the output it
gets back, are indexed within about a second, so a naive repeat search
finds the query itself. The tools therefore exclude the calling session's
rows by default.

## 9. Message bus

The message bus design is [`../message-bus/plan.md`](../message-bus/plan.md).

## 10. Operations

- Backup: S3 bucket plus the identity, sources, generations, chunks,
  manifest, and provisional tail tables. Message rows are derived.
- Rebuild: replay manifests through the parser; local index rebuilds from
  local files.
- Deletion: a user or admin deletes a conversation. Its conversation and
  message rows (live and superseded) are tombstoned and excluded from
  search and context; the manifests of its generations are removed;
  chunks are purged when their reference count reaches zero; an audit
  event records who deleted what and when. Tombstones stay so re-upload
  of the same source does not resurrect it silently.

## 11. Acceptance

### 11.1 Two-device end-to-end test (B4)

`internal/e2e` drives a real server (Compose: Postgres, MinIO, flopwire)
and two `flopwire agent run` processes with separate homes, configs and
indexes, and covers offline and reconnect, rewrite and deletion cases.
`make e2e` runs it; [`docs/two-laptop.md`](../../docs/two-laptop.md) is
the real two-laptop setup.

### 11.2 Acceptance bar for the local track

`flopwire bench acceptance` checks these against a device's real
transcripts, read-only; `scripts/acceptance.sh` runs every part, including
the oracle sample:

- The full corpus (Claude, Codex, Devin; about 18GB) indexes in under 5
  minutes with peak RSS under 600MB. The idle agent stays under 120MB. A
  sweep with no changes costs under 1s of CPU. The bulk load runs once per
  device, which is why the peak bound is loose.
- A line written by a live session is findable locally within about 2s.
- Oracle parity tests pass against fad-dump 0.3.1 and agentsview, on the
  fixtures and on a sampled slice of the real corpus, with every divergence
  documented.
- A saved set of about 30 real queries, harvested from past `cass search`
  invocations found in the transcripts, returns the expected hits in under
  200ms each.

## 13. Files

- `ftsidx.py`: the FTS5 prototype. `python3 ftsidx.py DB index --roots`,
  `python3 ftsidx.py DB query 'terms' [--agent codex] [--cwd PREFIX]`,
  `python3 ftsidx.py DB optimize`.
- `shape.py`: corpus byte breakdown by record type.
