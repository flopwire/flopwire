# Searching transcripts

Flopwire gives people and agents five retrieval tools over past and ongoing
AI-agent conversations (Claude Code, Codex, Devin, opencode). History can
contain research, planning, writing, troubleshooting and coding. A session
need not be live to search or read it. The CLI and the MCP server have the
same tools, with the same query semantics and output. `flopwire <tool>
--help` lists each tool's flags; this page states what the answers mean.

| Tool | CLI | MCP | Use it for |
|---|---|---|---|
| grep | `flopwire grep` (alias `find`) | `flopwire_grep` | Exact strings and regexes, like rg |
| search | `flopwire search` | `flopwire_search` | Ranked keyword matches, including plain-language queries |
| sessions | `flopwire sessions` | `flopwire_sessions` | Listing or globbing sessions |
| read | `flopwire read` | `flopwire_read` | Reading what an address points to |
| trace | `flopwire trace` | `flopwire_change` | Finding sessions with evidence of a commit |

These tools search the shared server after enrollment and the local index
before enrollment. Use `--local` for this device, or `--server` to select
the server explicitly. `--index PATH` selects a local index unless
`--server` is also set. Text answers show a scope header; JSON answers
include `scope.kind` (`local` or `shared`) and the shared server URL.
Server failures return an error and never switch to local search.

## Resumable hosted grep (opt-in)

Set `FLOPWIRE_GREP_CONTINUATIONS=true` on the server to enable this
experimental mode. It uses the existing Postgres database. The default is
disabled. Local search does not support it.

Start with `flopwire grep 'pattern' --continue --json` (MCP
`continue=true`). Pass `scan.next_cursor` as `--cursor` with the same
pattern, filters and limit. Use absolute `since` and `until` values when
paging; relative times resolve again on each call. The mode returns content
in newest or oldest order, at most 20 hits per call, and rejects offsets,
per-session match caps, session/count output and relevance order.

`scan.status` is `more`, `complete`, `overloaded` or `restart_required`.
`scan.checked` counts candidates verified for this page, not all matching
messages; replays report that original count. An empty `more` page still
advances the search. An empty `complete` page ends traversal; it does not
mean earlier pages had no matches. Output budgets shorten excerpts but
keep every result address.

Each search fixes the committed history visible when its database snapshot
starts, and returns each distinct message address once. Each response
rechecks current visibility, the exact message body and the credential's
authority, so changed, hidden, deleted and redacted messages can disappear.
These checks do not recall content returned before a change.

- Snapshots expire after two minutes. The last four continuation pages can
  be replayed with their incoming cursors; replay rechecks visibility and
  never fills gaps with other hits. Retrying the initial request starts
  fresh.
- A fresh search replaces that user's previous search. Restart, deployment,
  expiry or an evicted replay page needs a fresh search without a cursor.
- Cursors belong to one server process. Device credential rotation keeps
  cursor identity; a different login session or minted token does not.
- The server keeps at most four searches and one per user. Busy searches
  and full admission return `overloaded`; retry the same request.
- Each call verifies at most 256 candidates or 8 MiB of bodies. A body over
  2 MiB or 32,768 distinct matching addresses ends the search explicitly.
- The server reserves four pool connections when the mode is enabled.
  Explicit pool limits must leave room for retrieval, upload and
  maintenance.

Candidate planning and sorting can still use the whole timeout before the
first match. A database timeout invalidates the snapshot and returns
`restart_required`. If grep times out before checking any candidates, use
`search` with a quoted phrase, or `sessions` and then grep within one
session; neither gives complete chronological results. Snapshots retain old
row versions until closed: qualify this mode under representative ingestion
and search load before enabling it broadly.

## Commit evidence

Use `trace` to find the sessions that recorded a commit. Supply a
repository and one or more full commit IDs:

```sh
flopwire trace --repo . 0123456789abcdef0123456789abcdef01234567
```

