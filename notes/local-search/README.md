# Flopwire retrieval redesign: transcript capture, indexing, and search without CASS

Date: 2026-09-28. Status: design spec ready for implementation hand-off;
decisions from review incorporated. Section 11 fixes the milestone and PR
plan.

## 1. Summary

Flopwire keeps its durable core (identity, devices, credentials, the S3 raw
archive, backup, audit, policy) and replaces CASS as the retrieval engine
with its own parser, its own message index, and its own device agent.
Nothing is live yet, so there are no migrations; schemas below replace the
current `segments` table outright.

Why: CASS is a search-server design. On an 18GB corpus it holds 55GB on
disk, reaches 2 to 8GB of memory on incremental runs, has a load governor
that ignores memory, and freezes the first version it sees of any message
that is later rewritten. Only 2% of the corpus is prose and under 10% is
text anyone searches for. A 157-line SQLite FTS5 prototype indexed the whole
corpus in 162 seconds at a 253MB peak.

What ships:

- A **Go parser library**, Flopwire's own, that turns every supported
  transcript format into one message model with stable ids, versions,
  provenance, and subagent links. Wave 1 covers Claude Code, Codex, and
  Devin; Pi/OMP and Gemini follow. Format knowledge is ported by hand from
  permissively licensed projects (`franken_agent_detection` 0.3.1,
  agentsview, Entire, Codecast, google/codesearch; section 5.1), each
  ported file headed with its source and pinned commit. There is no
  runtime sidecar: formats without a native parser are not ingested. FAD
  and agentsview run only in tests, as parity oracles.
- A **device agent** that tails sources with a stat gate, keeps a local
  SQLite index for instant offline search, and ships content-defined chunks
  plus manifests to the server within seconds of activity.
- A **server** that stores chunks in S3, manifests and message rows in
  Postgres, and serves ranked and exact search with context and raw-byte
  retrieval.
- **No semantic search.** Agents get BM25 ranking, exact substring search,
  filters, and ripgrep over raw files.
- A **message bus** across harnesses, designed here and built last.

The first user is Gary, on two laptops, as a personal CASS replacement.
The first server runs on one of those laptops (Compose plus MinIO); the
other syncs to it over the tailnet. The same code is the team product;
redaction (section 6.6) ships before the first team user joins.

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
| Pi, Grok, Antigravity, Gemini | under 50MB combined |
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
separate BM25 index with a from-scratch engine (Quill, formerly Tantivy),
a from-scratch SQLite reimplementation, and a from-scratch terminal
framework. Search reads only the lexical index; there is no fallback to
raw files; a stale index makes `cass search` spawn a background reindex.

| CASS data directory | |
|---|---|
| `raw-mirror` (content-addressed copies of every observed source version) | 39GB |
| `agent_search.db` | 6.7GB |
| `index/v9-quill` (live) | 7.4GB |
| `index/v6`, `index/v8` (stale generations) | 1.8GB |
| Separate `-backup-20260923` directory | 6.4GB |

Memory: 0.8.0 spends 8.3GB of dirty heap in its "preparing" phase before
any tunable stage. `main` at 6b8228b (includes the 2026-09-26 fix for a
JOIN that materialized every message row) drops that to under 1GB, but
incremental indexing still reached 2.1 to 2.4GB idle and 4.6GB during
working hours, at which point it starved agentboard and every agent
session (event-loop blocks of 14 to 35 seconds in agentboard's
instrumentation, coincident to the second). Its responsiveness governor
reads load average and Linux pressure-stall info only; on macOS it is blind
to swap. Upstream issue #496 tracks memory budgets; fixes are partial and
unreleased. CASS's raw mirror uses fixed-size chunks, so one inserted
message shifts every later boundary and nothing dedupes across versions,
which is why the mirror is twice the corpus.

CASS matches re-read messages by array position and keeps the first
version it saw of any message whose content later changed, logging a
"collision". Streamed and later-filled messages are frozen at whatever
state was first indexed.

### 2.3 How other products do it

None full-text index raw tool output at scale. Nobody publishes numbers
near 18GB.

| Product | Index | Engine | Large transcripts | Live capture |
|---|---|---|---|---|
| Mosaic | cloud | undisclosed | undisclosed | undocumented |
| Entire | transcripts in git refs, hosted search | hybrid, backend undisclosed | splits at 50MB; compact transcript beside the full one | hook-driven; copies the whole transcript at each checkpoint; pushes on `pre-push` |
| Codecast | cloud (Convex) | Convex full-text plus semantic | undocumented | chokidar plus 2s rescan; byte-offset watermarks for Claude/Codex/Cursor, message count for Gemini, content signatures for Pi/Grok; `debounceMs 300, maxWaitMs 2000`; uploads parsed messages; disk retry queue deduped by message uuid |
| Lore (loreai) | local SQLite | BM25 + RRF, local embeddings | LLM-distilled 10 to 20x | notify + 500ms debounce; re-imports whole file on growth |
| claude-mem | local SQLite + Chroma | vectors, FTS5 fallback | compressed observations; raw tool I/O capped 64KB | per tool call via hook |
| agent-session-index | local SQLite | FTS5 | excludes tool args, results, base64 | hash-and-skip per file |
| Mem0 | Qdrant | vectors + BM25 | LLM-extracted facts only | per call |

Common pattern: store the raw transcript as a blob, index a reduced form,
keep a pointer back.

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
  -> message bus (built last): MCP send/inbox + per-harness delivery
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

Same model in SQLite locally and Postgres centrally. Column types are
Postgres; SQLite equivalents are obvious.

