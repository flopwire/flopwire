# Captured messaging exchange, second run (#55, item 6)

A real exchange between a Claude Code session and a Codex session through
Flopwire, re-captured with the build after #84, #103 and the read receipts.
It replaces the contract examples on the homepage preview (#54). The first
capture, from #79, stays in [`../exchange-capture/`](../exchange-capture/README.md)
for comparison.

**Status: the whole exchange ran for real in one run.** History,
presence, the request, its delivery mid-turn, the `flopwire_send` reply,
its delivery mid-turn, both read receipts and the client update all
happened in run 4 of four, with no retries inside the run. Runs 1 to 3
did not complete an exchange through both hooks. They are described under
[Runs that did not complete](#runs-that-did-not-complete) and are not
captured here.

**This is an own-user exchange.** Both sessions ran as the same OS user on
one device, with a local-only device agent. Every message has
`sender="own"`. Nothing here shows a cross-user exchange.

Every captured file is real output with only the substitutions listed at
the end applied.

## What ran

| Item | Value |
|---|---|
| Flopwire | built from `8c259bd0ecfec9a914de340a48466ee8262f7804` (`origin/main`, merge of #146) |
| Device agent | `flopwire agent run --no-sync`, isolated config, index and bus; `FLOPWIRE_CLOUD=off`; local-only |
| Session A, the API agent | Claude Code 2.1.289, `claude -p --model haiku` (model id `claude-haiku-4-5-20251001`), session `fda51669-afb5-428e-9c0f-3aa204419999` |
| Session B, the client agent | codex-cli 0.160.0, `codex exec -m gpt-5.6-luna`, `model_reasoning_effort="medium"`, `--sandbox workspace-write`, thread `01a10c80-5756-7310-8db5-659b27ee0d69` |
| Date | 2026-10-05 14:37–14:41 UTC |

Both models are the cheapest ones that `flopwire probe` uses. The roles are
the same as in #79: Claude Code commits, because Codex's `workspace-write`
sandbox cannot write a linked worktree's git directory.

## Setup

The setup follows the #79 capture and the `flopwire probe --local`
isolation recipe ([docs/probe.md](../../../docs/probe.md)). Each run had
its own scratch root, shown below as `/tmp/capture`.

1. **Binary and shim.** `go build -o <scratch>/bin/flopwire-real ./cmd/flopwire`.
   `/tmp/capture/bin/flopwire` is a shim, first on `PATH` for both
   sessions. It sets `FLOPWIRE_CONFIG`, `FLOPWIRE_INDEX`,
   `FLOPWIRE_CLOUD=off` and `FLOPWIRE_SOCKET`. The socket path is passed
   relative to the current directory, because the scratch path is longer
   than the 104-byte unix socket limit. For `flopwire hook`, the shim also
   logs the hook's input fields (event, session, tool) and its stdout.
2. **Device agent.** `env -i HOME USER PATH=/usr/bin:/bin FLOPWIRE_CONFIG=… FLOPWIRE_INDEX=… FLOPWIRE_CLOUD=off flopwire-real agent run --no-sync --socket agent.sock --claude-projects /tmp/capture/claude/projects --codex-home /tmp/capture/ch --devin-db - --opencode-db - --sweep 2s`.
   No server and no sync.
3. **Claude Code.** Claude Code runs with the real `~/.claude`, as in the
   probe, because it does not run logged in with an isolated config
   directory.
   - **Run:** `env -i HOME USER LANG TERM=dumb PATH=/tmp/capture/patha:/usr/bin:/bin claude -p --model haiku --plugin-dir <worktree>/plugins/claude-code/flopwire --setting-sources project`.
     `patha` holds only the shim and a `claude` link.
   - **Tools:** `--allowedTools Read Edit Write 'Bash(git:*)' 'Bash(sh scripts/check.sh:*)' mcp__plugin_flopwire_flopwire__flopwire_send mcp__plugin_flopwire_flopwire__flopwire_inbox`,
     the same as in #79. Shell `flopwire` is not allowed.
   - **Exposure to the agent:** a scratch loop copied A's project directory
     from `~/.claude/projects` into `/tmp/capture/claude/projects` with
     `rsync --append` every 0.25 s, and A's `~/.claude/sessions/<pid>.json`
     into `/tmp/capture/claude/sessions/`. The agent saw no other Claude
     session.
4. **Codex home.** `CODEX_HOME=/tmp/capture/ch`. Only `~/.codex/auth.json`
   was copied in (mode 600). Before the run, the next refresh was more
   than 4 days away. The copy was byte-identical at the end of every run
   and was then deleted. `flopwire setup --source <worktree>` ran with
   `PATH` holding only the shim, a `codex` link, `/usr/bin` and `/bin`. It
   installed `flopwire@flopwire` into the isolated home. The five hooks
   were trusted with `[hooks.state."flopwire@flopwire:hooks/hooks.json:<event>:0:0"] trusted_hash = …`
   tables; the hashes came from that home's app-server `hooks/list`.
   `flopwire setup --check` then reported `hooks: 5 of 5 trusted`. The
   documented auto-approve table for `flopwire_send` was added, and the
   project was marked trusted.
5. **Codex hook shell.** Codex 0.160 runs hook commands in the user's
   login shell (`zsh -lc`), and the user's `~/.zprofile` puts
   `~/.local/bin` first on `PATH`. So in run 1, B's hooks ran the user's
   installed `flopwire`, not the shim (finding 1). From run 2 on, B ran
   with `ZDOTDIR` set to an empty scratch directory. The login shell then
   read only `/etc/zprofile`, and `flopwire` resolved to the shim.
6. **Repository.** `/tmp/capture/app` is a git repository with a synthetic
   `src/api/users.ts` (page-number pagination), `src/client/users.ts`, and
   `scripts/check.sh N`, which sleeps 30 s. Its `origin` is
   `git@github.com:acme/app.git`, which was never contacted. It gives the
   repository a remote-based identity, `github.com/acme/app`.
   `/tmp/capture/app-api` is a linked worktree on branch `api-cursors`,
   where A worked. B worked in `/tmp/capture/app` on `main`. Run 4's
   repository was cloned from run 1's, so `git log --all` also shows
   run 1's commit as `5d07830 (origin/api-cursors)`.
7. **Order.** A started first. B started when A's commit landed.

### Prompts

Session A (cwd `app-api`). It does not mention Flopwire. The session got the
plugin's standing instruction at `SessionStart`
([`captured-standing-instruction.txt`](captured-standing-instruction.txt)).
Run 4 used four check stages instead of the eight in #79 (see
[Runs that did not complete](#runs-that-did-not-complete)); the prompt is
otherwise the #79 prompt.

```text
You are the API agent for this repository (current branch: api-cursors). Do these steps in order:

1. In src/api/users.ts, replace page-number pagination on GET /users with an opaque cursor. The request takes `cursor?: string` instead of `page`. The response returns `next_cursor: string | null` instead of `next_page`. The cursor is the base64url encoding of the JSON {created_at, id} of the last user on the page; the next page starts strictly after that position in (created_at, id) order, so pages stay consistent when users are inserted. Do not edit src/client.
2. Commit the change on this branch with git, message: "Switch GET /users to cursor pagination".
3. Run the API check suite, one stage per command, in order: `sh scripts/check.sh 1`, then `sh scripts/check.sh 2`, and so on through `sh scripts/check.sh 4`. Each stage takes about 30 seconds. Run them as four separate commands, one after another; do not combine them and do not run them in the background.
4. Finish with a one-paragraph summary.
```

Session B (cwd `app`), the #79 prompt unchanged:

```text
You maintain the client in src/client/users.ts. Another agent changed the GET /users pagination contract on branch api-cursors, and our client breaks against it. Find the agent session that made that API change, check whether it is still running, ask it what the client must send now, wait for its answer, then update src/client/users.ts to match. Do not edit src/api. Flopwire (agent session history and messaging) is installed.
```

## Timeline (UTC, 2026-10-05, run 4)

| Time | Event |
|---|---|
| 14:37:52.2 | A starts. `SessionStart` prints the standing instruction. |
| 14:38:17.5 | A commits `fc7114b` on `api-cursors` with a plain `git commit -m`; the result has the `[api-cursors fc7114b]` line. |
| 14:38:20.8 | A starts `sh scripts/check.sh 1` (30 s each). |
| 14:38:25.3 | B starts. `SessionStart` prints the standing instruction. |
| 14:38:35–48 | B looks for the messaging skill: first `~/.agents/skills/…`, then an `rg` over the plugin cache, then reads it (finding 5). |
| 14:38:52.8 | **Repository evidence:** `git log --all --oneline --decorate -- src/api src/client/users.ts` gives `fc7114b (api-cursors) Switch GET /users to cursor pagination`. |
| 14:39:02.1 | **History:** B runs shell `flopwire sessions --repo . --branch api-cursors --detail --limit 50` from the main checkout. It returns A's session with `commits: ["fc7114b"]`. |
| 14:39:06.4 | B runs shell `flopwire peers --session fda51669-…`. It fails with `sandbox_blocked`, whose `fix` names the MCP tools (finding 4). |
| 14:39:17.7 | **Presence:** `flopwire_peers session="fda51669-…"` (MCP) returns A as `busy`. |
| 14:39:22.633 | **Contact:** B calls `flopwire_send` with `intent=request` and gets `mfd8a363fd905ad00`, `arrives: "next_tool_call"`. |
| 14:39:22.981 | Delivered to A at `PostToolUse` of `sh scripts/check.sh 2` (14:38:52.8–14:39:23.0), 0.35 s after the send. `read_at` 14:39:23.020. |
| 14:39:24.8 | A: "I received a message from another session asking about the pagination contract. Let me reply to that". It loads the deferred `flopwire_send` through `ToolSearch`. |
| 14:39:29.1 | CLI captures of `sessions` and `peers`, with A busy. |
| 14:39:33.2 | A tries shell `flopwire send … --reply-to mfd8a363fd905ad00`; `claude -p` refuses it ("This command requires approval") (finding 3). |
| 14:39:36.536 | A calls `flopwire_send` with `reply_to=mfd8a363fd905ad00` and gets `m87e873eea6761420`. That is 13.6 s after delivery. A then runs check stages 3 and 4. |
| 14:39:38.6 | B, waiting, runs `sleep 3`. |
| 14:39:41.828 | Delivered to B at `PostToolUse` of that `sleep 3`, 5.3 s after the reply was sent. `read_at` 14:39:41.896. |
| 14:39:52.0 | B patches `src/client/users.ts` ([`captured-client.diff`](captured-client.diff)). |
| 14:40:44.5 | B's turn ends (`SessionEnd`); A ends 1 s later. |
| 14:40:55.0 | CLI captures of `inbox`, both sides. |

From request sent to reply in B's context: **19.2 s** (#79: 1 min 28 s).
Each delivery happened at the recipient's next tool boundary. Neither
session needed a new prompt.

## Captured items

The CLI captures ran in `/tmp/capture/app` as session B, or in
`/tmp/capture/app-api` as session A:
`env -i HOME PATH=/tmp/capture/bin:/usr/bin:/bin FLOPWIRE_SESSION_ID=<B or A> FLOPWIRE_AGENT=<codex or claude> flopwire … </dev/null`.

| File | Command or source |
|---|---|
| [`captured-sessions.json`](captured-sessions.json) | `flopwire sessions --repo . --branch api-cursors` (as B, from the main checkout) |
| [`captured-sessions.txt`](captured-sessions.txt) | the same with `--text` |
| [`captured-peers.json`](captured-peers.json), [`captured-peers.txt`](captured-peers.txt) | `flopwire peers`, `flopwire peers --text` |
| [`captured-peers-session.json`](captured-peers-session.json), [`captured-peers-session.txt`](captured-peers-session.txt) | `flopwire peers --session fda51669-afb5-428e-9c0f-3aa204419999` (and `--text`) |
| [`captured-send.json`](captured-send.json) | B's `flopwire_send` result, as B received it |
| [`captured-send-outcome.txt`](captured-send-outcome.txt) | its `outcome` field, the line `send --text` prints. It was not sent again. |
| [`captured-delivered-to-api.txt`](captured-delivered-to-api.txt), [`.json`](captured-delivered-to-api.json) | the request wrapper A's model received: `additionalContext`, and the hook's whole stdout |
| [`captured-reply-call.json`](captured-reply-call.json), [`captured-reply-receipt.json`](captured-reply-receipt.json) | A's `flopwire_send` call and its result, from A's transcript |
| [`captured-delivered-to-client.txt`](captured-delivered-to-client.txt), [`.json`](captured-delivered-to-client.json) | the reply wrapper B's model received |
| [`captured-inbox-client.json`](captured-inbox-client.json), [`captured-inbox-client.txt`](captured-inbox-client.txt) | `flopwire inbox` (and `--text`) as B |
| [`captured-inbox-api.json`](captured-inbox-api.json), [`captured-inbox-api.txt`](captured-inbox-api.txt) | `flopwire inbox` (and `--text`) as A |
| [`captured-inbox-thread.txt`](captured-inbox-thread.txt) | `flopwire inbox --text --thread mfd8a363fd905ad00` as B: the whole thread text |
| [`captured-client-tool-calls.txt`](captured-client-tool-calls.txt) | B's `git log` and every Flopwire call (shell and MCP), in order, with the result B saw. Codex 0.160 runs tools through its `exec` code tool, so each call is a short script. |
| [`captured-standing-instruction.txt`](captured-standing-instruction.txt) | `SessionStart` hook output, the same in both sessions |
| [`captured-client.diff`](captured-client.diff) | `git diff` of B's client change |

The two `.txt` wrappers equal, byte for byte, the
`hook_additional_context` attachment in A's transcript and the developer
message in B's rollout. Each `.txt` capture adds one final newline.

## Mapping onto the homepage fixtures

The fixtures are in `landing/coordination-preview/fixtures/` on
`design/homepage-layout-audit` (#54). They were made by `capture.py` with
synthetic transcripts and the story "Rename user response field"
(`api-users`, `a81f3c2`). This capture uses the pagination story
(`api-cursors`, `fc7114b`).

| Fixture | Captured replacement | Differences from the fixture |
|---|---|---|
| `intended-sessions.json`, `branch-session.json` | `captured-sessions.json` | Same concise shape and `excluded` note. Each row adds `intent` and `files`. `commits` is `["fc7114b"]`. `repo` is the worktree's full path, not `/work/acme/app`. |
| `sessions-text.txt` | `captured-sessions.txt` | Same `key=value` header form and `last:` line, with `files=1` added. `repo=app-api` is the checkout's directory name, not the remote. |
| `intended-peers.json` | `captured-peers-session.json` (one peer); `captured-peers.json` (default) | Each peer adds `remote` (`github.com/acme/app`) and `main` (the main checkout's path). The default `peers` lists only A. |
| `intended-send.json` | `captured-send.json` | Same fields, including `arrives: "next_tool_call"` and the `next` hint for a busy recipient. `to.repo` is a full path. |
| `reply-receipt.json` | `captured-reply-receipt.json` | Same fields. `intent` is `inform` (the default) and it has no `next`, because only a request gets one. |
| `delivered-request.json` | `captured-delivered-to-api.json` | Same hook-output shape and wrapper attributes. The request carries the `Reply with the flopwire_send tool…` line. |
| `delivered-reply.json` | `captured-delivered-to-client.json` | Same shape; adds `reply-to="mfd8a363fd905ad00"`. A multi-line Markdown body arrives as is. |
| `intended-inbox.json` | `captured-inbox-client.json` | Same order (received reply first), `thread_id`, `reply_to`, `direction`, `is_reply`. **Both messages are `state: "read"` with `read_at`**, not `delivered`. |
| (none) | `captured-inbox-api.json`, `captured-inbox-*.txt`, `captured-inbox-thread.txt` | The API side's view: the request `received`, the reply `sent`, both `read`. `--text` prints `read 2026-10-05 14:39Z` per message. |
| `intended-grep.txt`, `intended-read.txt` | not replaced | Retrieval output, outside this exchange. |

## Differences from the #79 capture

- **Read receipts.** Every message in both inboxes is `read`, with
  `read_at` 39 and 68 ms after `delivered_at`. In #79 the state stopped at
  `delivered`. `--text` shows `read <time>` instead of the state.
- **Concise `sessions`.** The default JSON is one short row per session
  (`session_id`, `address`, `agent`, `repo`, `branches`, `live`,
  `last_activity_at`, `messages`, `title`, `intent`, `commits`, `files`).
  The #79 shape, with `cwd`, `device`, timestamps and the whole `digest`,
  now needs `--detail`. B asked for `--detail` itself; its output is in
  `captured-client-tool-calls.txt`.
- **`.commits` is filled.** `commits: ["fc7114b"]` is on the row, and
  `commits=1` is in the text header. Haiku committed without `-q`, so this
  run does not exercise the #103 quiet-commit path.
- **`key=value` headers.** The `sessions --text` header leads with the
  full session id followed by `key=value` fields. B made no `flopwire_read`
  call in run 4.
- **Remote-based repo identity.** The repository has a remote, so the main
  checkout and the linked worktree share the identity
  `github.com/acme/app`. `sessions --repo .` from the main checkout found
  the worktree's session at once; in #79 (no remote) it returned nothing.
  `peers` adds `remote` and `main`. The JSON `repo` field and the wrapper's
  `repo="app@main"` / `repo="app-api@api-cursors"` still name the checkout.
- **`arrives` values.** Both receipts say `arrives: "next_tool_call"`,
  because each recipient was busy, and the outcome line says "busy,
  arrives at its next tool call". The request receipt also has `next`,
  which tells the sender how the reply will arrive. Both receipts' `to`
  now carries `busy`.
  The runs that did not complete also showed `only_if_resumed` and
  `next_prompt` (finding 6).
- **Delivery latency.** 0.35 s to A and 5.3 s to B (#79: 4.3 s and 40.8 s).
  Both are the time to the recipient's next tool boundary.
- **`sandbox_blocked` points to MCP.** B's shell `flopwire peers` failed
  with a `fix` that names the MCP tools, and B switched to them.

## Runs that did not complete

The same setup ran four times; each run had its own scratch root, agent,
index and bus. Only run 4 is captured.

| Run | A's check stages | What happened |
|---|---|---|
| 1 | 8 | B's hooks ran the user's installed `flopwire` (finding 1), so they reached the user's own device agent, not the scratch one, and printed nothing. The request reached A 7.4 s after the send. A finished all eight stages first and replied 4 min after delivery. Its receipt said `next_prompt` (finding 6), and the reply stayed `queued`: no hook of B's reached the scratch agent. B polled `flopwire_inbox` and read A's transcript with `flopwire_read`, then patched. |
| 2 | 8 | Hooks fixed with `ZDOTDIR`. The request reached A 8.9 s after the send. After the eight stages, A tried shell `flopwire send`, which was refused, ran `echo "Waiting for approval to send message..."`, and ended. It never replied. B read A's transcript and the API diff, then patched. |
| 3 | 8 | The request reached A 23.5 s after the send. A replied with `flopwire_send` after all eight stages, 3 min 29 s after delivery. B had ended 13 s earlier, after it patched from the committed diff. The receipt said `only_if_resumed`, and the reply stays `queued`. |
| 4 | 4 | Complete; captured above. A replied 13.6 s after delivery. |

## Findings

1. **Codex runs hooks in the user's login shell.** Codex 0.160 starts hook
   commands with `zsh -lc`, so `flopwire` resolves through the user's
   shell profile, not the `PATH` Codex was started with. In run 1, B's
   hooks ran `~/.local/bin/flopwire` with the user's own config and agent.
   The standing instruction still arrived, but no message did. For a
   normal install the two `flopwire` binaries are the same. For any
   isolated run (this capture, a manual probe) the hook leaves the
   isolation. `flopwire probe` is not affected: its project hooks call the
   binary by absolute path. Any recipe that relies on `PATH` for Codex
   hooks needs `ZDOTDIR` or an absolute path.
2. **The cheapest Claude model defers or drops replies.** Haiku saw the
   request mid-turn in all four runs. In runs 1 and 3 it finished the
   check suite before replying. In run 2 it never replied. In run 4, with
   four stages, it replied at once. Sonnet in #79 replied after one
   stage. The standing instruction does not say when to reply.
3. **A's first reply goes through the shell.** In runs 2 and 4, A first
   tried shell `flopwire send`, which the session's tool rules refused. In
   run 4 it fell back to `flopwire_send` 3 s later; in run 2 it stopped.
   This is #79 finding 4 again. The standing instruction names both
   paths; it does not say which one to prefer.
4. **Shell `sessions` works in the Codex sandbox; `peers` does not.**
   `sessions` reads the index file, while `peers`, `send` and `inbox` need
   the agent socket, which `workspace-write` blocks. The `sandbox_blocked`
   error's `fix` steered B to the MCP tools on the first try.
5. **The client agent still searches for the skill.** In all four runs B
   first read a path that does not exist (`~/.agents/skills/flopwire:messaging/SKILL.md`,
   or `<CODEX_HOME>/plugins/cache/flopwire/local/skills/…`, one
   `flopwire/` level short of the real cache path), then searched with
   `find` or `rg` before it read the skill. This is #79 finding 3.
6. **`arrives` covered three cases across the runs.** Run 4's receipts
   say `next_tool_call` (recipient busy). In run 3, B had ended 13 s
   before A replied: the receipt said `only_if_resumed` ("not running,
   arrives only if it resumes") and the reply stays `queued`, as
   documented. In run 1 the receipt said `next_prompt` ("idle, arrives
   with its human's next prompt") while B was mid-turn and polling,
   because B's hooks reached another agent (finding 1), so the scratch
   agent never saw B busy.
7. **The reply still needs the waiting agent to make tool calls.** In
   run 4, B ran `sleep 3` and the reply arrived at its end. In run 3, B
   gave up after about 3 minutes of polling and patched from the diff.

## Substitutions

Only these strings were replaced, in this order, in every captured file.
Session ids, message ids, commit hashes, timestamps, bodies and every
other value are as captured.

| Captured | Replaced with | Where |
|---|---|---|
| The run 4 scratch root (`/private/tmp/claude-501/-Users-<account>-Code-flopwire/<capturing session id>/scratchpad/capture/r4`) | `/tmp/capture` | `repo`, `main`, `cwd`, `workdir`, digest `repos` |
| The home directory `/Users/<account>` | `/Users/dev` | no captured file (B's skill lookup is not in `captured-client-tool-calls.txt`) |
| The device hostname | `devbox.local` | `device` in peers |
| The OS account name | `dev` | `user`, `user_id` (`local:dev`), `user_name`, `to_user`, `to_user_id`, wrapper `user=`, text views, `outcome` |

No email address, token or environment value appears in the captures.
`grep` for the account name, the hostname, the scratch path and the email
addresses found nothing before commit.