`flopwire change` is an alias of `flopwire trace`; the MCP tool is
`flopwire_change`, with `repo` and a `commits` array. Both return JSON.
Each match names a session, the observed hash, the matching input IDs, the
operation, and command/result addresses for `read`. A quiet commit or a
delayed result can add reveal addresses. Read those records before
attributing a change. When `command_raw` or `reveal_command_raw` is true,
read that command address with `--raw` (MCP `raw=true`): the command and its
output share one captured completion record, which must still be available
locally or in the server archive.

- Several sessions can match the same input. An abbreviated hash stays a
  prefix match even when it matches only one submitted ID.
- The execution outcome refers to the command as a whole: a later failure
  can follow a successful commit. An unknown operation or directory stays
  unknown.
- Repository filtering uses session placement, so it can miss work done in
  another repository. The caller's own sessions and descendants are
  included.
- Submit at most 100 full SHA-1 or SHA-256 IDs. A page holds 20 evidence
  events by default; pass `next_cursor` as `cursor` to continue. A session
  can appear on several pages. If `restart_required` is true, repeat the
  query without a cursor.
- Printed commit banners are excluded when the same shell command runs
  arbitrary programs (`python prepare.py && gofmt -w main.go && git commit
  -m fix`): the captured output belongs to the whole execution and cannot
  prove which program printed the banner. Run Git in a separate captured
  execution to keep its printed evidence.

`coverage` reports the visible index and indexing lag; it does not establish
complete history. Missing captures, unsupported commands, redaction and
rewritten hashes leave gaps. An empty result means only that no evidence
matched in the searched index. Server lookups refresh a bounded batch of
changed conversations first; pending conversations report lag, and a repeat
lookup continues indexing. `source_text_truncated` reports messages whose
text was capped before indexing; commit output outside the kept text cannot
match. Server backfill bounds each conversation to 100,000 tool records and
64 MiB of tool text and metadata; a larger conversation reports truncated
coverage.

For a squash merge, the calling agent uses its own GitHub access to get the
PR's original commit IDs and submits them together. Flopwire does not call
GitHub or verify that the IDs belong to a PR. PR mentions stay findable
with `grep`.

## Coverage observations

`flopwire agent status` separates collection, uploads, policy holds and
server parsing. Retrieval JSON carries optional observations in
`scope.coverage`; text results carry a short coverage note.

MCP `grep`, `search`, `sessions` and `read` use `coverage="summary"` by
default. The summary reports nonzero evidence gaps, unavailable observations
and the limits of discovery; an absent counter does not mean zero.
`coverage="detail"` returns the complete report, and fails explicitly when
it cannot fit the MCP output budget. For `sessions`, `detail=true` still
selects the full digest independently. Raw reads carry no coverage
metadata.

Observations describe only the current device, while shared history can
include other devices. They do not establish that all intended folders were
discovered or that other devices have caught up. Missing diagnostics are
unknown. An empty upload queue does not prove that uploaded history has
been parsed. Probes run beside retrieval and never delay it, so a busy
agent, a server without the endpoint or a small output budget can omit
detail. See [coverage reporting](coverage-reporting.md).

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
Ordinals are stable ids, not positions.

## grep

```
## 0b7e2c1a-0000-4000-8000-000000000001 agent=claude ended=2026-09-23 repo=alpha branch=main files=2 pr=#43 commits=1 failed=1 intent="why does the login test flake?"
16302080-1- tests/login.test.ts:14: expected 200, received 401
16302080:2 tool_result/Bash error: exit code 1
[3 hits in 3 sessions]
```