### 4.1 Sources and generations

```sql
sources (
  id            uuid primary key,
  device_id     uuid not null,
  agent         text not null,          -- claude, codex, devin, gemini, ...
  path          text not null,          -- as seen on the device
  file_id       text not null,          -- dev+inode (POSIX) or volume+file index (Windows)
  session_key   text,                   -- session id read from content, for rewrite-prone formats
  storage_kind  text not null,          -- jsonl_append | json_doc | sqlite | dir | markdown
  parser        text not null,          -- e.g. claude@1, codex@1+codex-events@1
  first_seen_at timestamptz not null,
  unique (device_id, path, file_id)
);

generations (
  source_id     uuid references sources,
  generation    bigint not null,        -- increments on rewrite, truncation, or identity change
  size          bigint not null,
  change_time   timestamptz,            -- inode ctime / NTFS ChangeTime at capture
  captured_at   timestamptz not null,
  complete      boolean not null,       -- false while a tail chunk is provisional
  primary key (source_id, generation)
);

chunks (
  hash          bytea primary key,      -- blake3 of content
  size          integer not null,
  object_key    text not null           -- S3 key, content addressed
);

manifest_entries (                      -- finalized chunks only
  source_id     uuid,
  generation    bigint,
  ordinal       integer,
  chunk_hash    bytea references chunks,
  byte_offset   bigint not null,        -- cumulative offset within the generation
  primary key (source_id, generation, ordinal)
);

provisional_tails (                     -- bytes past the last finalized boundary
  source_id     uuid,
  generation    bigint,
  byte_offset   bigint not null,        -- where the tail starts
  bytes         bytea not null,         -- under the chunker maximum, typically kilobytes
  updated_at    timestamptz not null,
  primary key (source_id, generation)
);
```

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

```sql
conversations (
  id                     uuid primary key,
  source_id              uuid references sources,
  agent                  text not null,
  session_id             text not null,     -- harness-native session/thread id
  device_id              uuid not null,
  user_id                uuid not null,     -- from the uploader's credential, never from content
  cwd                    text,
  repo_root              text,              -- cwd resolved to git root once, when first seen
  title                  text,
  started_at             timestamptz,
  last_activity_at       timestamptz,
  parent_conversation_id uuid,              -- subagent linkage
  spawned_by_message_id  uuid,              -- the tool call in the parent that spawned this
  depth                  smallint not null default 0,
  unique (device_id, agent, session_id)
);

messages (
  id             uuid primary key,
  conversation_id uuid references conversations,
  native_id      text,               -- Claude line uuid, Codex payload.id, Devin chat_message.message_id, Gemini id, Cursor bubbleId, ...
  parent_native_id text,
  ordinal        bigint not null,    -- derived from the locator (byte offset / rowid / array index at first sight)
  kind           text not null,      -- user | assistant | tool_call | tool_result | thinking | system
  role           text,
  tool_name      text,
  tool_call_id   text,
  is_error       boolean,
  ts             timestamptz,
  text           bytea not null,     -- zstd-compressed extracted text (capped per kind)
  text_len       integer not null,
  content_sha    bytea not null,     -- hash of the uncapped extracted text, for change detection
  version        integer not null default 1,
  superseded     boolean not null default false,  -- absent from the latest generation, or replaced by a newer version
  superseded_by  uuid,               -- the replacing version, when there is one
  superseded_in_generation bigint,
  on_active_path boolean,            -- null unless the harness records an active-path pointer
  commands       jsonb,              -- Codex codex-events@1 enrichment: [{cmd, cwd, exit_code, duration}]
  changed_paths  text[],             -- Codex codex-events@1 enrichment
  source_generation bigint not null,
  line_no        integer,            -- JSONL
  byte_offset    bigint,             -- JSONL
  byte_len       integer,
  locator        text,               -- rowid, JSON pointer, or event file for other storage kinds
  parser         text not null,
  tsv            tsvector generated always as (to_tsvector('simple', decompressed text)) stored  -- or maintained by trigger
);
create index on messages using gin (tsv);
create index on messages (conversation_id, ordinal);
create index on messages (native_id) where native_id is not null;
create index on messages (conversation_id, ordinal)
  where not superseded and on_active_path is not false;   -- default search filter
```

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
- **Identity.** Upsert by `native_id` where the format has one (every
  whole-document format except Cline `api_conversation_history.json`, which
  is redundant with `ui_messages.json`). Where no id exists, key on
  (source, locator). Positional index is never a key. Claude rows keep the
  per-line `uuid`; lines sharing `message.id` are not merged.
- **Keep everything.** Every record the parser understands becomes a row,
  including branches, retries, and history a harness later drops. Two
  per-row facts decide what default search shows:
  - `superseded`: the row is absent from the latest generation of its
    source (rewrite, truncation, deletion, a deleted Devin session), or a
    newer version replaced it.
  - `on_active_path`: set only from a pointer the harness itself writes.
    Devin `sessions.main_chain_id` now, ChatGPT `current_node` later. Null
    for Claude, Codex, Pi, and every harness without such a pointer.
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
  row is marked superseded and a new row becomes live. ChatGPT keeps
  regenerations as sibling nodes; store every node and set
  `on_active_path` from `current_node`.
- **Deletions.** A message absent from a new generation is marked
  superseded with `superseded_in_generation`, never deleted. Claude's
  compaction cleanup, Amp and Cline edits, Codex migration rollback,
  Goose compaction, and Devin session deletion all drop history from the
  source; the index keeps it.
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
- **Pi / OMP:** subagent sessions in an artifact directory beside the
  parent file, with a `session_init` entry. Unverified on this machine.
