# Flopwire pre-landing punch list (2026-09-29)

**Landed on `main` 2026-09-30 in #111.** Follow-up work is tracked in GitHub issues under the "first team" milestone; this file is the record of what landed and the decisions behind it. The agent process is in `agent-workflow.md`.

Source: five Opus reviews of integ/full plus the search-UX research, and Gary's decisions. Work lands on `integ/full`, then gets re-cut into 4 PRs: parsers, local, server+sync, search UX. The 25 stacked PRs close as superseded.

Ground rule: the product is pre-release. Add no backward-compatibility paths, legacy handling or data migrations for our own formats. A schema change can reset migrations or force a local reindex. Harness formats on disk still need full coverage.

## A. Decisions to implement

| # | Decision |
|---|---|
| D1 | A transcript file the harness deletes keeps its rows live and searchable, with no flag. A deleted Devin session is still superseded. |
| D2 | The ranked-search cap stays (newest 20k matches for very common terms). When it triggers, output says `ranked newest 20k of N matches — add terms or filters`. |
| D3 | `find`/`grep` indexes everything. Remove the head/tail/error-line trigram cap locally and on the server. Re-measure acceptance. |
| D4 | Self-exclusion uses exact matches only: `FLOPWIRE_SESSION_ID`, the parent's Claude session file, `CLAUDE_CODE_SESSION_ID`, or a parent Codex process holding exactly one rollout open. Drop the cwd/recency guess. Never exclude when a human runs it at a TTY. Output always names the excluded session. |
| D5 | The search UX redesign lands before landing (section C). |
| D6 | New kinds: `injected` (CLAUDE.md/AGENTS.md/system-reminder text injected into user turns), hidden by default; and `agent_message` (Codex messages between agents). Hits with identical text collapse into one with `+N copies`. |
| D7 | "Newest" means message timestamp everywhere: find/grep order, the rank cap, the early stop. |
| D8 | Query budget: 10s by default, adjustable per call up to 60s, backed by a byte-verification budget. Stop early at `limit`. On timeout, return partial hits plus one line (`timed out after 10s: checked X of Y candidates, newest first; narrow with …`), flagged `truncated/reason`, never an error. MCP handles calls concurrently and honours `notifications/cancelled`. The server sets a Postgres `statement_timeout` to match. |
| D9 | Deletes are manual only. The owner or an admin can delete (members get a route to delete their own conversations). A delete forgets the session for good: later lines are dropped, it cascades to subagent conversations, the tombstone applies per user across devices, and backups leave out deleted chunks. |
| D10 | Raw bytes are org-wide. Every hit and every raw/read response carries attribution: user, device, harness, session and repo. Scoping `/v1/sync/has` per user is hygiene (a proof-of-possession check), low priority. |
| D11 | Codex forks with no history marker skip items whose payload id already appears in the `forked_from` parent. |
| D12 | One indexer per index: an exclusive flock on `index.db.lock`. A second daemon fails fast with `agent already running (pid N)`. `agent run --once` asks the running daemon for a pass and waits. |
| D13 | Transport: the server always speaks TLS. By default it generates a self-signed cert whose SHA-256 fingerprint rides in the invite and gets pinned at `enroll`. With a public domain configured it uses ACME. `--tls=proxy` suits operators with their own reverse proxy. Plain HTTP works only on loopback. Update SECURITY.md, docs/two-laptop.md and compose. Tailscale is not assumed. |
| D14 | The server keeps one text column: plain, uncapped, lz4 TOAST with a lower `toast_tuple_target`. It feeds both tsvector and trigram. Drop the zstd duplicate. |
| D15 | Sync recovery: retries per source (a bad source delays only itself), a 30s backoff cap, and a hook flush resets backoff. No reachability probe and no "mark lost"; unreadable sources show in `flopwire agent status`. |
| D16 | Stable ids: Claude always uses `uuid#<block index>`, and the ordinal comes from file position (`offset<<12 | block`). A parser-version change triggers a background re-parse from the original bytes. Rows are replaced only when that succeeds, and sources whose files are gone keep their rows. |
| D17 | Terminal output replaces control characters (ESC becomes ␛; `\t` and `\n` are kept). JSON stays exact. There is no untrusted-content framing for agents. |
| D18 | Path rules have three modes: `allow` (default), `local` (index only, never upload) and `deny` (neither). They match the session's cwd/repo with globs. The most restrictive rule wins. Admin rules are floors that a user can tighten but not loosen. A new deny purges existing local rows and prompts about the server copy. Enforce in the agent before indexing and upload. |
| D19 | Uniform empty tool outputs: always emit the tool_result row, with `is_error` from the exit code (Codex currently drops about 7.9k and loses 1,340 failures). |
| D20 | A Codex rename into `archived_sessions/` links as `Previous` (by FileID/session id) instead of re-uploading as a new source. |
| D21 | Devin export tails seal after 5 minutes with no change, the same as files. |

