# Searching transcripts

Flopwire gives people and agents four tools over coding-agent transcripts
(Claude Code, Codex, Devin). The CLI and the MCP server have the same
four, with the same flags and the same output. `grep`, `search` and
`read` print text by default (`--json` for JSON). `sessions` prints JSON
by default (`--text` for readable rows). See [Output](#output).

| Tool | CLI | MCP | Use it for |
|---|---|---|---|
| grep | `flopwire grep` (alias `find`) | `flopwire_grep` | Exact strings and regexes, like rg |
| search | `flopwire search` | `flopwire_search` | Fuzzy and natural-language questions |
| sessions | `flopwire sessions` | `flopwire_sessions` | Listing or globbing sessions |
| read | `flopwire read` | `flopwire_read` | Reading what an address points to |

All four read the local index by default. Add `--server` to query the
team server. `flopwire <tool> --help` shows three examples and every flag.

## Addresses

Every result starts with an address. Pass it to `read`.

| Address | Means |
|---|---|
| `0b7e2c1a/28672:14` | Line 14 of the message at ordinal 28672 of session `0b7e2c1a…` |
| `0b7e2c1a/28672` | That message |
| `0b7e2c1a` | The session, read from its start |
| `self` | The calling agent's own session (see [Your own session](#your-own-session)) |
| `4211` (local) or a UUID (server) | A message id |
| `/Users/me/.claude/projects/x/abc.jsonl:8` | The message(s) at line 8 of a transcript |

A session prefix works when it names one session, as with git's short
hashes. Printed addresses use the shortest unique prefix of 8 or more
characters. An ambiguous prefix gets an error that lists the candidates.

## grep

```sh
flopwire grep 'upload\.test.*timeout'                # RE2 regex, smart case
flopwire grep -F 'exit status 1' --since 7d -C 2     # literal, 2 lines of context
flopwire grep -l flaky --repo flopwire --agent codex   # sessions with matches
```

Output, newest message first, grouped under one header line per session
(rg's `--heading` layout):

```
## session: 0b7e2c1a agent: claude ended: 2026-09-23 repo: alpha branch: main files: 2 pr: #43 commits: 1 failed: 1 intent: "why does the login test flake?"
16302080-1- tests/login.test.ts:14: expected 200, received 401
16302080:2 tool_result/Bash error: exit code 1
[3 hits in 3 sessions]
```

- The header is the session's short digest (see [Session digests](#session-digests))
  as labeled fields (see [Labeled fields](#labeled-fields)):

  | Key | Value |
  |---|---|
  | `session` | The session's address: its shortest unique id prefix |
  | `who` | `user@device`, on the team server |
  | `agent` | `claude`, `codex` or `devin` |
  | `live` or `ended` | `live: 4m` (since its last activity) or `ended: 2026-09-23` |
  | `repo`, `branch` | The repo's name; the branch, or `a→b` when it switched |
  | `files`, `pr`, `prs`, `commits`, `failed` | Files edited, the first PR, how many PRs, commits, failed tool calls. A `+` means the digest capped the list |
  | `intent` | What the session was for, cut to 80 characters at a word. Always last |

  Fields without a value are left out. The header stays within about 260
  bytes: long names are cut, then the intent, then `who`, `repo` and
  `branch` go. A session indexed so recently that it has no digest yet
  gets `session`, `who`, `agent`, the time, `repo` and `branch` only.
- Under the header, a matching line is `ORDINAL:LINE kind/tool: text` on
  a message's first matching line and `ORDINAL:LINE: text` on the others.
  A context line is `ORDINAL-LINE- text`. The address for `read` is
  `SESSION/ORDINAL:LINE`, with SESSION from the header. Flags follow the
  kind: `error`, `+N copies`, `superseded`, `off-path`.
- When a session's hits resume after another session's, a short header
  (`## session: SESSION`) opens them again. A page cut by `--offset` or the output
  budget starts with a full header.
- `--no-heading` prints the flat form instead: one line per hit,
  `SESSION/ORDINAL:LINE: [agent kind time repo@branch] text`.
- Every matching line prints, at most 10 per message. A message with more
  says how many more and how to read them.
- Long lines are cut to about 300 bytes around the match.
- The footer gives the totals and the next page: `[showing 1-20 of 57
  hits in 9 sessions; next: --offset 20]`. A `+` after a total means
  counting stopped early and the total is a lower bound.
- Identical texts show once, marked `+N copies`.

Flags follow rg: `-e PAT` (repeat for alternatives), `-F`, `-i`, `-s`,
`-w`, `-l`, `-c`, `-A/-B/-C N` (lines), `-m N` (hits per session),
`--limit`, `--offset`. `-n` and `-r` are accepted and do nothing. Put
`--` before a pattern that starts with `-`, or use `-e`.

- `-o` (`--only-matching`, MCP `only_matching`) prints only the matched
  text of each match, one per line, with its address. Context flags do
  not apply.
- `-U` (`--multiline`, MCP `multiline`) lets `.` match a newline, so a
  match may span the lines of one message, never two messages. The hit
  prints every line of the match (at most 50; the rest are counted).
- `--sort newest` (the default), `oldest`, or `relevance`: the messages
  with the most matches first, ties newest first. Relevance counts every
  match before it pages, so it reads more of the index than `newest`.

Smart case: a pattern with an uppercase letter matches case; else case is
ignored. `-i` and `-s` override it.

## search

```sh
flopwire search sqlite trigram tokenizer
flopwire search '"exponential backoff" retry' --repo flopwire
flopwire search papercut --agent codex --since 30d --offset 20
```

- BM25 ranking. Prompts and replies (user, assistant, agent_message) rank
  above tool calls and tool output that match as well. `--sort newest` or
  `oldest` orders the matching messages by time instead.
- Hits are grouped under the same labeled session headers as grep's:
  `ORDINAL:LINE kind/tool: snippet`. `--no-heading` prints the flat form.
- A quoted phrase must appear as written.
- When no message holds every word, search ranks the messages that hold
  any of them, with common words dropped, and adds a note.
- In the local index, a very common word matches too many messages to
  rank them all. Search then ranks the newest 20,000 matches and says so:
  `ranked newest 20k of N matches — add terms or filters`. The team
  server ranks every match and has no such cap.

## sessions

```sh
flopwire sessions
flopwire sessions 'flopwire*' --since 7d
flopwire sessions --agent codex --repo .
```

A glob matches the session id, the title, the repo or the working
directory. A word without `*` or `?` matches anywhere. `--sort oldest`
lists the least recent activity first.

The answer is compact JSON, one line:

```
{"kind":"sessions","sessions":[{"address":"01a0d550","id":"…","agent":"codex","session_id":"01a0d550-…","title":"…","repo":"/src/pandora","branches":["main"],"last_activity_at":"2026-09-24T18:02:11Z","messages":151,"digest":{"intent":"scope a proper fix for…","commits":["3d446be"],"prs":["o/pandora#110"],"failed":3,"last":"One loose end: …",…}},…],"has_more":true,"next_cursor":"1788220800000000.ID"}
```

- `session_id` is the full harness session id. `flopwire peers
  --session ID`, `flopwire send ID` and `--session` of grep and search
  take it. `address` is its shortest unique prefix.
- Each session carries the fields of `--json` before: `title`, `cwd`,
  `repo`, `device`, `user`, `started_at`, `last_activity_at`,
  `parent_session` (a subagent's parent), `branches`, `messages`, `live`
  and the [digest](#session-digests).
- The list pages by cursor, not offset. `has_more` says whether more
  sessions follow; pass `next_cursor` as `--cursor` (MCP `cursor`). There
  is no total: a page reads about as many sessions as it shows, however
  long the list is.

The cursor is a position in the order, not a snapshot. A session whose
last activity changes during a walk moves: newest first, it is skipped
if it had not been shown yet; oldest first, it can show again on a later
page. Start a new walk to see the list as it is now.

`--text` (MCP `format: "text"`) prints each session's short digest, the
labeled line grep's header uses with `msgs` and `parent`, then its last
reply. Its footer gives the cursor: `[20 sessions shown, more follow;
next: --cursor 1788220800000000.ID]`, or `end of list`.

```
session: 01a0d550 agent: codex ended: 2026-09-24 repo: pandora branch: main msgs: 151 files: 9 pr: #110 commits: 1 failed: 3 intent: "scope a proper fix for…"
    last: "One loose end: local rollout. The daemon still uses 3d446be…"
```

## Session digests

The index computes a digest of every conversation when it indexes it,
locally and on the team server, and refreshes it on every append. It is
deterministic: no model reads the transcript. `sessions` and the grouped
headers show its short form; `read SESSION --outline` shows all of it;
`--json` returns it as `digest`.

| Field | What |
|---|---|
| `intent` | The first user prompt, trimmed to about 120 characters. When that prompt is weak (under 20 characters, a continuation such as "continue", or a paste such as an error dump), the harness title, else the first prompt that is not weak |
| `repos` | The repo root (or working directory) and the git remote, when known |
| `branches` | The git branches the session ran on, first seen first, then the current branch again when the session went back to an earlier one (a→b→a is `[a b a]`). Claude records one per line; Codex one per session |
| `cwd` | The working directory, when it is not the first repo |
| `duration_s` | Last activity minus start, in seconds |
| `messages` | Live messages by kind |
| `subagents` | Subagent sessions it spawned |
| `files_edited` | Files edited by Edit, Write, MultiEdit, NotebookEdit, apply_patch, and the changed paths Codex and Devin record; relative to the repo when under it. At most 25; `files_more` says there were more |
| `commands` | Shell commands run (Bash, exec_command, shell, ...) |
| `tools` | Tool calls by tool name |
| `prs` | PRs as `owner/repo#N`: the URLs `gh pr create`, `edit`, `merge`, `view` and the like print, and PR URLs in prompts and replies. At most 10 |
| `commits` | Hashes `git commit` printed (`[branch abc1234] message`). At most 20 |
| `issues` | Issue URLs as `owner/repo#N`, from prompts, replies and `gh issue` output. At most 10 |
| `more` | The lists above that hit their cap (the lists keep the first ones seen) |
| `failed` | Distinct tool calls the harness marked failed |
| `last` | The last assistant reply, trimmed to about 160 characters |
| `tokens` | Input, output, cache read and cache write tokens, where the harness records usage (Claude; each API message once) |

A PR or commit counts only from the output of the command that makes or
shows it, so a `cat` of an old log does not add one. The stored digest
also holds the fold's bookkeeping (`state`), which output leaves out. A
typical digest is a few hundred bytes; the caps keep a busy session's to
a few KB.

## read

```sh
flopwire read 0b7e2c1a/28672:14
flopwire read 0b7e2c1a/28672 -B 2 -A 2
flopwire read 0b7e2c1a
```

- The header is labeled fields: `# session: FULL ID agent: claude
  repo: /src/api  branch: main  device: mac  user: U  parent: P  start:
  2026-09-23T10:00Z active: 2026-09-23T11:02Z msgs: 42 title: "…"`.
  `cwd` stands in for `repo` outside a repo. Times are UTC.
- The focus message prints with line numbers. The addressed line is
  marked `>`, and the text starts a few lines above it.
- `--max-chars` (default 4000) bounds the focus text. Neighbours get a
  quarter of it. Cut text says which lines it shows and how to read on
  with `--line-offset`.
- `-B/-A/-C N` add neighbouring messages. A session address shows its
  first 20 messages.
- `--outline` prints the session's full digest, then its skeleton: every
  user prompt (trimmed to one line, with its time), and every tool call
  as `tool(args summary)` on an indented line with its address. A failed
  call is marked `error`; a call that spawned a subagent names it
  (`→ sub a85f12`). No tool output prints. `--limit` (default 200, at
  most 2000) and the output budget bound a page. The footer gives the
  `--cursor` that reads the next page. Any address in the session works.
- `--raw` prints the transcript record's bytes. When the local file is
  gone or replaced, the team server's copy is read by this device's path.
  On a terminal, control characters are shown as pictures. Into a pipe or
  a file, or with `--json`, the bytes are exact.

## Output

Text-shaped answers stay text; record-shaped answers are JSON.

- `grep`, `search` and `read` print transcript text and code, so they
  print text by default: JSON would escape every newline and quote.
  `--json` prints the answer as JSON instead.
- `sessions` prints compact JSON by default, as `peers`, `send` and
  `inbox` do. `--text` prints readable rows.
- All seven verbs accept both `--json` and `--text`. Each is a no-op
  where it is the default; `--text` wins when both are given.
- In JSON mode a failure prints one JSON object on stderr and exits 1:
  `{"kind":"error","error":{"code":"bad_request","detail":"…","fix":"…","example":"…"}}`.
  Codes: `bad_request`, `not_found`, `forbidden`, `no_index`,
  `sync_only`, `error`. In text mode it prints one line.

### Labeled fields

Header lines (grep and search session headers, `sessions --text`, `grep
-l`, read's header) are `key: value` fields one space apart. A value is
bare when it is one token. A value with a space, a quote, an apostrophe, a
backslash or a control character, an empty value, and a value ending in `:` print as
a JSON string; `intent` and `title` always do, and come last. Split a
header on the regex `([a-z_]+): ("(?:[^"\\]|\\.)*"|\S+)` and decode
quoted values as JSON. Hit lines keep rg's shape, `ORDINAL:LINE
kind/tool: text`.

## Filters

`grep`, `search` and `sessions` share these filters:

| Flag | Takes |
|---|---|
| `--agent` | `claude`, `codex`, `devin` (comma list) |
| `--repo` | `.`, a repo name, an absolute path, or a glob such as `team*` |
| `--branch` | A git branch the session ran on, or a glob such as `feat/*` |
| `--since`, `--until` | `24h`, `7d`, `2w`, `2026-09-01`, or RFC 3339 |
| `--kind`, `--exclude-kind` | `user`, `assistant`, `tool_call`, `tool_result`, `thinking`, `system`, `agent_message`, `injected` |
| `--tool` | A tool name, such as `Bash` or `exec_command` |
| `--device`, `--user` | A device name, a user email (server) |
| `--session` (grep, search) | One session (a unique id prefix) and its subagents, or `self` |
| `--sort` | `newest`, `oldest`, `relevance` (not for sessions) |
| `--exclude-subagents`, `--exclude-live`, `--include-superseded`, `--include-branches`, `--include-self` | Booleans |

Injected text (CLAUDE.md, AGENTS.md, system reminders) is hidden unless
`--kind` names `injected`.

### Live sessions

A session is live when it wrote in the last 10 minutes, or when its
harness still holds it open and it wrote in the last hour:

- Claude Code: a `sessions/<pid>.json` file of a running process in the
  Claude config dir.
- Devin: a `session_locks/<session>.lock` holding a running pid.
- Codex: a process named codex holds the rollout open.

Headers print `live, 4m ago` for a live session. `--exclude-live` leaves
live sessions out, and the subagents of a session its harness holds open. The device agent reports its live
session ids with each sync flush (at most 32), so the team server knows
them exactly; for a device that has not reported within the hour, the
server uses the 10-minute rule alone.

## Budgets

A grep or search stops after 10 seconds by default. `--timeout` raises
the budget to at most 60 seconds. A query that runs out of budget prints
what it found and one line that says what it checked. It is never an
error. On the team server, `sessions`, `read` and raw reads have the
same 10-second budget; one that runs out fails with 504 and says it
timed out.

A regex with no run of 3 letters or digits that every match must contain
cannot use the trigram index. The local index then checks only the
newest 20,000 messages that pass the filters. The team server refuses
it: add a literal of at least 3 letters or digits, or a filter.

## Your own session

`grep`, `search` and `sessions` leave out the session that calls them,
with its subagents: the call and its output are indexed within a second.
MCP calls always check for a calling session. The CLI checks only when
its standard input is not a terminal, so a person at a terminal sees
everything. A session is left out only when exact evidence names it
(the thread id in a Codex MCP call, `FLOPWIRE_SESSION_ID`, the parent
Claude Code session file, a parent Devin session lock, a parent Codex
process with `CODEX_THREAD_ID` or exactly one rollout open, or
`CLAUDE_CODE_SESSION_ID`). The
output names the session it left out. `--include-self` (MCP:
`include_self`) keeps it. Naming a session with `--session` turns the
exclusion off.

`--session self` (MCP: `session: "self"`) searches only the calling
session and its subagents, found by the same exact evidence;
`flopwire read self --outline` reads its skeleton. When no evidence names
the caller, the call fails and says how to name the session. See
[architecture.md](architecture.md#self-exclusion) for the order of the
checks.

## Register the MCP server

Claude Code:

```sh
claude mcp add flopwire -- flopwire mcp
```

Codex, in `~/.codex/config.toml`:

```toml
[mcp_servers.flopwire]
command = "flopwire"
args = ["mcp"]
default_tools_approval_mode = "approve"
```

The approval line also approves `flopwire_send`, which sends messages (see
[agent.md](agent.md#messaging)). Remove the line to be asked before each
call.

Add `--server` to the arguments to query the team server. The MCP output
is the same as the CLI's: text for `flopwire_grep`, `flopwire_search` and
`flopwire_read` (`format: "json"` for JSON), JSON for `flopwire_sessions`
(`format: "text"` for rows). Every tool also returns its answer as
`structuredContent`, as its `outputSchema` declares, within the same
24,000-byte budget: whole hits, sessions, messages or outline entries,
with `next_offset`, `next_cursor`, `outline_next` or a `hint` that says
where to go on. The server's instructions describe the addresses and the
shared filters once.

The MCP server runs up to 8 tool calls at once. A client's
`notifications/cancelled` stops a call. A request line over 4MB gets a
JSON-RPC error, and the server keeps reading.