- **Devin:** subagents in the same SQLite database. A `subagent_heads`
  table exists but has 0 rows here, so the linkage is unverified.

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
| kenn-io/agentsview (Go) | MIT | Long-line reader; Claude subagent and persisted tool-result resolution; Codex `call_id` pairing and checkpoint/anchor logic; Devin `main_chain_id` walk; secret rules (for section 6.6, later); second oracle | Reference and port; do not vendor |
| entireio/cli | MIT | Claude token dedupe by `message.id`; `apply_patch` path extraction; Codex `compacted` sanitising; fixtures | Port |
| codecast-sh/codecast (TS) | MIT | Codex subagent edge cases: `thread_spawn` vs `codex exec` review children vs forks | Port |
| specstoryai/getspecstory (Go, 15 providers) | Apache-2.0 | Format coverage for wave 2 and later | Reference only |
| neilberkman/ccrider (Go) | MIT | Format coverage | Reference only |
| google/codesearch | BSD-3 | `index/regexp.go` `RegexpQuery`, the regex-to-trigram planner for `find` (section 8) | Port |
| CASS; mcp_agent_mail | MIT with OpenAI/Anthropic rider | Nothing | Copy nothing, including fixtures. The rider forbids derivative works reaching those companies and defines analysis as use. CASS's connectors are thin wrappers over FAD anyway |
| trufflehog, claude-squad, grafana loki | AGPL | Nothing | Do not copy |