## B. Bugs (verified by review)

### Parsers
- P1 Devin: when a message's chosen copy changes, the old copy's extra rows (`A#call:c1`) stay live, and the text row is duplicated across parts. Also, off-chain copies' unique tool calls are never indexed. Fix: a part fixed per native id; emit an off-chain copy's unique rows with `on_active_path=false`; supersede on a switch.
- P2 The change-detection watermark hashes the live file after parsing, so a rewrite during the parse is missed. Hash the bytes the line reader consumed.
- P3 A cursor-state version bump or corrupt state leaves a source permanently stuck. Treat unreadable state as a signal for a full re-parse and a new generation.
- P4 Codex cursor state holds up to 150KB per rollout (closed calls keep full rows). Store only the turn, the command key and the minimal row fields.
- P5 Oversized lines bypass the shared memory budget (Claude reads lines up to 256MiB whole; the Codex json.Decoder buffers the whole value). Route them through `LineBudget`.
- P6 Some Codex tool_result text keeps literal `\n`/`\"` escapes, a decoding bug that search UX turned up.

### Local index and agent
- L1 Two writers lose each other's FTS entries. Fixed by D12's flock.
- L2 A failed shard transaction is skipped forever and `sh.err` sticks. Retry the failed batch with backoff and never advance `applied` past it.
- L3 The CLI/MCP open writes shard config and replays the queue concurrently with the agent, which can leave duplicate or stale FTS entries. Add a read-only Open mode for readers, and make `apply` idempotent per entry (sequence number per entry, or delete the rowid before insert).
- L4 A lost or recreated shard file is never rebuilt, and power loss can reuse sequence numbers. At Open, check `applied` against the queue range and rebuild the shard from `messages` if it is inconsistent.
- L5 A crash between saving the watermark and marking rows superseded leaves stale rows live. Supersede inside the final batch's transaction, before the watermark.
- L6 Cancelling a request's context rolls back the whole deferred transaction. Run statements with `context.Background()` and check ctx only before a request starts.
- L7 Discovery queues files in path order, so row id order doesn't track time. Covered by D7; also sort discovery by mtime.
- L8 The Linux watcher only compiles and is never run: `IN_MODIFY` causes a full project rediscovery, a Poll error busy-loops, and watches on removed directories leak. Fix these and add a Linux CI job for internal/agent.

### Sync
- S1 Salvage discards intermediate versions whose chunks the server already holds (a Devin/Gemini/Amp rewrite during an outage loses its generations). Keep entries the server already has.
- S2 One request can crash the server with OOM (unbounded Body/Tail Size is allocated before the read). Bound sizes in Validate, check `Offset+Size` for overflow, and add conformance cases.
- S3 One failing source blocks all uploads. Fixed by D15's per-source retry.
- S4 No HTTP timeouts: the device sync client, the CLI `--server` path and the raw fallback all use `http.DefaultClient`. Use a ResponseHeaderTimeout plus a per-request deadline scaled to payload size.
- S5 Orphaned spool chunks after a gap cut are never released. Add `AND m.ordinal < g.entries`.
- S6 The spool leaks after a crash between spooling and `saveCapture`. At startup, clean up `*.tmp` and chunk files nothing references.
- S7 The server trusts `Entry.Size`. Require it to equal the stored chunk size.
- S8 `describe` stats the parent at upload time, so a companion can link to a newer parent. Capture the parent identity at capture time.
- S9 Flushes of up to 32MB must finish within the 30s ReadTimeout, which slow links can't do. Lower `MaxRequestBytes` or add zstd on the wire.

### Server
- V1 The DB pool can be exhausted (default MaxConns, no statement timeout, a no-trigram regex scans everything). Set MaxConns explicitly, add the D8 statement_timeout, and have the server use the same regexq planner: verify in Go and refuse unbounded scans.
- V2 A failed deletion job blocks uploads of its chunks (503) and fails every backup. Leave deletion-owned states out of the backup inventory, and let an uploader reclaim a `deletion_pending` chunk.
- V3 Parse-time tombstoning skips the purge lock (so it races backup) and writes no audit event. Take the lock and audit it.
- V4 Parse failures get no effective backoff because each flush resets attempts, and nothing quarantines them. Keep attempts across flushes, quarantine after N, and surface it in admin status.
- V5 The quota check sums every chunk under a global lock. Keep per-user byte counters instead.
- V6 The opposite-order chunk upload deadlock (40P01 returns 503). Use `pg_try_advisory_lock`; on contention, skip the put and report Missing.
- V7 Server regex semantics differ from local (`^`/`$` multiline, `\b`). Fixed by V1's Go-side verification.
- V8 `raw()` fallback finds the server copy by text search. Add `GET /v1/raw?path=&file_id=&offset=&length=` resolved by (device from credential, path, file_id).