Hits print newest first, grouped under one header per session (rg's
`--heading` layout). The header is the session's short
[digest](#session-digests) in the [header line](#header-lines) format: the
full session id, then:

| Key | Value |
|---|---|
| `who` | `user@device`, on the team server |
| `agent` | `claude`, `codex`, `devin` or `opencode` |
| `live` or `ended` | `live=4m` (since its last activity) or `ended=2026-09-23` |
| `repo`, `worktree`, `branch` | The recorded repository name; the checkout directory when it differs; the branch, or `a→b` when it switched. Without recorded repository identity, `repo` is the directory name |
| `files`, `pr`, `prs`, `commits`, `failed` | Files edited, the first PR, how many PRs, commits (with or without a sha), failed tool calls. A `+` means the digest capped the list |
| `intent` | What the session was for, cut to 80 characters at a word. Always last |

- Fields without a value are left out. The header stays within about 260
  bytes: long names are cut, then the intent, then `who`, `repo` and
  `branch` go. A session too new to have a digest gets the id, `who`,
  `agent`, the time, `repo` and `branch` only. JSON keeps the original
  `repo` path, plus optional `repo_identity` (the normalized remote, or the
  main checkout without one) and `worktree` (the checkout root).
- A hit is `ORDINAL:LINE kind/tool: text` on a message's first matching
  line, `ORDINAL:LINE: text` on the others, and `ORDINAL-LINE- text` for
  context. The `read` address is `SESSION/ORDINAL:LINE`. Flags follow the
  kind: `error`, `+N copies`, `superseded`, `off-path`.
- When a session's hits resume after another session's, a short header
  (`## SESSION`) opens them again. A page cut by `--offset` or the output
  budget starts with a full header.
- At most 10 matching lines print per message; a message with more says
  how to read them. Long lines are cut to about 300 bytes around the match.
  Identical texts show once, marked `+N copies`.
- The footer gives the totals and the next page: `[showing 1-20 of 57 hits
  in 9 sessions; next: --offset 20]`. A `+` after a total means counting
  stopped early and the total is a lower bound.
- `-U` lets a match span the lines of one message, never two; the hit
  prints at most 50 lines of the match.
- `--sort relevance` puts the messages with the most matches first, ties
  newest first. It counts every match before it pages, so it reads more of
  the index than `newest`.

## search

- Lexical ranking: BM25 locally and PostgreSQL `ts_rank_cd` on the server.
  Search matches words and phrases; it uses no embeddings and does not
  interpret meaning. Prompts and replies (`user`, `assistant`,
  `agent_message`) rank above tool calls and output that match as well.
  `--sort newest` or `oldest` orders by time instead. Hits use grep's
  session headers.
- A quoted phrase must appear as written.
- `--matching auto` is the default. When no message holds every term, it
  removes common filler words and relaxes terms gradually. Quoted phrases
  and the rarest matching term stay required, and a note names the
  required fallback terms. Rarity probes respect the filters and share the
  ranking deadline. If every matching term reaches the rarity probe cap,
  auto keeps the strict query and says why.
- `--matching all` requires all terms. `--matching any` allows matches on
  any unquoted term. Quoted phrases stay required. Explicit server `OR` and
  negative query syntax keep their meaning.
- In the local index, a very common word matches too many messages to rank
  them all. Search then ranks the newest 20,000 matches and says so:
  `ranked newest 20k of N matches — add terms or filters`. The team server
  ranks every match.

## sessions

A glob matches the session id, the title, the repo or the working
directory; a word without `*` or `?` matches anywhere. The answer is
compact JSON, one brief row per session; `--help` lists its fields.

- `session_id` is the full harness session id, which `peers --session`,
  `send` and `--session` of grep and search take. `address` is its
  shortest unique prefix, left out when it is the whole id.
- `repo` is the root of the checkout the session ran in (for a linked
  worktree, the worktree), or the working directory outside git. `title` is
  cut to 160 bytes. Commit fields come from the digest, including
  `commits_no_sha` (see [commits](#commits)).
- A page that would pass the 24,000-byte MCP budget is cut at a whole row
  and says so. `--detail` prints every field and the whole
  [digest](#session-digests).
- The list pages by cursor: pass `next_cursor` as `--cursor` while
  `has_more` is true. There is no total.

The cursor is a position in the order, not a snapshot. A session whose last
activity changes during a walk moves: newest first, it is skipped if it had
not been shown yet; oldest first, it can show again on a later page. Start
a new walk to see the list as it is now.

## Session digests

The index computes a digest of every conversation, locally and on the team
server, and refreshes it on every append. It is deterministic: no model
reads the transcript. `sessions` and the grouped headers show its short
form; `read SESSION --outline` shows all of it; `--json` returns it as
`digest`.

| Field | What |
|---|---|
| `intent` | The first user prompt, trimmed to about 120 characters. When that prompt is weak (under 20 characters, a continuation such as "continue", or a paste such as an error dump), the harness title, else the first prompt that is not weak |
| `repos`, `cwd` | The repo root (or working directory) and the git remote, when known; the working directory when it is not the first repo |
| `branches` | Branches in the order first seen, repeating the current branch when the session went back (a→b→a is `[a b a]`). Claude records one per line; Codex one per session |
| `duration_s`, `messages`, `subagents` | Last activity minus start; live messages by kind; subagent sessions spawned |
| `files_edited` | Files edited by Edit, Write, MultiEdit, NotebookEdit, apply_patch, and the changed paths Codex and Devin record; relative to the repo when under it. At most 25 |
| `commands`, `tools`, `failed` | Shell commands run; tool calls by name; distinct tool calls the harness marked failed |
| `prs`, `issues` | `owner/repo#N`. PRs come from the output of the command that makes or shows them (`gh pr create`, `view` …) and from URLs in prompts and replies; a `cat` of an old log adds none. At most 10 each |
| `commits` | Commit hashes the session made, as its transcript shows them (see [commits](#commits)). At most 20 |
| `commits_no_sha` | Commits whose hash the transcript never shows: `subject` (from `-m` or a here-document, cut to 80 characters), `branch` (empty when the session was on several) and `at`. At most 5 |
| `more`, `files_more` | The lists that hit their cap (lists keep the first ones seen) |
| `last` | The last assistant reply, trimmed to about 160 characters |
| `tokens` | Input, output, cache read and cache write tokens, where the harness records usage (Claude; each API message once) |

### Commits

A commit counts from shell calls only (Bash, exec_command, Codex exec
scripts, Devin exec); a hash in a prompt or a reply never counts. The
command line is split into its commands (quotes, `$(…)`, here-documents,
`cd`, `git -C`, `env` and `VAR=…` prefixes are understood; nothing is
expanded).

| How the commit shows | Recorded |
|---|---|
| `git commit`, `git cherry-pick`, `git revert` print `[branch abc1234] subject` | The hash, even when a later command of the call failed (git prints the line only once the commit exists) |
| `git commit -q` (or a commit whose output has no such line) that succeeded | In `commits_no_sha`, until a later command shows its hash: then the hash moves to `commits` |
| … then `git rev-parse HEAD` (or `--short HEAD`) | The bare hash it prints |
| … then `git log -1` / `-n 1` (with `--oneline`, `--format=%H…` or `%h…`, `--stat`, `--decorate`), or `git show` of HEAD | The hash of its one entry |
| … then `git log` of HEAD with several entries, alone in its call | The first entry's hash |
| … then `git push` | The new hash of the push line whose local ref is the commit's branch or `HEAD` |
| A commit and its `git log --oneline -1` in one `&&` chain | The hash, directly |

A commit "succeeded" when the harness reports exit code 0 and only `&&`
follows it in its command line. A command that reveals HEAD must end its
call's output, run in the commit's directory, and come before anything else
moves HEAD: a later commit, merge, rebase, reset, checkout, switch, pull,
am, cherry-pick, revert, `gh pr checkout` or `gh pr merge` closes the
window, and so does a `cd` to another directory (Claude's shell keeps it for
the next call).

Not recorded:

- a failed commit (hook rejected, nothing to commit), `git commit --dry-run`;
- a commit followed by `;`, `||` or `|`: its success is unknown, and a
  following log may show the old HEAD;
- hashes from `git log` of other revisions or paths, a `--format` that does
  not start with the hash, a push of another branch or a new branch;
- merges, fast-forwards, `git rebase`, `git am`, `git pull`, `gh pr merge`
  (its PR URL counts as a PR);
- commits made inside a script or alias, from an IDE, or by another tool;
- a commit without a hash that ran outside the session's repo and working
  directory (`git -C /other commit -q`). With a hash, such a commit counts.

The digest never asks the repository. To match a `commits_no_sha` entry,
list the sessions on the branch (`sessions --repo R --branch B`) and compare
the subject and time with `git log`.

## read

- The header is grep's form, with `device`, `user`, `parent`, `start`,
  `active`, `msgs` and `title`; `cwd` stands in for `repo` outside a repo.
- The focus message prints with line numbers. The addressed line is marked
  `>`, and the text starts a few lines above it. Cut text says which lines
  it shows and how to read on with `--line-offset`.
- `--messages-before` and `--messages-after` add whole messages in
  conversation order; a tool call and its result are two messages. `read`
  has no line context: `-A`, `-B` and `-C` are usage errors.
- Under the output budget, `read` keeps the focus and the nearest
  neighbours, and the hint gives the call that reads the rest.
- `--outline` prints the full digest, then the skeleton: every user prompt
  and every tool call as `tool(args summary)` with its address, failed calls
  and spawned subagents marked, no tool output. Any address in the session
  works.
- `--raw` prints the transcript record's bytes. Local reads verify a stored
  head or tail content hash covering the requested range. An unchanged
  file with a stable indexed change time also permits reads outside those
  regions, and an append does not hide indexed records covered by a
  matching stored hash. Other ranges return unavailable until a stable
  index sweep (every 45 seconds by default, `flopwire agent run --sweep` or
  `FLOPWIRE_SWEEP`); a write during that interval can postpone it. Missing
  files and older local generations return an evidence-unavailable error.
  For archived evidence, use `--server` with an address from a server
  query; local and server generation numbers are independent. On a
  terminal, control characters are shown as pictures; into a pipe or file,
  or with `--json`, the bytes are exact. Over MCP, `raw: true` answers the
  record's bytes whatever `format` says.

## Output

- `grep`, `search` and `read` print text by default: JSON would escape
  every newline and quote. `sessions`, `peers`, `send`, `inbox` and
  `messages` print compact JSON by default. All of them accept `--json`
  and `--text`; `--text` wins when both are given. `trace` returns JSON
  only.
- In JSON mode a failure prints one JSON object on stderr and exits 1:
  `{"kind":"error","error":{"code":"bad_request","detail":"…","fix":"…","example":"…"}}`.
  Codes: `bad_request`, `not_found`, `forbidden`, `no_index`, `sync_only`,
  `error`. In text mode it prints one line.

### Header lines

Header lines (grep and search session headers, `sessions --text`, `grep
-l`, read's header) are the session id, then `key=value` fields, one space
apart. A value is bare when it is one token. An empty value, and a value
with a space, a quote, a backslash, `=` or a control character, print as a
JSON string; `intent` and `title` always do, and come last. The session id
follows the same rule. To split a header, take the first token after `## `
or `# ` (`"(?:[^"\\]|\\.)*"|\S+`), then match the rest with
`([a-z_]+)=("(?:[^"\\]|\\.)*"|\S+)`, and decode quoted values as JSON.

## Filters

`grep`, `search` and `sessions` share their filters; `--help` lists them
and their value formats. Times are UTC. `--session` takes one session (a
unique id prefix) and its subagents, or `self`. Injected text (CLAUDE.md,
AGENTS.md, system reminders) is hidden unless `--kind` names `injected`.

### Which repo

`--repo` names a repository, not one checkout of it. A session in a linked
worktree (`git worktree add`) is on the same repo as a session in the main
checkout; so are the worktrees of a bare repository. The `repo` that results
show is the checkout a session ran in, so a worktree's directory name there
is not a repo name for `--repo`: pass the worktree's path, or the repo's
name.

A repository is known by its main checkout (for a bare repository with
worktrees beside it, the bare repository). The comparison resolves
symlinks and, on macOS, the case the volume stores. When the repository has
a remote (`origin`, else the first), the normalized remote
(`github.com/acme/app`) also names it, so a second clone of the same remote
is the same repo.

| `--repo` | Matches |
|---|---|
| `.`, `./x`, `../x`, or an absolute path in a checkout | Every session of the repository that holds the path: the main checkout, every linked worktree, subdirectories, and worktrees deleted since (see [path-rules.md](path-rules.md#sessions-in-deleted-worktrees)) |
| A path outside git | Sessions in that directory or under it |
| A path that no longer exists | The repository the device placed sessions there in, or in a deleted directory above it, when they agree on one; else sessions in that directory or under it. A repository above it that still exists (a home directory kept in git) is not its repository |
| The bare repository's path | Every worktree of it |
| A name (`app`) | The one repository with that name: the last element of its remote or of its main checkout (`.app.git` and `app.git` are `app`). A checkout at `~/Code/app` with the remote `github.com/acme/web` is named both `app` and `web`. Two repositories with the name are an error that lists both. A name that no repository has is an error too. It never matches a directory outside git |
| `owner/name`, `host/owner/name` | The repository whose remote, or main checkout's path, ends with it |
| A glob (`team*`) | A session whose directory, or its last element, matches |

The device resolves `--repo` from its git files and from where it placed its
sessions; it never runs git. A session matches by the main checkout and
remote its placement records, so cost does not grow with the number of
worktrees. A session without a placement matches when its directory is
under the argument's checkout or a main checkout of the repository.

#### With `--server`

Each device uploads, with every transcript, the main checkout and normalized
remote it placed the session in, and sends the new one when the placement
changes. With `--server`, the device resolves `--repo` as above and sends
the repository's remotes, its main checkouts and the argument's checkout,
never a name it resolved. The server matches a main checkout only against
sessions the calling device uploaded. A request carries at most 256 of each
list; a larger list is an error, never a cut. The request leaves out every
checkout a `local` or `deny` path rule covers ([path-rules.md](path-rules.md)),
because the server keeps each query in its audit log; a rule on the main
checkout or the remote leaves only the given path, and no remote.

- A repository with a remote matches every session that any device placed
  in a checkout of that remote, at any path.
- A repository without a remote matches the sessions this device placed in
  it, and by path a session on another device that ran under the
  argument's checkout. Checkouts of it on two devices at different paths
  never merge.
- A name this device does not know (a teammate's repository) is resolved on
  the server over what devices uploaded, with the same rules: one
  repository by that name, else an error that lists them.
- Separate projects cloned from one template that keep its `origin` share a
  remote, so they are one repository. The remote is the only identity that
  crosses devices; remove or rename the template's `origin` to keep them
  apart.

`flopwire peers --repo` matches the same way, against the repo root and
remote each live session reports. An `@user` message is routed by the
sending session's remote when it has one, else by its repo name; `--repo`
on the send names the repository the same way. A session is on a name when
its remote's last element is the name, or, without a remote, its repo
root's last element. A message routed by a remote goes to a session on that
remote; when none is live, to a session without a remote whose repo root
has the remote's name, and then to any session. A name the sending device
does not know is resolved over the recipient's live sessions: one remote
routes by that remote, and two repositories of that name refuse the send
and list them.

### Live sessions

A session is live when it wrote in the last 10 minutes, or when its harness
still holds it open and it wrote in the last hour:

- Claude Code: a `sessions/<pid>.json` file of a running process in the
  Claude config dir.
- Devin: a `session_locks/<session>.lock` holding a running pid.
- Codex: a process named codex holds the rollout open.

Headers print `live=4m` for a live session. `--exclude-live` leaves out
live sessions and the subagents of a session its harness holds open. The
device agent reports up to 32 live session ids with each sync flush, so the
team server knows them exactly; for a device that has not reported within
the hour, the server uses the 10-minute rule alone.

## Budgets

A grep or search stops after 10 seconds by default; `--timeout` raises the
budget to at most 60 seconds. Exit codes:

| Exit | Means |
|---|---|
| 0 | Complete, including no matches |
| 1 | Operational failure |
| 2 | Partial: the time, scan or verification budget ran out. Partial results print; the CLI warns on stderr, and JSON on stdout stays one valid object |

MCP returns partial results successfully, with `truncated` and a reason in
JSON or a warning before the text. A truncated grep scan with no verified
matches sets `isError=true` and keeps its reason and narrowing suggestion.
Counting caps stay successful with their lower-bound warning. Partial
results cannot establish that a message does not exist; keep warnings when
filtering tool output. On the team server, `sessions`, `read` and raw reads
have the same 10-second budget and fail with 504 when it runs out.

A grep with a repo, agent, session, device, user, branch or `--since`
filter resolves those conversations first and scans only their messages, up
to 5000 conversations. A session's last activity is never before its newest
message, so `--since` admits every session with a message in range.

Grep selects index candidates when it can, then verifies the original
regex. Literal alternatives can use the index even when they contain short
words separated by punctuation; they need no literal shared by all
alternatives. When no index strategy applies, the local index scans up to
20,000 messages that pass the filters. The team server scans only when you
name a session with `--session` (with its subagents unless excluded): up to
20,000 messages and 32 MiB of text within the original timeout. Without a
session, it rejects patterns it cannot index and suggests a session-scoped
search. The scan keeps the regex, filters and sort order; it does not escape
punctuation, drop alternatives or retry with a new timeout. For example,
`4040|kickstart|0.31.0` needs a session scan and its dots stay wildcards;
`4040|kickstart|0\.31\.0` can use indexed literal alternatives without one.

A scan that reaches a budget reports incomplete results, even with no
matches. Result offsets page through found hits; they do not resume
scanning unchecked messages. Narrow the scope or time window to finish a
truncated scan.

## Your own session

`grep`, `search` and `sessions` leave out the session that calls them, with
its subagents: the call and its output are indexed within a second. MCP
calls always check for a calling session. The CLI checks only when its
standard input is not a terminal, so a person at a terminal sees
everything. A session is left out only when exact evidence names it; see
[architecture.md](architecture.md#self-exclusion) for the evidence and its
order. The output names the session it left out. `--include-self` (MCP
`include_self`) keeps it. Naming a session with `--session` turns the
exclusion off.

`--session self` (MCP `session: "self"`) searches only the calling session
and its subagents, found by the same evidence; `flopwire read self
--outline` reads its skeleton. When no evidence names the caller, the call
fails and says how to name the session.

## Register the MCP server

`flopwire setup` configures the supported harness integrations, including
tools, hooks and guidance. See [agent discovery](harnesses/README.md#what-agents-discover).
Use the manual configuration below when you need only MCP. Do not add it
alongside the plugin's MCP entry. Manual MCP registration does not install
the skill or message-delivery hooks.

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
[Messaging](messaging.md)). Remove the line to be asked before each call.

After enrollment, the MCP retrieval tools query the shared server; add
`--local` to the arguments to query only this device. MCP output matches
the CLI's: text for `flopwire_grep`, `flopwire_search` and `flopwire_read`
(`format: "json"` for JSON), JSON for `flopwire_sessions` (`format: "text"`
for rows). Every answer is one text block within the 24,000-byte budget,
holding whole hits, sessions, messages or outline entries; the footer (or
`next_offset`, `next_cursor`, `outline_next` and `hint` in JSON) says where
to go on. JSON is encoded inside the text block; the server returns no
`structuredContent` and declares no `outputSchema`. The server's
instructions cover scope, evidence handling and coordination; tool and
parameter descriptions define argument formats and defaults.

The MCP server runs up to 8 tool calls at once. A client's
`notifications/cancelled` stops a call. A request line over 4MB gets a
JSON-RPC error, and the server keeps reading.