Why not vendor agentsview: a spike carried 27.6k lines into the tree for a
model that does not fit. It merges Claude lines by `message.id` and keeps
59% of assistant uuids, drops system lines and fork branches, keeps none of
18,688 Codex `payload.id` values, and reads only Devin's main chain (29% of
nodes). The relevant files change about 10 times a week upstream, and its
roadmap (issue #1352) converges on Flopwire's server, not on a library.

FAD's normalized model is `NormalizedConversation {agent_slug, external_id,
title, workspace, source_path, started_at, ended_at, metadata, messages}`
and `NormalizedMessage {idx, role, author, created_at, content, extra,
snippets, invocations}`. Tool calls flatten into content text plus an
`invocations` list; results become `role: tool` messages. `idx` is
positional after filtering and sorting, never durable. Native ids reach
`extra` inconsistently: the raw record for Claude text rows (Claude
tool-result rows carry a synthesized `extra` without the line uuid), and
for Codex, Gemini, Pi, Amp, Cline, Kiro, and Cursor; explicit keys for
OpenCode, Goose, Codebuff, and Devin; nothing for Aider and Hermes. FAD
records no byte offsets, no line numbers, no versions, no streaming, and
collapses supersession. The Go port keeps FAD's format knowledge and
discards its message model in favour of section 4.

Libraries: `modernc.org/sqlite` (no cgo; FTS5 `trigram` and
`contentless_delete` verified), `zeebo/blake3`,
`PlakarKorp/go-cdc-chunkers` (FastCDC), `klauspost/compress/zstd`, and the
existing `pgx` v5 and `minio-go` v7. Go 1.26.

### 5.2 Coverage strategy and parity oracles

There are no trivial connectors: the smallest is 300 lines and the whole
set is 8 to 10 engineer-weeks.

- **Wave 1:** Claude Code, Codex, Devin. These hold nearly all live data
  on the machine.
- **Wave 2:** Pi/OMP and Gemini. `pi` is not installed, OMP has no
  transcripts, and the newest Gemini session is from May, so there is
  little to test against.
- **Later:** Cursor, OpenCode, Copilot, Cline, Amp, Goose, Crush, and the
  rest as users appear.
- **No runtime sidecar.** A format without a native parser is not
  ingested. Native parsers are what give offsets, stable ids, tailing, and
  versioning; a sidecar's positional rows would need re-ingesting anyway.

Parity oracles run only in tests:

- **fad-dump.** agentboard's `tools/fad-dump` bumped to FAD `=0.3.1` and
  extended to emit `extra` and `invocations`. Build `testdata/fad/home/` in
  each agent's native layout (SQLite as `.sql` seeds), regenerate expected
  JSON under `env -i HOME=...`, point the Go parsers at the same home, and
  assert discovered sources, session and workspace, and the (role,
  content, created_at) sequence match.
- **agentsview.** Its binary and its MIT parser testdata as a second
  differential oracle: per-session counts, roles, and ids.
- **Real corpus.** Both oracles also run against a sampled slice of the
  real corpus. Every divergence is documented per agent.

Oracle limits, which is why id-level parity uses small fixtures:

- FAD compacts `extra` and drops native ids for Claude, Codex, and Gemini
  files of 32MiB or more.
- FAD's Codex connector errors on files over 100MiB (it drops them whole;
  4 local rollouts) and on a partial final line, and skips
  `archived_sessions/`.
- FAD Claude tool-result rows carry a synthesized `extra` without the line
  uuid.
- FAD never reads Codex `session_meta.id` or `thread_spawn`.
- FAD's Gemini connector ignores `toolCalls` and `thoughts`.
- agentsview's model differs (see 5.1), so it checks counts and roles, not
  row identity.

Effort per connector: 3 to 6 hours for the small tier (clawdbot, vibe,
qwen, aider, crush, codebuff, grok_bot; about a day for `pi_wire` with pi
and omp), 1 to 2 days for medium (gemini, cline, amp, devin, kiro,
factory, copilot_cli, hermes, muse, openhands), 2 to 4 days for Claude and
Codex, 3 to 5 days each for Cursor, OpenCode, Copilot VS Code, Shelley,
Goose.

### 5.3 Storage kinds and the append-only assumption

The byte-offset watermark is the fast path, not an invariant. Both Claude
Code and Codex rewrite transcript files in specific cases.

| Connector | Storage | Append-only | Rewrite or truncate cases | Strategy | Present here |
|---|---|---|---|---|---|
| Claude Code | JSONL per session, plus `subagents/agent-*.jsonl` and `subagents/workflows/<runId>/agent-*.jsonl` with `.meta.json` sidecars; `tool-results/` companion files | mostly | Failed streaming message removed in place. Compaction cleanup rewrites the file, moves title/mode records to the top, drops pre-boundary history. About 2% of transcripts got a new inode mid-session. `/compact` itself appends a `compact_boundary` line. Cleanup deletes files over `cleanupPeriodDays` (30 by default) at startup, which has orphaned 1,015 of 2,219 `tool-results/` files here. Resume and `--continue` append. | offset + rewrite check; companion files whole | 6.4GB JSONL, 613MB `tool-results/` |
| Codex CLI | JSONL rollout; top-level `ordinal` per line, `payload.id` per `response_item` | mostly | Live sessions only append (`compacted`, `turn_aborted`). The 0.158 binary has a legacy-rollout migration with rollback, a compression worker, and a truncation path; 124 finished rollouts were replaced whole. Archiving moves files to `archived_sessions/`, which Flopwire follows (FAD skips it). `thread_history_1.sqlite` (2.8GB parsed projection with `item_id`, `rollout_ordinal`, `next_rollout_byte_offset`) and `state_5.sqlite` (`threads`, `thread_spawn_edges`) are cross-checks, not sources. | offset + rewrite check; follow renames | 11GB |
| Pi, OMP | JSONL | yes | rewritten in place on version migration | offset | 3MB |
| Devin | SQLite `message_nodes`, autoincrement `row_id`, branching tree; `created_at` in epoch seconds | no | Revert changes `sessions.main_chain_id` with no new rows. Whole sessions are deleted (`sqlite_sequence` at 338k against about 40k live rows; 64 sessions gone). `tool_call_state` rows update in place. | rowid watermark + `main_chain_id` check + session-set diff (vanished sessions become superseded) + `tool_call_state` re-read | 652MB db |
| agentboard Devin mirror | JSONL | no | rewritten whole on revert, compaction, format bump | offset + rewrite check | 79MB |
| Grok CLI | session dir; `updates.jsonl` | updates yes; `chat_history.jsonl` unverified | rewind may rewrite history | offset + rewrite check | 632KB |
| Gemini CLI, Qwen | one JSON document per session, `id` + `timestamp` per message | no | whole document every turn; `toolCalls` filled into the last message later; JSONL variant `$set` replaces the array | hash-gated whole-document re-parse, upsert by `id` | 6MB |
| Antigravity | `transcript.jsonl` plus `.pb` | mostly | some written in one shot | offset + rewrite check; `.pb` whole | 35MB |
| Cursor | SQLite key-value `state.vscdb`; `bubbleId` per message | no | values overwritten in place; timing and tool status fill in later | re-read a composer when its key changes, upsert by `bubbleId` | 146MB, dormant |
| VS Code Copilot | JSON snapshot plus JSONL change log; `requestId` | partly | log compacted into snapshot; can truncate and delete | whole file, upsert by `requestId` | 92KB |
| Copilot CLI | `events.jsonl` | yes | none known | offset | absent |
| ChatGPT desktop | encrypted JSON per conversation; node uuid tree | no | regenerations become sibling branches, old kept | whole blob, store every node, flag active path | 25MB |
| Amp | `threads/T-*.json`; `messageId` | no | rewritten **without bumping mtime**; edit drops the rest of the thread | hash-gated whole file, upsert by `messageId`, tombstone missing | absent |
| Cline | `ui_messages.json` (`ts` as id), `api_conversation_history.json` (no id) | no | `partial:true` rewritten in place; edit or checkpoint restore cuts the array | whole file, upsert by `ts` with same-millisecond guard; skip api history | absent |
| Codebuff | `chat-messages.json`; `id`, `parentId` | no | blocks grow while streaming | whole file, upsert by `id` | absent |
| OpenCode, Crush | SQLite; message and part ids | no | part rows overwritten while streaming; revert deletes | session `time_updated` re-read, upsert by id | absent |
| Goose | SQLite (new) or JSONL (old) | rows mostly | compaction deletes and reinserts, flips visibility | rowid watermark; tombstone | absent |
| Hermes, Shelley, OpenClaw 2 | SQLite | rows mostly append | Shelley carries rows on compaction | rowid or seq watermark | absent |
| Kiro, Factory, Vibe, Muse, clawdbot, Prime | JSONL | yes | Kiro has a JSON snapshot sidecar | offset | absent |
| Kimi | `wire.jsonl` plus `context.jsonl` | wire yes | context rewritten | offset on wire | absent |
| OpenHands | one immutable file per event | yes | none | directory listing | absent |
| Aider | markdown history | yes | user may delete | offset | absent |
| Grok Bot | lossy rolling replica | no | oldest entries dropped | whole file | absent |

Cline behaviour is from reading its upstream code, not a local sample.

### 5.4 Wave 1 parser rules

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
- **Gate tuple:** file identity (device + inode; volume + file index on
  Windows), size, change time. Change time is the inode ctime on POSIX,
  which no user-space writer can preserve (verified: a same-size rewrite
  with mtime restored still moves ctime), and NTFS `ChangeTime` via
  `GetFileInformationByHandleEx` on Windows. Never gate on mtime alone:
  Amp rewrites threads without touching it, and any writer can restore it.
- **Racy rule** (from git): a file whose change time equals the previous
  sweep's timestamp is re-hashed regardless.
- **Identity for rewrite-prone documents:** path + inode + a session id
  read from the content. Never a fingerprint of the file head; Gemini, Amp
  and Cline rewrite the head. (Filebeat and Vector fingerprint the head and
  would break here.)
- **FS events** (FSEvents, inotify, ReadDirectoryChangesW / USN) are an
  optional latency improvement that wakes the agent for the touched path.
  The sweep is the floor and the source of truth.
- **Hooks as flush signals:** Claude `Stop` and `PostToolUse`, Codex
  equivalents, trigger an immediate flush of the affected source.
- **SQLite sources** are polled by rowid or session updated-stamp. Devin
  also checks `main_chain_id`, diffs the session set to detect deleted
  sessions, and re-reads `tool_call_state` rows, which update in place.

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
  hashes), not fixed 4MB offsets. Starting parameters: 256KB to 1MB average, 128KB minimum,
  4MB maximum; measure the dedupe ratio on a real rewritten Gemini or Amp
  document before fixing them. Content-addressed by hash. After a rewrite
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
- **Flush cadence, append-only files:** trailing-edge debounce of 250 to
  300ms with a 1 to 2s maximum wait, on line completion, plus immediate
  flush on hook. No quiet window. Active sessions reach the server in
  seconds.
- **Flush cadence, rewritten documents:** debounce 2 to 3s with a 10s
  maximum wait. A mid-stream flush uploads a version that existed; the
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

Timing: redaction ships before the first team user, not in wave 1. During
the two-laptop phase, the only user is the owner of every byte, and the S3
archive of that phase is unredacted; section 12 records what happens to
it before a team joins.

Where it runs: on the device, on the upload path only, before chunking,
and irreversibly. The local index is never redacted: it holds the owner's
own data, and a masked secret there would only stop the owner finding
where they pasted it. The agent masks known secret shapes in the bytes it
uploads: API keys and tokens by known prefixes and entropy (OpenAI,
Anthropic, GitHub, AWS, GCP, Slack, Stripe, JWTs), private key blocks,
connection strings with embedded credentials, `Authorization` and cookie
headers, and `.env`-style `KEY=value` assignments whose key matches
`SECRET|TOKEN|PASSWORD|KEY`. Each match is replaced by a fixed-width marker
carrying the rule name and a short hash of the original, so two hits of the
same secret still correlate and a false positive is diagnosable. From then
on the server never receives the original bytes; the S3 archive holds the
redacted version, and that is documented as the trust boundary. The
existing path denylist stays as a coarse exclusion on top. Rules are
versioned; a rule change does not rewrite history. agentsview's secret
rules are a reference for the rule set. False negatives are expected and
the product copy says so plainly.

Once redaction is on, server-side chunks and offsets describe the redacted
stream, not the source file: `raw()` on the server returns redacted bytes,
while the local CLI reads the source file.

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
provisional tails, conversations, messages. Text is TOAST-compressed by default only when a value exceeds
about 2KB; store `text` as zstd-compressed `bytea` for uniform
compression and decompress in the API. The GIN index is uncompressed
either way.

Backup: S3 plus the identity, sources, generations, chunks, manifest, and
provisional tail tables. Everything else is derived. Rebuild = replay
manifests (and tails) through the parser.

### 7.3 Retrieval API

- `search(q, mode=ranked|exact, filters{agent, repo, device, user, kind,
  time range, include_subagents, include_superseded, include_branches,
  include_self}, limit)` returns hits
  with conversation, message, kind, timestamp, snippet, and provenance
  (path, generation, line, offset).
- `context(message_id, before, after)` returns neighbouring rows by
  ordinal from Postgres, decompressed. No object store involved.
- `raw(source_id, generation, byte_offset, byte_len)` maps the range to
  chunks by manifest arithmetic, fetches one or two objects (or reads the
  provisional tail), and returns the bytes. This serves uncapped tool
  output and exact provenance.
- `conversation(id)` returns the message list with links to children.

Local CLI serves the same calls from SQLite and reads raw bytes from the
source file, falling back to the server when the file is gone.

## 8. Search for agents

The only clients are agents (Claude Code, Codex, Devin, and whatever else
calls the MCP server), so the query surface is designed around how agents
already search: exact strings and regexes, file paths, identifiers, error
text, then reading a few lines of context. That is `grep` with provenance,
plus a ranked mode for "where did we deal with X".

No embeddings. Three modes, one MCP tool each and matching CLI verbs:

- **`find(pattern, regex=false, case_sensitive=false, filters)`** exact
  substring or regex. Backed by a trigram index in both stores (SQLite
  FTS5 `trigram` tokenizer; Postgres `pg_trgm` GIN), which accelerates
  substring and case-insensitive search. For regexes, a port of
  google/codesearch's `RegexpQuery` (`index/regexp.go`) plans the pattern
  into an AND/OR query over trigrams; that query selects candidate rows
  from the trigram index, and Go's `regexp` verifies each candidate. A
  pattern that yields no trigrams (for example `.*`) falls back to a scan
  bounded by the other filters. Costs roughly 3x the token index,
  about 2GB more locally for this corpus; accepted, because this is the
  mode agents use most. Output is grep-shaped: one line per hit with
  `path:line`, agent, session, timestamp, kind, and the matching line,
  plus `--context N` to include neighbouring messages.
- **`search(query, filters)`** ranked BM25 over prose and capped tool text
  for the "I don't know the exact string" case. Returns hits with a
  snippet and the same provenance.
- **`context(message_id, before, after)`** and **`raw(...)`** as in 7.3,
  so an agent can widen a hit into the conversation or fetch an uncapped
  tool output.

Filters everywhere: agent, repo (resolved git root), device, user, kind,
time range, include subagents, include superseded, include branches
(section 4.2 defines the default), include self.

**Self-session exclusion.** An agent's own `find` call, and the output it
gets back, are indexed within about a second, so a naive repeat search
finds the query itself. Search, find, and context therefore exclude the
calling session's rows by default; `include_self` turns the exclusion
off. How the MCP server identifies its calling session in each harness
(for example, the parent process's entry in `~/.claude/sessions/`) is
settled in PR A3.

Output is JSON by
default for MCP and a compact text form for the CLI, both stable enough
to be parsed by an agent. Ripgrep over raw files remains available locally
for anything the extraction cap removed. Embeddings can attach to the same
rows later (sqlite-vec locally, pgvector on the server) without changing
the model.

## 9. Message bus (design now, build last)

> Superseded on 2026-10-01 by `notes/message-bus/plan.md`: direct messages
> only, hook delivery, no wake. This section is kept for the harness survey
> and the reasoning behind the carried decisions.

### 9.1 What the harnesses offer today

> Delivery mechanisms were re-verified on 2026-09-28 against live sessions.
> `notes/message-bus/README.md` holds the protocol detail and supersedes
> this section where they differ. Its corrections (MCP channels need launch
> flags; the Codex queue works without the daemon; Claude peer origin is
> unauthenticated) are folded into 9.1 to 9.3 below.

| Harness | In-session subagents | Cross-session messaging | Transport | Addressing | Receive-side hook points | Semantics |
|---|---|---|---|---|---|---|
| Claude Code 2.1.x | Agent tool, named subagents, agent teams; `SendMessage` to teammates; JSON mailboxes under `~/.claude/teams/<team>/inboxes/` | `ListAgents` + `SendMessage` to other local sessions, cloud sessions, and Remote Control sessions; cloud targets one-way; `notify_when_idle` | Local: per-session Unix socket `/tmp/cc-socks/<pid>.sock`, newline JSON, optional auth line. Remote: Anthropic relay, no public API | Session name, `uds:<socket>`, `bridge:<id>`; registry `~/.claude/sessions/<pid>.json` | Post `{"type":"user","message":{...}}` to the socket; MCP channels (`notifications/claude/channel`); hooks `UserPromptSubmit`/`SessionStart` `additionalContext`, `Stop`; `claude -p --resume` | Plain text. Read between tool calls mid-turn, starts a turn when idle. `crossSessionInbound` accept/hold/refuse; explicit `hold` never expires; bypass-mode approval dialog expires (5 min default); 100 held max, inbox 50. No threads or acks. Sender sees held/denied/expired/dropped |
| Codex CLI 0.15x | `spawn_agent`, `send_message` (queue only), `followup_task` (starts a turn), `wait_agent`, `list_agents`; in-memory message board | `codex queue --thread <uuid or name>` via app-server `thread/queue/add`, `--remote ws(s)://` | Queue persisted in `~/.codex/queue_1.sqlite`, polled by every Codex process; app-server daemon (JSON-RPC over WebSocket on `~/.codex/app-server-control/app-server-control.sock`) for steer and inject | Thread UUID or exact name; subagents by agent path, not externally addressable | `codex queue`; app-server `turn/start`, `turn/steer` (mid-turn, daemon-hosted threads only), `thread/inject_items`; `PostToolUse` `additionalContext` (mid-turn, any session); hooks; `codex exec resume` | Rollouts store `agent_message{author, recipient}` with encrypted payloads. Queued messages wait for the turn to end or wake an idle session. No ack or reply |
| Devin CLI | foreground/background subagents, `read_subagent` | none | ACP over stdio; `--cloud` relays over WebSocket | session id in `sessions.db` | `hooks.json` (Claude format); `-r <id> -p`; an ACP client can send prompts | n/a |
| Cursor | subagents with hooks | Cloud Agents API follow-ups | HTTPS to Cursor cloud | agent/chat id | `stop` hook `followup_message` auto-submits; `beforeSubmitPrompt` | run-oriented |
| Gemini CLI | local subagents; remote subagents over A2A | A2A to remote agents as tools | HTTP (A2A) | agent card URL | `--acp`, `--resume` | A2A tasks have ids and states |