### CLI, MCP and security
- C1 The regex DoS (≈47 minutes) and MCP handling one request at a time. Fixed by D8.
- C2 The path denylist isn't enforced. Fixed by D18.
- C3 A line over 4MB kills the MCP server. Answer with a JSON-RPC error and keep reading.
- C4 Codex self-detection picks the first open rollout. Under D4, pick none when it's ambiguous.
- C5 Per-IP login limits merge behind the Docker Desktop forwarder. Document it, or rely on the per-account limit.
- C6 Docs: two-laptop.md still says local search "arrives with A3", and runbook.md's deletion wording is wrong. Update for D13.

## C. Search UX redesign (D5)

- One address everywhere, `SESSION/ORDINAL[:LINE]`: any unique session prefix works, and `read` also accepts a message id and the raw `path:line`.
- CLI and MCP get 4 tools:
  - `grep` (replaces `find`; `find` stays as an alias). RE2 regex by default, smart-case. Flags: `-F -i -s -w -e -l -c -A/-B/-C -m --limit --offset --kind --exclude-kind --tool --agent --repo NAME|PATH|GLOB --since --until --json`. `-n` and `-r` are accepted as no-ops. It emits every matching line, not just the first, and ends with a footer `[showing X of Y hits in Z sessions; next: --offset N]`.
  - `sessions [GLOB]`: the Glob analogue, newest first.
  - `read ADDR [-A -B -C] [--max-chars 4000] [--line-offset N] [--raw]`: readable text, never escaped JSON, with explicit truncation.
  - `search`: ranked. If an AND query returns nothing, it retries with stopwords dropped as an any-term query. It supports quoted phrases, dedupes identical text, and carries the D2 cap notice.
- MCP: compact text by default, `format:"json"` optional. Shared filters are described once in server `instructions`. `isError` messages are short and include a hint.
- Help: per-verb help that fits a screen, with 3 examples first. `--limit` is an int. An unknown flag gets one line naming the nearest valid flag.
- Keep regression tests: dash-leading patterns, `--` handling, and every printed address round-tripping through `read`.

## D. Verification before landing
- `go test -race ./...` with Postgres and MinIO, plus web tests and build.
- `scripts/e2e-sync.sh`: every scenario, rerun with TLS pinning (D13).
- Full acceptance on the real corpus, re-measured after D3's index-everything change: under 5 minutes, peak under 600MB, idle under 120MB, sweep under 1s CPU, freshness around 2s, parity, and the query set extended with grep/read address round-trips.
- A real `claude -p` and `codex exec` session using the MCP tools.

## E. Execution plan (2026-09-29)

Two waves. Each stream is one Opus agent in its own worktree off `integ/full`, opening a PR into `integ/full`. The orchestrator merges in the order listed and resolves conflicts.

### Wave 1

