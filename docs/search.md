# Searching transcripts

Flopwire gives people and agents five tools over coding-agent transcripts
(Claude Code, Codex, Devin, opencode). The CLI and the MCP server have the same
tools, with the same query semantics and output. `grep`, `search` and
`read` print text by default (`--json` for JSON). `sessions` prints JSON
by default (`--text` for readable rows). See [Output](#output).

| Tool | CLI | MCP | Use it for |
|---|---|---|---|
| grep | `flopwire grep` (alias `find`) | `flopwire_grep` | Exact strings and regexes, like rg |
| search | `flopwire search` | `flopwire_search` | Ranked keyword matches, including plain-language queries |
| sessions | `flopwire sessions` | `flopwire_sessions` | Listing or globbing sessions |
| read | `flopwire read` | `flopwire_read` | Reading what an address points to |
| change | `flopwire change` | `flopwire_change` | Finding sessions with evidence of a commit |

These tools search the shared server after enrollment and the local index
before enrollment. Use `--local` for this device, or `--server` to select
the server explicitly. `--index PATH` selects a local index unless
`--server` is also set. Text answers show a scope header; JSON answers
include `scope.kind` (`local` or `shared`) and the shared server URL.
Server failures return an error and never switch to local search. `flopwire <tool> --help` shows its flags.

## Resumable hosted grep (opt-in)

Set `FLOPWIRE_GREP_CONTINUATIONS=true` on the server to enable this experimental
mode. It uses the existing Postgres database. The default is disabled.

Start with `flopwire grep 'pattern' --continue --json`. Pass `scan.next_cursor`
as `--cursor` with the same pattern, filters, and limit. MCP uses `continue=true`
and `cursor`. Use an absolute `since` or `until` value when paging; relative
times resolve again on each call. This mode supports content in newest or oldest
order, with at most 20 hits per call. It rejects offsets, per-session match caps,
session/count output, and relevance order. Local search does not support it.

`scan.status` is `more`, `complete`, `overloaded`, or `restart_required`.
`scan.checked` counts candidates verified in this call, not all matching messages.
An empty `more` page still advances the search. An empty `complete` page ends
traversal; it does not mean earlier pages had no matches. Output budgets shorten
excerpts but preserve every result address. Use `read` for omitted context.

Each search fixes the committed history visible when its database snapshot starts.
It returns each distinct message address once, including identical text at different
addresses. Each response rechecks current visibility and the exact message body.
Changed, hidden, deleted, and redacted messages can disappear. Credential authority
is checked again before the response. These checks do not retroactively recall
content returned before a change.

Snapshots expire after two minutes. The last four pages can be replayed with their
incoming cursors; replay rechecks visibility and never fills gaps with other hits.
A fresh search replaces that user's previous search. Restart, deployment, expiry,
or an evicted replay page requires a fresh search without a cursor. Cursors belong
to one server process; requests must reach that process. Device credential rotation
preserves cursor identity; different login sessions or minted tokens do not.

The server retains at most four searches and one per user. Busy searches and full
admission return `overloaded`; retry the same request. Each call verifies at most
256 candidates or 8 MiB of bodies. A body over 2 MiB or 32,768 distinct matching
addresses ends the search explicitly; narrow the query. The server reserves four
pool connections when enabled. Explicit pool limits must leave the existing
retrieval, upload, and maintenance capacity available.

Postgres candidate planning and sorting can still consume the request timeout
before the first match. A database timeout invalidates the snapshot and returns
`restart_required`. This mode is not a claim of capacity for 30 simultaneous users.
Short-lived snapshots still retain old database row versions until closed; qualify
this mode with representative ingestion and search load before enabling it broadly.

## Commit evidence

Use `change` to find the sessions that recorded a commit. Supply a repository
and one or more full commit IDs:

```sh
flopwire change --repo . 0123456789abcdef0123456789abcdef01234567
```

The MCP call uses `flopwire_change` with `repo` and a `commits` array.
Both interfaces return JSON. Each match identifies a session, the observed
hash, matching input IDs, the operation, and command/result addresses for
`read`. A quiet commit or delayed result can include additional reveal
addresses. Read those records before attributing a change.
When `command_raw` or `reveal_command_raw` is true, read that command address
with `raw=true` (CLI: `--raw`). The command and output share one captured
completion record. The raw record must still be available locally or in the
server archive.

Several sessions can match the same input. An abbreviated hash stays a prefix
match even when it matches only one submitted ID. The execution outcome refers
to the command as a whole: a later failure can follow a successful commit.
An unknown operation or directory remains unknown. Repository filtering uses
session placement, so it can miss work performed in another repository.
The caller's own sessions and descendants are included.

Submit at most 100 full SHA-1 or SHA-256 IDs. The default page contains 20
evidence events. Pass `next_cursor` as `cursor` to continue the same query.
A session can appear on several pages. New appends appear in a new lookup.
If `restart_required` is true, repeat the query without a cursor.

`coverage` reports the visible index and indexing lag. It does not establish
complete history. Missing captures, unsupported commands, redaction, and
rewritten hashes can leave gaps. An empty result means no evidence matched
in the searched index.
Printed commit banners are excluded when the same shell command runs arbitrary
programs, such as `python prepare.py && gofmt -w main.go && git commit -m fix`.
The captured output belongs to the whole execution. It cannot prove which
program printed a banner. This can exclude a real commit even when the command
succeeds. Run Git in a separate captured execution to retain its printed
evidence. A separate Git completion remains eligible even when a wrapper also
runs preparation commands.
Server lookups refresh a bounded batch of changed conversations before
searching. Pending conversations report indexing lag; repeat the lookup to
continue indexing.
`source_text_truncated` reports captured messages whose text was capped before
indexing. Commit output outside the retained text cannot match. Server backfill
also bounds each conversation to 100,000 tool records and 64 MiB of retained
tool text and metadata; a larger conversation reports truncated coverage.

For a squash merge, the calling agent can use its own GitHub access to obtain
the PR's original commit IDs and submit those IDs together. Flopwire does not
invoke GitHub or verify that the supplied IDs belong to a PR. PR mentions
remain available through `grep`.

## Coverage observations

`flopwire agent status` separates collection, uploads, policy holds, and server
parsing. Retrieval JSON includes optional observations in `scope.coverage`.
Text results include a short coverage note.

These observations describe the current device. They do not establish that
all intended folders were discovered or that other devices have caught up.
Missing diagnostics are unknown. An empty upload queue does not prove that
uploaded history has been parsed for search.

Coverage probes run beside retrieval and do not delay its results. A busy
agent, older server, or small output budget can omit detailed observations.
Collection rules and exclusions still apply. See
[coverage reporting](coverage-reporting.md) for scope and limitations.

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
## 0b7e2c1a-0000-4000-8000-000000000001 agent=claude ended=2026-09-23 repo=alpha branch=main files=2 pr=#43 commits=1 failed=1 intent="why does the login test flake?"
16302080-1- tests/login.test.ts:14: expected 200, received 401
16302080:2 tool_result/Bash error: exit code 1
[3 hits in 3 sessions]
```

- The header is the session's short digest (see [Session digests](#session-digests))
  (see [Header lines](#header-lines)): the full session id (the
  SESSION of a hit's address), then:

  | Key | Value |
  |---|---|
  | `who` | `user@device`, on the team server |
  | `agent` | `claude`, `codex`, `devin` or `opencode` |
  | `live` or `ended` | `live=4m` (since its last activity) or `ended=2026-09-23` |
  | `repo`, `worktree`, `branch` | The recorded repository name; the checkout directory when it differs; the branch, or `a→b` when it switched. Without recorded repository identity, `repo` keeps the legacy directory name |
  | `files`, `pr`, `prs`, `commits`, `failed` | Files edited, the first PR, how many PRs, commits (with or without a sha), failed tool calls. A `+` means the digest capped the list |
  | `intent` | What the session was for, cut to 80 characters at a word. Always last |

  Fields without a value are left out. The header stays within about 260
  bytes: long names are cut, then the intent, then `who`, `repo` and
  `branch` go. A session indexed so recently that it has no digest yet
  gets the session id, `who`, `agent`, the time, `repo` and `branch` only.
  JSON keeps the original `repo` path. Optional `repo_identity` records the
  normalized remote, or the main checkout for a repository without a remote.
  Optional `worktree` records the checkout root. For example, an Eichler
  checkout named `ike-ux-audit` prints `repo=eichler worktree=ike-ux-audit`.
- Under the header, a matching line is `ORDINAL:LINE kind/tool: text` on
  a message's first matching line and `ORDINAL:LINE: text` on the others.
  A context line is `ORDINAL-LINE- text`. The address for `read` is
  `SESSION/ORDINAL:LINE`, with SESSION from the header. Flags follow the
  kind: `error`, `+N copies`, `superseded`, `off-path`.
- When a session's hits resume after another session's, a short header
  (`## SESSION`) opens them again. A page cut by `--offset` or the output
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

- Lexical ranking: BM25 locally and PostgreSQL `ts_rank_cd` on the server.
  Search matches words and phrases. It does not use embeddings or interpret
  a question's meaning. Prompts and replies (user, assistant, agent_message) rank
  above tool calls and tool output that match as well. `--sort newest` or
  `oldest` orders the matching messages by time instead.
- Hits are grouped under the same labeled session headers as grep's:
  `ORDINAL:LINE kind/tool: snippet`. `--no-heading` prints the flat form.
- A quoted phrase must appear as written.
- `--matching auto` is the default. When no message holds every term, it
  removes common filler words and relaxes terms gradually. Quoted phrases
  and the rarest matching term remain required. A note names the required
  fallback terms. Rarity probes respect the filters and share the ranking
  deadline. Each later probe stops once it cannot identify a rarer term.
- `--matching all` requires all terms. `--matching any` explicitly allows
  broad matches on any unquoted term. Quoted phrases remain required.
  MCP uses `matching: "auto"`, `"all"`, or `"any"`.
- If every matching term reaches the rarity probe cap, automatic relaxation
  keeps the strict query and says why. Use quoted topic terms or narrow the
  filters. Explicit server `OR` and negative query syntax retain their meaning.
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

The answer is compact JSON, one line, one brief row per session:

```
{"kind":"sessions","sessions":[{"session_id":"01a0d550-…","address":"01a0d550","agent":"codex","user":"gary@example.test","repo":"/src/pandora","branches":["main"],"live":false,"last_activity_at":"2026-09-24T18:02:11Z","messages":151,"title":"…","intent":"scope a proper fix for…","commits":["3d446be"],"files":9,"failed":3},…],"has_more":true,"next_cursor":"1788220800000000.ID"}
```

- `session_id` is the full harness session id. `flopwire peers
  --session ID`, `flopwire send ID` and `--session` of grep and search
  take it. `address` is its shortest unique prefix, left out when it is
  the whole id.
- A row has `agent`, `user` and `device` (when known), `repo` (the
  root of the checkout the session ran in, which for a linked worktree is
  the worktree, or the working directory outside git), `branches`, `live`, `last_activity_at`, `messages`,
  `title` (cut to 160 bytes), `intent` (when it is not the title),
  `parent_session` (a subagent's parent), and from the digest the commit
  ids, the commits recorded without a sha (`commits_no_sha`, see
  [commits](#commits)), and the counts of files edited and failed tool
  calls. A row is about 270 bytes on the test fixtures and at most about
  1,500 (20 commit ids, 5 commits without a sha, a long title and
  intent), so the default page of 20 is about 5–30 KB; a page that would
  pass the 24,000-byte MCP budget is cut at a whole row and says so.
- `--detail` (MCP `detail: true`) prints every field instead, with the
  whole [digest](#session-digests): the files edited, PRs, tools,
  tokens and last reply.
- The list pages by cursor, not offset. `has_more` says whether more
  sessions follow; pass `next_cursor` as `--cursor` (MCP `cursor`). There
  is no total: a page reads about as many sessions as it shows, however
  long the list is.

The cursor is a position in the order, not a snapshot. A session whose
last activity changes during a walk moves: newest first, it is skipped
if it had not been shown yet; oldest first, it can show again on a later
page. Start a new walk to see the list as it is now.

`--text` (MCP `format: "text"`) prints each session's short digest, the
line grep's header uses with `msgs` and `parent`, then its last reply. Its footer gives the cursor: `[20 sessions shown, more follow;
next: --cursor 1788220800000000.ID]`, or `end of list`.

```
01a0d550-6c1e-7a52-9d0e-3f4b2a1c9e77 agent=codex ended=2026-09-24 repo=pandora branch=main msgs=151 files=9 pr=#110 commits=1 failed=3 intent="scope a proper fix for…"
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
| `commits` | Commit hashes the session made, as its transcript shows them (see [commits](#commits)). At most 20 |
| `commits_no_sha` | Commits the session made whose hash its transcript never shows (`git commit -q`): `subject` (from `-m` or a here-document, cut to 80 characters; empty when the line does not give it), `branch` (the session's branch; empty when the session was on several, since the transcript does not say which one the commit was on) and `at` (when the call returned). At most 5 |
| `issues` | Issue URLs as `owner/repo#N`, from prompts, replies and `gh issue` output. At most 10 |
| `more` | The lists above that hit their cap (the lists keep the first ones seen) |
| `failed` | Distinct tool calls the harness marked failed |
| `last` | The last assistant reply, trimmed to about 160 characters |
| `tokens` | Input, output, cache read and cache write tokens, where the harness records usage (Claude; each API message once) |

A PR counts only from the output of the command that makes or shows it,
so a `cat` of an old log does not add one. The stored digest
also holds the fold's bookkeeping (`state`), which output leaves out. A
typical digest is a few hundred bytes; the caps keep a busy session's to
a few KB.

### Commits

A commit counts from shell calls only (Bash, exec_command, Codex exec
scripts, Devin exec); a hash in a prompt or a reply never counts. The
command line is split into its commands (quotes, `$(…)`, here-documents,
`cd`, `git -C`, `env` and `VAR=…` prefixes are understood; nothing is
expanded).

| How the commit shows | Recorded |
|---|---|
| `git commit`, `git cherry-pick`, `git revert` print `[branch abc1234] subject` | The hash, though a later command of the call failed (git prints the line only once the commit exists) |
| `git commit -q` (or a commit whose output has no such line, such as hooks only) that succeeded | In `commits_no_sha`, until a later command shows its hash: then the hash moves to `commits` |
| … then `git rev-parse HEAD` (or `--short HEAD`) | The bare hash it prints |
| … then `git log -1` / `-n 1` (any of `--oneline`, `--format=%H…` or `%h…`, `--stat`, `--decorate`), or `git show` of HEAD | The hash of its one entry |
| … then `git log` of HEAD with several entries, alone in its call | The first entry's hash |
| … then `git push` | The new hash of the push line whose local ref is the commit's branch or `HEAD` (`abc..def  b -> b`) |
| A commit and its `git log --oneline -1` in one `&&` chain | The hash, directly |

A commit "succeeded" when the harness reports no error (exit code 0)
and only `&&` follows the commit in its command line. A command that
reveals HEAD must end its call's output, run in the same directory as
the commit, and come before anything else moves HEAD: a later commit,
merge, rebase, reset, checkout, switch, pull, am, cherry-pick, revert,
`gh pr checkout` or `gh pr merge` closes the window, and so does a `cd`
that leaves the shell in another directory (Claude's shell keeps it for
the next call); the commit then stays without a hash.

Not recorded:

- a failed commit (hook rejected, nothing to commit), `git commit
  --dry-run`;
- a commit followed by `;`, `||` or `|` (`git commit -q -m x; git log
  -1`): its success is unknown, and a following log may show the old HEAD;
- hashes from `git log` of other revisions or paths, `git log -1 -- path`,
  a `--format` that does not start with the hash, a push of another
  branch or of a new branch (`* [new branch]` prints no hash);
- merges (`Merge made by …` prints no hash), fast-forwards (no new
  commit), `git rebase`, `git am`, `git pull`;
- `gh pr merge` (the merge commit is made on GitHub; a PR URL it prints
  counts as a PR);
- commits made inside a script or alias (`./release.sh`), from an IDE, or
  by another tool: the transcript shows no git command;
- a commit without a hash that ran outside the session's repo and
  working directory (`git -C /other commit -q`): its branch would be
  the session's. With a hash, such a commit (a sibling worktree, say)
  counts.

The digest never asks the repository: a commit whose hash the transcript
never shows stays in `commits_no_sha`. To match one, list the sessions on
the branch (`sessions --repo R --branch B`) and compare the subject and
time with `git log`.

## read

```sh
flopwire read 0b7e2c1a/28672:14
flopwire read 0b7e2c1a/28672 --messages-before 2 --messages-after 2
flopwire read 0b7e2c1a
```

- The header is grep's form: `# FULL_ID agent=claude repo=/src/api
  branch=main device=mac user=U parent=P start=2026-09-23T10:00Z
  active=2026-09-23T11:02Z msgs=42 title="…"`.
  `cwd` stands in for `repo` outside a repo. Times are UTC.
- The focus message prints with line numbers. The addressed line is
  marked `>`, and the text starts a few lines above it.
- `--max-chars` (default 4000) bounds the focus text. Neighbours get a
  quarter of it. Cut text says which lines it shows and how to read on
  with `--line-offset`.
- `--messages-before N` and `--messages-after N` (MCP `messages_before`
  and `messages_after`) add whole messages before and after the focus, in
  conversation order. A tool call and its result are two messages. A
  session address shows its first 20 messages. `read` has no line
  context: `-A`, `-B`, `-C` and their long forms are usage errors that
  name these flags. Use `--line-offset` for lines inside the focus.
- Under the output budget (MCP, or `--max-bytes`), `read` keeps the focus
  and the nearest neighbours. When it leaves out neighbours it fetched,
  the hint says so and gives the call that reads the rest:
  `[later messages: showing 2 of 150 messages after; output budget of
  24000 bytes reached; next: flopwire_read address=X messages_after=148]`.
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
  a file, or with `--json`, the bytes are exact. Over MCP, `raw: true`
  answers the record's bytes whatever `format` says (the record is
  itself JSON).

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

### Header lines

Header lines (grep and search session headers, `sessions --text`, `grep
-l`, read's header) are the session id, then `key=value` fields, one
space apart. A value is bare when it is one token. An empty value, and a
value with a space, a quote, a backslash, `=` or a control character,
print as a JSON string; `intent` and `title` always do, and come last.
The session id follows the same rule. To split a header, take the first
token after `## ` or `# ` (`"(?:[^"\\]|\\.)*"|\S+`), then match the rest
with `([a-z_]+)=("(?:[^"\\]|\\.)*"|\S+)`, and decode quoted values as
JSON. Hit lines keep rg's shape, `ORDINAL:LINE
kind/tool: text`.

## Filters

`grep`, `search` and `sessions` share these filters:

| Flag | Takes |
|---|---|
| `--agent` | `claude`, `codex`, `devin`, `opencode` (comma list) |
| `--repo` | A path (`.`, `./x`, `../x`, an absolute path), a repo name, `owner/name`, `host/owner/name`, or a glob such as `team*`. See [Which repo](#which-repo) |
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

### Which repo

`--repo` names a repository, not one checkout of it. A session in a
linked worktree (`git worktree add`) is on the same repo as a session in
the main checkout. This is also true of the worktrees of a bare
repository, with or without a remote. The `repo` that results show is
the checkout a session ran in, so a worktree's directory name there is
not a repo name for `--repo`: pass the worktree's path, or the repo's
name.

A repository is known by its main checkout. For a bare repository with
worktrees beside it (`~/Code/.app.git` and `~/Code/app`), it is the bare
repository. The comparison resolves symlinks, so `/tmp/x` and
`/private/tmp/x` are one path. On macOS it also uses the case that the
volume stores, so `/p/App` and `/p/app` are one checkout on a
case-insensitive volume. When the repository has a remote (`origin`,
else the first), the normalized remote (`github.com/acme/app`) also names
it. A second clone of the same remote is then the same repo.

| `--repo` | Matches |
|---|---|
| `.`, `./x`, `../x`, or an absolute path in a checkout | Every session of the repository that holds the path: the main checkout, every linked worktree, subdirectories, and worktrees deleted since (see [agent.md](agent.md#sessions-in-deleted-worktrees)) |
| A path outside git | Sessions in that directory or under it |
| A path that no longer exists | The repository the device placed sessions there in, or in a deleted directory above it, when they agree on one; else sessions in that directory or under it. A repository above it that still exists (a home directory kept in git) is not its repository |
| The bare repository's path | Every worktree of it |
| A name (`app`) | The one repository with that name: the last element of its remote or of its main checkout (`.app.git` and `app.git` are `app`). A checkout at `~/Code/app` with the remote `github.com/acme/web` is named both `app` and `web`. Two repositories with the name are an error that lists both; pass a path or `owner/name` instead. A name that no repository has is an error too. It never matches the last element of a directory outside git; pass the path |
| `owner/name`, `host/owner/name` | The repository whose remote, or main checkout's path, ends with it |
| A glob (`team*`) | A session whose directory, or its last element, matches |

The device resolves `--repo` from its git files and from where it placed
its sessions. It never runs git. A session matches by the main checkout
and remote that its placement records, so the query does not list the
repository's worktrees. Its cost does not grow with their number. A
session that has no placement matches when its directory is under the
argument's checkout or a main checkout of the repository.

#### With `--server`

Each device uploads, with every transcript, the repository it placed the
session in: the main checkout and the normalized remote. When the
placement changes later (the agent finds the checkout of a deleted
worktree), the device sends the new one.

With `--server`, the device resolves `--repo` as above. It sends the
repository's remotes, its main checkouts, and the argument's checkout. It
never sends a name that it resolved. The server cannot read this device's
git files. The server matches a main checkout only against sessions that
the calling device uploaded. A request can carry at most 256 of each
list. A larger list is an error, never a cut. The request leaves out
every checkout that a `local` or `deny` path rule covers (see
[agent.md](agent.md#keep-sessions-out-with-path-rules)), because the
server keeps each query in its audit log. A rule on the main checkout or
the remote covers every checkout, so then only the path that you gave is
sent, and no remote.

- A repository with a remote matches every session that any device
  placed in a checkout of that remote, at any path.
- A repository without a remote matches the sessions that this device
  placed in it, and by path a session on another device that ran under
  the argument's checkout. Two devices' checkouts of it at different paths are not told
  apart from two repositories with the same name, so they never merge.
- A name that this device does not know (a teammate's repository) is
  resolved on the server, over what the devices uploaded, with the same
  rules: one repository by that name, else an error that lists them
  (each remote, or `device:checkout` for one without a remote).
- Separate projects cloned from one starter template that keep its
  `origin` share a remote, so they are one repository. This is by design:
  the remote is the only identity that crosses devices. Remove or rename
  the template's `origin` to keep them apart.

`flopwire peers --repo` matches the same way, against the repo root and
remote that each live session reports. An `@user` message is routed by
the sending session's remote when it has one, else by its repo name.
`--repo` on the send names the repository the same way: the device sends
its remote when it knows one. A session is on a name when its remote's
last element is the name, or, without a remote, its repo root's last
element. A message routed by a remote goes to a session on that remote.
When none is live, it goes to a session without a remote whose repo root
has the remote's name, and then to any session. A name that the sending
device does not know is resolved over the recipient's live sessions: one
remote routes the message by that remote, and two repositories of that
name refuse the send and list them.

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
its partial results. The CLI warns on stderr and exits 2. JSON on stdout
remains one valid result object. A scan or verification budget also exits 2.
Complete searches exit 0, including searches with no matches. Operational
failures exit 1. MCP returns partial results successfully, with `truncated`
and a reason in JSON or a warning before the text results. A truncated grep
scan with no verified matches sets MCP `isError=true` and keeps its rendered
reason and narrowing suggestion, including when the query times out. Complete
empty answers and useful partial answers remain successful MCP calls. Counting
caps also remain successful and keep their lower-bound warning. Partial results
cannot establish that a message does not exist. Keep warnings when filtering
tool output. On the team server, `sessions`, `read` and raw reads have the
same 10-second budget; one that runs out fails with 504 and says it
timed out. A grep with a repo, agent, session, device, user, branch or
`--since` filter resolves those conversations first and scans only their
messages, up to 5000 conversations. A session's last activity is never
before its newest message, so `--since` admits every session with a
message in range, whatever its agent reports.

Grep selects index candidates when it can, then verifies the original regex.
Literal alternatives can use the index even when they contain short words
separated by punctuation. A query does not need one literal shared by all
alternatives.

When no text index strategy applies, the local index scans up to 20,000
messages that pass the filters. The team server also supports a bounded scan
when you name a session with `--session` (MCP: `session`). That scope includes
the session's subagents unless excluded. The server checks up to 20,000
messages and 32 MiB of message text within the original timeout. The scan
preserves the regex, filters, and requested sort order. It does not escape
punctuation, drop alternatives, or retry with a new timeout.

For example, `4040|kickstart|0.31.0` can use a bounded session scan. Its dots
remain regex wildcards. Use `4040|kickstart|0\.31\.0` if you intend literal
dots; that pattern can use indexed literal alternatives without session scope.

A scan that reaches a budget reports incomplete results, even if it found no
matches. Result offsets page through found hits; they do not resume scanning
unchecked messages. Narrow the scope or time window to finish a truncated
scan. Without an explicit session, the server rejects patterns it cannot
index and suggests a session-scoped search.

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

After enrollment, the MCP tools query the shared server by default. Add
`--local` to the arguments to query only this device. The MCP output
is the same as the CLI's: text for `flopwire_grep`, `flopwire_search` and
`flopwire_read` (`format: "json"` for JSON), JSON for `flopwire_sessions`
(`format: "text"` for rows). Every answer is one text block within the
24,000-byte budget: whole hits, sessions, messages or outline entries,
and the footer (or `next_offset`, `next_cursor`, `outline_next` and
`hint` in JSON) says where to go on. No tool returns `structuredContent`:
Claude Code shows a model only that part of a result, and Codex shows
both parts, so either would replace or double the answer. The server's instructions describe the addresses and the
shared filters once.

The MCP server runs up to 8 tool calls at once. A client's
`notifications/cancelled` stops a call. A request line over 4MB gets a
JSON-RPC error, and the server keeps reading.