Only Claude Code can send across sessions natively, and only to Claude.
Only Claude (socket) and Codex (queue) can receive into a running session.
No harness has threads, replies, acknowledgements, or expiry across
harnesses. Codex inter-agent bodies are encrypted in the rollouts, so the
archive cannot read them.

### 9.2 Design

- **Identity.** Map (device, harness, session id) to a Flopwire device and
  user. Claude: session id and name from `~/.claude/sessions/<pid>.json`.
  Codex: thread UUID, with `session_meta.source.subagent` giving the parent.
  Devin: session id in `sessions.db`. Agentboard does not provide this for
  Claude or Codex: it matches tmux scrollback to logs by content
  (`logMatcher.ts`) and never reads `~/.claude/sessions/<pid>.json`. Only
  its Devin path matches a lock file to a pid (`devinLockMatch.ts`), which
  is portable. Flopwire builds the Claude and Codex mapping itself.
- **Model.** `threads (id, created_by, subject)`, `bus_messages (id,
  thread_id, from_session, to_session, body, reply_to, created_at,
  expires_at, delivered_at, read_at)`. Delivery state is owned by Flopwire,
  not by any harness. `read_at` is set when the message text appears in
  the recipient's transcript, which the archive already captures; this is
  the receipt no harness provides (except for Codex inter-agent payloads,
  which are encrypted).