| Stream | Branch | Owns (files) | Items |
|---|---|---|---|
| parsers | fix/parsers | internal/transcript/** | P1–P6, D6 (emit kinds `injected`, `agent_message`), D11, D16 (stable ids, position ordinals, parser version bumps), D19 |
| localindex | fix/localindex | internal/localindex/**, internal/sqlitemem, the flock and `--once` paths in cmd/flopwire/agent.go and internal/agent/control.go | L1–L6, D12, D3 (local), D7 (local ordering), D8 (local query budget, partial results, counts for the D2 notice), D6 (hide `injected` by default, collapse identical text) |
| agent | fix/agent | internal/agent/** (except the D12 control path), internal/pathpolicy | L7, L8 (plus a Linux CI job), D16 (background re-parse on parser version change), D18, D21, D1 (local) |
| sync | fix/sync | internal/devicesync/**, the client side of internal/syncproto | S1, S3, S5, S6, S8, S9, D15, D20 |
| server | fix/server | internal/ingest, store, api, retrieval (server), backup, auth, migrations, the server side of syncproto, Dockerfile, CI | S2, S7, V1–V8, D9, D10, D14, D3 (server), D1 (server), C5, the build/v1 CI Dockerfile failure |
| tls | fix/tls | internal/client, enrollment and invites, server listener setup, SECURITY.md, docs/, compose files | D13, S4, C6 |

### Wave 2

| Stream | Branch | Items |
|---|---|---|
| search-ux | fix/search-ux | Section C, D2 (notice), D4, D8 (MCP concurrency and cancellation), D17, C1, C3, C4 |

Then section D verification, followed by the re-cut into 4 PRs against main.

## F. Wave 1 outcome (2026-09-29)

All six wave 1 streams are merged into `integ/full` (PRs #40–#45), plus the integrator's follow-ups below.

### Decisions

- **Full index time.** Accepted at about 5m40s on the real corpus (the least loaded run), over the 5-minute bar. The cause is D3: `fts_tri` now indexes the whole stored text. No trigram cap is re-added.
- **Tool text cap.** The parsers' storage cap (`transcript.ToolCap`: head 3KB, tail 1KB, 2KB of error lines) stays for now. Wave 2 measures what `grep` misses because of it and what removing it costs in index size and time.
- **Admin policy field.** `mandatory_denylist` is renamed `path_rules` everywhere: API, domain, Postgres schema, store, agent fetch, web admin UI and docs. It holds D18 rules (`PATTERN` deny, `local:PATTERN`).

### Follow-ups done by the integrator

1. The agent fetches admin path rules over the pinned client (`client.Config.HTTPClient()`). `FetchAdminRules` panics on a nil client.
2. `syncproto.Client.HTTP` is required. A nil client returns `ErrNoHTTPClient`; there is no `http.DefaultClient` fallback.
3. A TLS pin mismatch is permanent (`client.PinError.Permanent()`, `syncproto.Permanent`). The scheduler stops all uploads until the agent restarts, keeps the queue, and `Status.Stopped` shows the error in `flopwire agent status`.
4. The D18 "server copies stay" notice is in `flopwire agent status` (`Response.ServerCopies`: count, first 20 sessions, time). It covers purges since the agent started; after a restart only the log has it.
5. The `path_rules` rename (above).
6. The server's Codex parser reads a fork's parent from the archive (D11). Limit: a fork parsed before its parent is uploaded keeps the copied history; the parent's arrival does not re-parse its forks. Fixing that needs same-generation supersession by absence.
7. Skipped: the server has no parser-version re-parse trigger. It stores `sources.parser` but never compares it, so there is nothing to match `devin-export@1` against `devin.Name`.
8. D20 on the server: `sources` keeps `previous_path`/`previous_file_id`. An old source that arrives after its successor links to it, and its parse supersedes its own rows.
9. Claude `parent_native_id` stays the parent line's uuid. The rule for consumers (documented in the parser, `claude.ParentRow`): the parent row is that line's row with the greatest ordinal in the conversation (`native_id LIKE parent || '#%' ORDER BY ordinal DESC LIMIT 1`). A parent line that yielded no rows resolves to nothing.
10. `Agent.Pass` (`agent run --once` against a running agent) waits for an in-flight Devin poll, then polls. It does not wait for a D16 background re-parse.
11. The inherited index lock descriptor gets close-on-exec back after the re-exec.
12. V4: a parse that needs the purge lock uses a try-lock. When the lock is busy, the source is requeued 5s out without counting an attempt.
13. The `MaxFlushBytes` comment matches the 4MB device cap.

## Final verification outcome (2026-09-29, PR #57)

- Accepted: full index about 7.5 minutes (7m21s measured at load 4–7, 10.0GB), since #50 stores whole tool output.
- Landing is not blocked by two queries over 200ms (`error` 294ms, `sessionpane-path` 246ms). Follow-up: optimize common-term ranking and path queries.
- Follow-up: `api.test` peaks near 2GB under `-race`; find the heavy test.
- Blocking landing: `scripts/e2e-sync.sh` fails 3 of 11 scenarios on integ/full (`0-initial-sync`, `f-devin-session-deleted`, `z-final-consistency`); fix in progress on fix/e2e-sync.

## Before landing (decided 2026-09-29)

- Server-side enforcement of admin path rules at ingest (fix/server-rules), restoring what old commit 004879b did.
- CI: test with MinIO on every PR; e2e-sync on PRs to integ/full and main, required for main; drop agent-linux; add `GOOS=darwin go vet` (PR #56).
- Landing: one integ/full → main merge PR after #16/#17; close #18–#39 as merged via integ/full, and #2–#8 as superseded (keepers verified ported).

## After landing, before the first team

- Multi-user load test: a synthetic N-device harness against Compose (replaces the CASS-era 30-user test from #4).
- Coverage gaps:
  - cross-user and auth negatives (another device's raw, per-user quota, expired invites and tokens; a second user in e2e)
  - web console against a real server (handler httptest, fetch `/` in the Compose check)
  - local search, MCP and path rules in e2e (drive `flopwire mcp` over stdio, local grep, one deny rule)
  - agent startup (plist test, a systemd unit, install docs)
- Performance: the two slow queries; `api.test` near 2GB under -race.
