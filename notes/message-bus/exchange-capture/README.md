# Captured messaging exchange (#55, item 6)

A second capture with the build of 2026-10-05 (read receipts, concise
`sessions`, remote-based repo identity) is in
[`../exchange-capture-2026-10-05/`](../exchange-capture-2026-10-05/README.md).
This directory is kept as it was captured.

A real exchange between a Claude Code session and a Codex session through
Flopwire, captured to replace the contract examples on the homepage
preview (#54). Tracker: #73.

**Status: the whole exchange ran for real, with one gap.** History, presence,
the request, its delivery mid-turn, the `flopwire_send` reply, its delivery
mid-turn and the client update all happened in one run, with no retries.
The gap: the `sessions` digest has **no `commits` field**. Session A
committed with `git commit -q`, which prints no `[branch sha]` line, and the
digest only records commits from that line (finding 1). Session B matched
the commit another way: `git log` gave `650a939 Switch GET /users to cursor
pagination`, and `flopwire_read --outline` of the session showed the same
`git commit -q -m "Switch GET /users to cursor pagination…"` call. No
capture is edited to add the commit.

**This is an own-user exchange.** Both sessions ran as the same OS user on
one device, with the device agent in local-only mode (no server). Every
message therefore has `sender="own"`. Nothing here shows a cross-user
(`sender="teammate"`) exchange.

Every captured file is real output with only the substitutions listed at
the end applied.

## What ran

| Item | Value |
|---|---|
| Flopwire | built from `f03fc99c9038e8bedd416491b65287b577d2e6a9` (`origin/main`, merge of #78) |
| Device agent | `flopwire agent run --no-sync`, isolated config, index and bus; local-only |
| Session A, the API agent | Claude Code 2.1.287, `claude -p --model sonnet` (model id `claude-sonnet-5-5`), session `b5dd812f-aae0-4bb1-8363-45dee116e371` |
| Session B, the client agent | codex-cli 0.159.3, `codex exec` with `gpt-6-luna`, `model_reasoning_effort="medium"`, `--sandbox workspace-write`, thread `01a0fa0a-b16a-7ca2-87a6-092f0f36cd28` |
| Date | 2026-10-02 00:36–00:41 UTC |

Why Claude Code is the committer: Codex's `workspace-write` sandbox refuses
writes to the git directory (`.git/worktrees/app-api/index.lock: Operation
not permitted`), even with that directory added as a writable root. So the
API agent that must commit is Claude Code, and the client agent is Codex.
That also matches the `agent` values in the homepage fixtures.

## Setup

All of it ran in one scratch directory, shown below as `/tmp/capture`.

1. **Binary and shim.** `go build -o /tmp/capture/bin/flopwire-real ./cmd/flopwire`.
   `/tmp/capture/bin/flopwire` is a shim, first on `PATH` for both sessions.
   It sets `FLOPWIRE_INDEX=/tmp/capture/fw/index.db` and sets
   `FLOPWIRE_CONFIG` to `/tmp/capture/fw/config.json` as a path relative to
   the current directory. The real scratch path is too long for the
   104-byte unix socket limit, so the shim passes it relative. For
   `flopwire hook` the shim also appends the hook's stdout, with the event
   name and session id from its input, to a log. It logs no environment.
   Item 4 and item 6 come from that log, and they match the
   `hook_additional_context` attachment in A's transcript and the developer
   message in B's rollout byte for byte.
2. **Device agent.** It ran in `/tmp/capture/fw` with
   `env -i HOME USER PATH=/usr/bin:/bin FLOPWIRE_CONFIG=config.json FLOPWIRE_INDEX=… flopwire-real agent run --no-sync --claude-projects /tmp/capture/clidx/projects --codex-home /tmp/capture/ch --devin-db - --sweep 2s`.
   No server and no sync.
3. **Codex home.** `CODEX_HOME=/tmp/capture/ch`. Only `~/.codex/auth.json`
   was copied in (mode 600, never printed). Its checksum was the same at
   the end, so no token refresh happened. The copy is deleted.
   `flopwire setup --source <worktree>` ran with `PATH` holding only the
   shim, a `codex` link, `/usr/bin` and `/bin`, so `claude` was not found.
   It installed `flopwire@flopwire` into the isolated home. Hook trust went
   into the isolated `config.toml` as four
   `[hooks.state."flopwire@flopwire:hooks/hooks.json:<event>:0:0"] trusted_hash = …`
   tables. The hashes came from that home's app-server `hooks/list` and are
   the same as in #78. Then `flopwire setup --check` reported `hooks: 4 of 4 trusted`.
   The documented auto-approve table was added there too:
   `[plugins."flopwire@flopwire".mcp_servers.flopwire.tools.flopwire_send] approval_mode = "approve"`.
4. **Claude Code.** Claude Code would not run with an isolated
   `CLAUDE_CONFIG_DIR` (`Not logged in`). Session A therefore used the real
   `~/.claude`. Claude Code itself wrote A's transcript and its
   `sessions/<pid>.json` there. Settings were not touched.
   - **Run:** `env -i HOME USER LANG TERM=dumb PATH=/tmp/capture/bin:… claude -p --plugin-dir <worktree>/plugins/claude-code/flopwire --setting-sources project`.
     No `CLAUDE_CODE_*` or `CLAUDECODE` variable from the parent session
     was set.
   - **Tools:** `--allowedTools Read Edit Write 'Bash(git:*)' 'Bash(sh scripts/check.sh:*)' mcp__plugin_flopwire_flopwire__flopwire_send mcp__plugin_flopwire_flopwire__flopwire_inbox`.
     The claude.ai Docs connector tools were disallowed.
   - **Exposure to the agent:** a scratch script exposed only session A to
     the isolated agent. It hard-linked A's transcript into
     `clidx/projects/<project>/`, which is the same inode, so appends show
     at once. It also copied A's `~/.claude/sessions/<pid>.json` into
     `clidx/sessions/` every 0.5 s. The agent read A's presence (pid
     alive, `status` busy) from that copy. This copy is the one part of the
     presence chain that the capture harness supplied. The agent saw no
     other Claude session.
5. **Repository.** `/tmp/capture/app` is a git repository with a synthetic
   `src/api/users.ts` (page-number pagination), `src/client/users.ts`, and
   `scripts/check.sh N`, which sleeps 30 s. `/tmp/capture/app-api` is a
   linked worktree on branch `api-cursors`, where A worked. B worked in
   `/tmp/capture/app` on `main`.
6. **Order.** A started first. B started after A's commit
   landed, while A was in its check stages.

### Prompts

Session A (cwd `app-api`). It did not mention Flopwire. The session got the
plugin's standing instruction at `SessionStart`
([`captured-standing-instruction.txt`](captured-standing-instruction.txt)).

```text
You are the API agent for this repository (current branch: api-cursors). Do these steps in order:

1. In src/api/users.ts, replace page-number pagination on GET /users with an opaque cursor. The request takes `cursor?: string` instead of `page`. The response returns `next_cursor: string | null` instead of `next_page`. The cursor is the base64url encoding of the JSON {created_at, id} of the last user on the page; the next page starts strictly after that position in (created_at, id) order, so pages stay consistent when users are inserted. Do not edit src/client.
2. Commit the change on this branch with git, message: "Switch GET /users to cursor pagination".
3. Run the API check suite, one stage per command, in order: `sh scripts/check.sh 1`, then `sh scripts/check.sh 2`, and so on through `sh scripts/check.sh 8`. Each stage takes about 30 seconds. Run them as eight separate commands, one after another; do not combine them and do not run them in the background.
4. Finish with a one-paragraph summary.
```

Session B (cwd `app`). This is the first and only prompt. Its last sentence
is the one hint about Flopwire. No tool names or steps were given.

```text
You maintain the client in src/client/users.ts. Another agent changed the GET /users pagination contract on branch api-cursors, and our client breaks against it. Find the agent session that made that API change, check whether it is still running, ask it what the client must send now, wait for its answer, then update src/client/users.ts to match. Do not edit src/api. Flopwire (agent session history and messaging) is installed.
```

## Timeline (UTC, 2026-10-02)

| Time | Event |
|---|---|
| 00:36:15.9 | A starts. `SessionStart` prints the standing instruction. |
| 00:36:24.8 | A commits `650a939` on `api-cursors` (`git add … && git commit -q -m … && git log --oneline -1`). |
| 00:36:26.7 | A starts `sh scripts/check.sh 1` (30 s each). |
| 00:36:40.1 | B's turn starts. |
| 00:36:52 | **Repository evidence:** B runs `git log --all --oneline --decorate -- src/api src/client/users.ts` and gets `650a939 (api-cursors) Switch GET /users to cursor pagination`. |
| 00:36:52–00:37:13 | **History:** `flopwire_sessions repo="." branch="api-cursors"` returns `[no sessions]` (finding 2), and `flopwire_grep "GET /users" repo="."` returns `[no matches]`. B drops `repo`, and `flopwire_sessions branch="api-cursors"` returns `b5dd812f`. **Presence:** `flopwire_peers session="b5dd812f"` returns `live busy`. B checks the session with `flopwire_read b5dd812f --outline`, which shows the `git commit -q` call. B also reads the messaging skill, after two wrong paths. |
| 00:37:24.472 | **Contact:** B calls `flopwire_send` with `intent=request` and gets message `m3fb586c92d74b7d1`. The receipt says `busy, arrives at its next tool call`. |
| 00:37:28.725 | Delivered to A: the agent's `delivered_at`. The hook printed the message at `PostToolUse` of A's `Bash(sh scripts/check.sh 2)`, which ran 00:36:58.6–00:37:28.8. That is 4.3 s after the send, at the first tool boundary. |
| 00:37:33.9 | A's first reply attempt is a shell `flopwire send … --reply-to m3fb586c92d74b7d1 -- "…"` with `sh scripts/check.sh 3` on the next line. Claude Code refuses it because shell `flopwire` is not in the session's allowed tools (finding 4). |
| 00:37:37.9 | A runs `check.sh 3` and, in parallel, `ToolSearch select:mcp__plugin_flopwire_flopwire__flopwire_send`. The tool was deferred. Both return at 00:38:08. |
| 00:38:11.461 | A calls `flopwire_send` with `reply_to=m3fb586c92d74b7d1` and gets message `m22f4b3d36892f170`. Its receipt says `busy, arrives at its next tool call`. That is 43 s after delivery, most of it the 30 s check stage. A then goes on with `check.sh 4`. |
| 00:38:22.2 | B, waiting for the answer, calls Codex's `sleep` tool for 30 s. |
| 00:38:52.267 | Delivered to B, at `PostToolUse` of that sleep. The hook input names the tool `clocksleep`. That is 40.8 s after the reply was sent. |
| 00:39:08 | B patches `src/client/users.ts` ([`captured-client.diff`](captured-client.diff)). |
| 00:39:13 | CLI captures of `sessions` and `peers`, with A still busy. |
| 00:39:16 | B's turn completes. |
| 00:40:54 | A finishes all 8 stages and its summary. |
| 00:41:15 | CLI captures of `inbox`. |

Totals: from request sent to reply received, 1 min 28 s. Each delivery
waited for the recipient's next tool boundary. Neither session needed a new
prompt.

## Captured items

The CLI captures (items 1, 2, 7 and 8) ran in `/tmp/capture/app` as session
B: `env -i HOME PATH=/tmp/capture/bin:/usr/bin:/bin FLOPWIRE_SESSION_ID=01a0fa0a-b16a-7ca2-87a6-092f0f36cd28 FLOPWIRE_AGENT=codex flopwire … </dev/null`.
Without these variables, the CLI would have named the capturing session as
its caller. B's shell could not run these commands itself, because
`workspace-write` blocks the agent socket (`sandbox_blocked`), so B used
the MCP tools. B's own tool calls and their exact results are in
[`captured-client-tool-calls.txt`](captured-client-tool-calls.txt).

| # | File | Command or source |
|---|---|---|
| 1 | [`captured-sessions.json`](captured-sessions.json) | `flopwire sessions --repo /tmp/capture/app-api --branch api-cursors --json` |
| 2 | [`captured-peers.json`](captured-peers.json) | `flopwire peers` |
| 2 | [`captured-peers-session.json`](captured-peers-session.json) | `flopwire peers --session b5dd812f-aae0-4bb1-8363-45dee116e371` |
| 3 | [`captured-send.json`](captured-send.json) | B's `flopwire_send` result: `{"to":"b5dd812f-aae0-4bb1-8363-45dee116e371","intent":"request","message":"…"}` |
| 4 | [`captured-delivered-to-api.txt`](captured-delivered-to-api.txt) | `additionalContext` of A's `PostToolUse` hook output, which is the text A's model received |
| 5 | [`captured-reply-call.json`](captured-reply-call.json), [`captured-reply-receipt.json`](captured-reply-receipt.json) | A's `flopwire_send` tool call and its result, from A's transcript |
| 6 | [`captured-delivered-to-client.txt`](captured-delivered-to-client.txt) | `additionalContext` of B's `PostToolUse` hook output |
| 7 | [`captured-inbox.json`](captured-inbox.json) | `flopwire inbox` |
| 8 | [`captured-peers.txt`](captured-peers.txt), [`captured-peers-session.txt`](captured-peers-session.txt) | `flopwire peers --text`, `flopwire peers --session b5dd812f-aae0-4bb1-8363-45dee116e371 --text` |
| 8 | [`captured-inbox.txt`](captured-inbox.txt) | `flopwire inbox --text` |
| 8 | [`captured-send-outcome.txt`](captured-send-outcome.txt) | The `outcome` field of item 3. `send --text` prints this line. It was not run again, because that would have sent a second message. |
| — | [`captured-client-tool-calls.txt`](captured-client-tool-calls.txt) | Every Flopwire MCP call B made, in order, with its result (text format, as B chose) |
| — | [`captured-standing-instruction.txt`](captured-standing-instruction.txt) | `SessionStart` hook output, the same in both sessions |
| — | [`captured-client.diff`](captured-client.diff) | `git diff` of B's client change |

`flopwire inbox --thread m3fb586c92d74b7d1` returned the same two messages
as item 7.

## Mapping onto the homepage fixtures

The fixtures are in `landing/coordination-preview/fixtures/` on
`design/homepage-layout-audit` (#54).

| Fixture | Captured replacement | Differences from the contract example |
|---|---|---|
| `intended-sessions.json` | `captured-sessions.json` | **No `digest.commits`** (finding 1). The default `sessions --json` output is not the lean envelope. Each session also has `address`, `id`, `cwd`, `repo`, `device`, `started_at`, `last_activity_at`, `messages`, `live`, and a larger `digest` (`intent`, `repos`, `branches`, `duration_s`, `messages`, `files_edited`, `commands`, `tools`, `failed`, `tokens`). The envelope adds `excluded`. `title` is the first line of the session's prompt. `repo` is a full path, and the linked worktree counts as its own repo. The fixture's story ("Rename user response field", `api-users`, `a81f3c2`) is not the pagination story; the capture uses `api-cursors` and `650a939`. |
| `intended-peers.json` | `captured-peers-session.json` (one peer); `captured-peers.json` (default, all peers) | The envelope adds `kind`, `total`, `more`, `limit` and `caller` (B's session). Each peer adds `user_id`, `user_name`, `device`, `title`, `own` and `seen_at`. `repo` is a full path, not `/work/acme/app`. `user` is the OS account name (substituted, see below), not a display name. The default `peers` also lists an unrelated session that had ended (finding 6). The same full session id is in history, presence and the send. |
| `intended-send.json` | `captured-send.json` | Field order is `kind, id, thread_id, state, to, …`. `to` adds `agent`, `user`, `user_id`, `repo`, `branch` and `live`. The receipt adds `sender: "own"`, `intent`, `sent`, `expires_at`, `from`, `arrives: "next_tool_call"` and `outcome`. `state` is `queued` and `thread_id` equals `id`, as in the contract. |
| `intended-inbox.json` | `captured-inbox.json` | The order matches: received answer first, sent request second. `thread_id`, `reply_to`, `from`, `agent`, `direction` and `is_reply` match the contract. The sent request has no `reply_to`. Each message adds `user`, `user_id`, `repo`, `branch`, `sender`, `intent`, `sent`, `expires_at`, `to_session`, `to_agent`, `to_user`, `to_user_id`, `addressed`, `seq`, `state` (`delivered`) and `delivered_at`. The reply's `intent` is `inform`, the `flopwire_send` default, not a separate reply intent. |
| `intended-grep.txt`, `intended-read.txt` | not replaced | Retrieval output, outside this exchange. B's real `flopwire_read --outline` output is in `captured-client-tool-calls.txt`. |
| (none) | `captured-delivered-to-api.txt`, `captured-delivered-to-client.txt` | No fixture yet. The wrapper matches the format in `docs/agent.md` ("What the recipient sees"). The reply wrapper carries `reply-to="m3fb586c92d74b7d1"`. Its body escapes `&` and `<…>` (`&amp;cursor=&lt;next_cursor&gt;`). Only the request carries the `Reply with the flopwire_send tool…` line. |

## Findings: what did not go smoothly

1. **The digest misses a quiet commit.** A ran `git commit -q`, so the
   tool result had no `[api-cursors 650a939] …` line. `digest.commitOut`
   matches only that line, so `sessions` showed `files_edited` but no
   `commits`. The homepage's "history shows the commit" step depends on
   this field. A fix could also read the hash from `git log`/`rev-parse`
   output in the same command, or from the transcript's git state. Not
   fixed here.
2. **`--repo .` from the main checkout does not find a session in a linked
   worktree.** The scratch repository has no remote, so its identity is
   the checkout path. The worktree `app-api` and the checkout `app` count
   as two repos (`repo="app@main"` and `repo="app-api@api-cursors"` in the
   wrappers). B's first `sessions` and `grep` calls with `repo="."`
   returned nothing. B recovered on its own by filtering on the branch
   alone. With a shared remote this may not happen, but that was not tested.
3. **B looked for the messaging skill twice in the wrong place** (`~/.agents/skills/…`,
   then a wrong cache path) before reading it. Codex lists the user's real
   `~/.agents/skills` as skill root `r0` even with an isolated
   `CODEX_HOME`. B only read there; it wrote nothing.
4. **A's first reply went through the shell and was refused.** A put the
   suggested shell `flopwire send` and the next check stage in one Bash
   call. The session allowed only `Bash(git:*)` and `Bash(sh scripts/check.sh:*)`,
   so `claude -p` refused the call. A then loaded the deferred
   `flopwire_send` tool through `ToolSearch` and replied with it. With
   shell `flopwire` allowed, the reply would have gone out about 35 s
   sooner, but through the shell. In Codex the shell path fails anyway
   (`sandbox_blocked`).
5. **The reply reached B only because B kept making tool calls.** B said
   it was "holding off … the message is queued for its next tool call" and
   then called Codex's `sleep` tool for 30 s. The reply arrived at the end
   of that call. A model that ended its turn instead would have seen the
   reply only at its next prompt (`flopwire inbox` would still show it).
6. **`peers` reports ended sessions as live for a while.** A probe Codex
   session (`01a0fa0a-1c5f-…`, "Reply with just: ok"), which ended at
   00:35, was still `live idle` at 00:39 and 00:41. Session A was still
   `live idle` at 00:41, after its process exited at 00:40:54. The cause is
   the transcript-recency live window. The default `peers` capture keeps
   that probe session as it was printed. *Superseded 2026-10-02:* ended
   sessions are now tracked on harness evidence (a `SessionEnd` hook, a
   dead pid, or a missing registry entry; #67, #82) and leave `peers` at
   once; their waiting messages become `undelivered` with
   `session_ended`. An idle session that is still open counts as live,
   up to `LiveCap` (1 h), which is intended (B3). See `field-observation-2026-10-05.md` finding 4.
7. **No model ignored a message.** No approval prompt appeared in Codex,
   because of the auto-approve table. B hit no sandbox block, because it
   used only MCP for Flopwire. Neither session retried.
8. **Capture harness limits.** Claude Code cannot run with an isolated
   config directory without a new login. Its presence therefore reached
   the isolated agent through the registry copy described in setup step 4.
   The Codex sandbox blocks commits in a linked worktree, which decided
   the roles.

## Substitutions

The files were captured and then substituted. Only these strings were
replaced, in this order, in every file of this directory. Session ids,
message ids, thread ids, commit hashes, timestamps, bodies and every other
value are as captured.

| Captured | Replaced with | Where |
|---|---|---|
| The scratch directory path (`/private/tmp/claude-501/-Users-<account>-Code-flopwire/<capturing session id>/scratchpad/capture55`) | `/tmp/capture` | `repo`, `cwd`, outline headers |
| The home directory `/Users/<account>` | `/Users/dev` | B's skill lookup in `captured-client-tool-calls.txt` |
| The device hostname | `devbox.local` | `device` in peers |
| The OS account name | `dev` | `user`, `user_id` (`local:dev`), `user_name`, `to_user`, `to_user_id`, wrapper `user=`, text views, `outcome` |

No email address, token or environment value appears in the captures.
`grep` for the account name, the home path and the email addresses found
nothing before commit.