- **Send side.** One MCP server, `flopwire-bus`, with `send(to, text,
  reply_to?)`, `list_peers()`, `inbox()`. MCP works in every harness.
- **Delivery side, per harness, by the device agent:**
  - Claude: post the documented JSON line to `messagingSocketPath` (read
    from the registry); wakes an idle session, arrives between tool calls
    in a busy one. Prompting sessions need no `crossSessionInbound`
    setting. MCP channels are not used: they need launch flags.
  - Codex: `codex queue --thread <UUID>` (`thread/queue/add`); durable in
    SQLite and polled by every Codex process, so the daemon is not needed.
    `turn/steer` and `thread/inject_items` need a daemon-hosted thread; a
    `PostToolUse` hook reaches any session mid-turn. Queue only to live
    sessions: Codex runs a queued message to a closed thread on resume.
  - Devin, Cursor, Gemini: a `UserPromptSubmit` or `SessionStart` hook
    that drains `inbox()` into `additionalContext`, plus a `Stop` hook
    (or Cursor's `followup_message`) to keep the session going; resume
    headless only when nothing is running.
- **Framing.** Every delivered message is wrapped as untrusted peer input,
  as Claude Code already does for its own peers.
- **Impossible today:** delivery into Claude cloud or Remote Control
  sessions from outside Anthropic; structured payloads into Claude;
  `turn/steer` into a Codex session not hosted on the daemon; pushing into a
  live Devin, Cursor or Gemini session with no user turn and no hook;
  harness-native acks; addressing Codex subagents.

### 9.3 Decisions

- **Addressing.** A message is addressed to a session: `(user, device,
  harness, session id)`. The id comes from the transcript itself (Claude
  `sessionId`, Codex `session_meta.id`, Devin session id, Pi
  `session_init`, Gemini session file, Cursor composer id) and is known
  from the first uploaded line. Flopwire shows a friendly alias derived from
  the id plus title and repo, as agentboard does; harness-native names
  (`--name`, `/rename`, Codex thread names) are optional labels, never the
  key. The user is the enrolled owner of the device, per the existing rule
  that the uploader's credential determines authorship. `list_peers()`
  returns live sessions across the team with owner, harness, repo, cwd, a
  task summary from the transcript, and idle or busy state, read from the
  harness registries on each device (`~/.claude/sessions/<pid>.json`,
  Codex app-server).
- **Permission.** Messages between a user's own sessions deliver
  automatically. A message from another user is held until the
  recipient's human accepts that sender once; acceptance is per sender and
  revocable. Held messages are surfaced to the human in the CLI and web
  console. Nothing enters an agent's context that its human did not allow.
  This mirrors Claude Code's own accept, hold, refuse, but the device agent
  enforces it: Claude Code does not authenticate same-user peers on macOS
  or Linux, so its own policy is not a boundary.
- **Delivery.** Recipient busy mid-turn: queue and deliver between tool
  calls (Claude socket, Codex queue do this natively). Idle: deliver and
  wake it. No live session: hold in the recipient user's inbox, deliver to
  their next session on the same repo or, failing that, their next session
  anywhere, and expire after a configurable time (24h default). The sender
  is told which outcome happened. Flopwire never resumes a session headless
  to force delivery; that spends a human's tokens without them present.
- **Payload.** Body text, optional `reply_to`, and references to
  conversations or messages in the archive. On delivery, references render
  as short excerpts with provenance, so Claude's text-only socket still
  works; the recipient widens them through the search tools. Handoffs stay
  small and inspectable.
- **Receipts.** `delivered_at` when the device agent hands the message to
  the harness; `read_at` when the message text appears in the recipient's
  transcript, which the archive captures. Codex inter-agent bodies are
  encrypted in rollouts, so for Codex-to-Codex only `delivered_at` is
  available.
- **Re-verify before building.** Claude Code's socket contract and Codex's
  queue protocol change monthly. The builder re-reads the installed binary
  strings and docs the week bus work starts.

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
- CASS on Gary's machine is left alone. It is not running now, but its
  launchd plist has `RunAtLoad` and `KeepAlive`, so it restarts at the next
  login. Deleting its stale index generations (1.8GB) and backup directory
  (6.4GB), or unloading the plist, is Gary's call and not part of this
  work.

## 11. Sequencing

### 11.1 Milestones and PRs

Main is the trunk (`build/v1` is promoted to it by #16). Every PR below
targets main, is small enough to review in one sitting, and is built by an
Opus subagent in its own worktree; the orchestrating session fans them out
and Gary reviews and merges.

**M1: parser library.** A dependency of everything else.

| PR | Scope | Depends on |
|---|---|---|
| PR1 core | `internal/transcript`: the section 4 model, including `superseded` and `on_active_path`. Line reader that streams lines of any length and reports byte offset and length (section 6.2). JSONL cursor with the rewrite checks of 6.2. Fixture and oracle harness: `tools/fad-dump` pinned to FAD 0.3.1 and extended to emit `extra` and `invocations`, the agentsview binary, `testdata/` layout, and `third_party/<name>/` license files (section 5.2) | none |
| PR2 Claude | Claude parser per 5.4: recursive subagent discovery and `meta.json` links, `tool-results/` companions, ignored line types, oracle tests | PR1 |
| PR3 Codex | Codex parser on `response_item`, plus the `codex-events@1` enrichment sub-parser; `thread_spawn` links; `archived_sessions/`; oracle tests | PR1 |
| PR4 Devin | Devin SQLite parser: every node, upsert by `chat_message.message_id`, `on_active_path` from `main_chain_id`, deleted sessions superseded, `tool_call_state` updates | PR1 |

PR2 to PR4 run in parallel once PR1 merges.

After M1, two tracks run in parallel. Both depend only on M1.

**Track A: local index on one laptop (personal CASS replacement).**

| PR | Scope | Depends on |
|---|---|---|
| A1 | SQLite schema (`modernc.org/sqlite`): section 4 tables, FTS5 token table and FTS5 `trigram` table, default-filter index | M1 |
| A2 | Indexer: stat-gate sweep, FS events, tailing, rewrite detection into new generations, superseded marking, Devin polling | A1 |
| A3 | CLI `find`, `search`, `context`, `raw` and the MCP server; `RegexpQuery` planner port; default filters and self-session exclusion (section 8) | A2 |
| A4 | Acceptance benchmarks (11.2) and the harvested query set, runnable as one command | A3 |

**Track B: sync between two laptops (the team product's path).**

| PR | Scope | Depends on |
|---|---|---|
| B1 | One fresh migrations schema (section 4, Postgres), and the hand-ported salvage from the old PR stack (11.3): identity, audit, backup and deletion, security, UI, release scripts, migration ledger | M1 |
| B2 | Device side: FastCDC chunker, BLAKE3 hashes, manifests, provisional tails, upload protocol, watermark queue and spool; server down treated as normal | B1 |
| B3 | Server ingest: S3 chunks, Postgres manifests and `provisional_tails`, delta parse into message rows, `tsvector` and `pg_trgm` indexes, retrieval API (7.3) | B1, B2 |
| B4 | Two-laptop end-to-end: server on one laptop (Compose plus MinIO), the other syncing over the tailnet; offline and reconnect; rewrite and deletion cases | B3, A2 |

**Then:** redaction (section 6.6) before the first team user, including a
decision on the unredacted two-laptop archive (section 12); wave 2 parsers
(Pi/OMP, Gemini) when there is data to test against.

**Last: the message bus** (section 9), after re-verifying the harness
protocols.

### 11.2 Acceptance bar for the local track

Track A is done when all four hold:

- The full corpus (Claude, Codex, Devin; about 18GB) indexes in under 5
  minutes with peak RSS under 300MB. The idle agent stays under 50MB. A
  sweep with no changes costs under 1s of CPU.
- A line written by a live session is findable locally within about 2s.
- Oracle parity tests pass against fad-dump 0.3.1 and agentsview, on the
  fixtures and on a sampled slice of the real corpus, with every divergence
  documented.
- A saved set of about 30 real queries, harvested from past `cass search`
  invocations found in the transcripts, returns the expected hits in under
  200ms each.

### 11.3 Existing branch stack

The open PR stack #2 to #8 (about 107 commits) hardened a launch candidate
built around CASS. It is not rebased or merged. After M1, B1 hand-ports
what survives onto the fresh schema:

| From | Keep |
|---|---|
| #3 backup and deletion | `8a3fc34`, `1f6d8fc`, `4120fba`, `9c39cfd` |
| #5 UI | everything except `209193f` |
| #6 security | `031cf6f`, `4d2cbae`, `4d2e741`, `d4eb6a1`, `4c3ee87`, `3d9ad07`; remap `raw_object_ledger` to chunks |
| #8 identity and runtime | `62d5a64`, `4bbf5fe`, `cb16d94`, `004879b` (pathpolicy), `1a64874`; the migration-ledger mechanism (`7bcbe33`, `91e1724`, `b9e6387`) |
| #4 release | release scripts only: `validate-release-tag*`, `source-release.yml`, `aa35add` |
| #7 | `internal/provenance/git.go` and its sanitized fixtures only |
| #2 | the notes, by hand from `origin/launch/readiness` |

Dropped: all CASS ingestion, staging, prune, fence, and reconstruction
code, and every existing migration. Migrations reset to one fresh schema.

## 12. Open questions and measurements to do

- The two-laptop-phase S3 archive is unredacted. Before a team joins,
  either re-chunk it through the redactor or scope it to its owner so no
  other user can read it.
- Chunker parameters: measure dedupe ratio and per-flush upload size on a
  rewritten Gemini or Amp document and on a busy Codex rollout.
- Tool-text cap policy per kind, and whether to keep reasoning summaries
  (5% of bytes, rarely searched).
- Redaction rule set and its false-positive rate on real tool output;
  measure on the corpus before shipping to a team.
- Pi `session_init` and artifact subagent directories, and Devin subagent
  linkage (`subagent_heads` has 0 rows here): verify on real data before
  their parsers claim subagent links.
- How the MCP server identifies its calling session in each harness, for
  self-session exclusion (decided in A3).
- Cline edit and checkpoint-restore behaviour: confirm on a real sample.
- Windows: implement the `ChangeTime` and USN paths and test the racy rule
  on ReFS and NTFS.

## 13. Files

- `ftsidx.py`: the FTS5 prototype. `python3 ftsidx.py DB index --roots`,
  `python3 ftsidx.py DB query 'terms' [--agent codex] [--cwd PREFIX]`,
  `python3 ftsidx.py DB optimize`.
- `shape.py`: corpus byte breakdown by record type.

Research was carried out with Opus subagents on 2026-09-24 through
2026-09-28, starting from the agentboard stall investigation. The CASS
build from `main` and its scheduled-run logs live under `~/.local/cass-main/`
on Gary's machine.
