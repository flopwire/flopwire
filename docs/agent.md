# Device agent

The device agent keeps the local index current. It reads Claude Code, Codex
and Devin transcripts. It never writes to the harness directories. When the
device has a server configuration, the agent also uploads the transcripts.

## Run the agent

1. Run `flopwire agent run`.
2. Keep the process running. Use a login item, a launchd agent, or a
   terminal multiplexer.

The first pass indexes every transcript on the device. On the reference
laptop (about 18GB of transcripts) the first pass takes about 7 minutes.
After a first pass of more than 200 sources, the agent restarts itself
once to release the memory of the bulk load. It keeps the index lock
across the restart.

The agent uses these paths:

| Item | Default | Override |
|---|---|---|
| Local index | `<user cache dir>/flopwire/index.db`, plus `index.db-tok`, `index.db-tri0`, `index.db-tri1` and the lock file `index.db.lock` | `--db` or `FLOPWIRE_INDEX` |
| Control socket | `<config dir>/flopwire/agent.sock` | `--socket` |
| Sync spool | `<config dir>/flopwire/spool`, at most 1GiB | `--spool-cap` (bytes) |
| Message inbox | `<config dir>/flopwire/bus.db` | none |
| User path rules | `<config dir>/flopwire/path-rules` | none |
| Admin path rules cache | `<config dir>/flopwire/admin-path-rules.json` | none |
| Claude projects | `~/.claude/projects` | `--claude-projects` or `CLAUDE_CONFIG_DIR` |
| Codex home | `~/.codex` | `--codex-home` or `CODEX_HOME` |
| Devin store | `~/.local/share/devin/cli/sessions.db` | `--devin-db` or `FLOPWIRE_DEVIN_DB`; `-` disables |

On macOS the user cache dir is `~/Library/Caches` and the config dir is
`~/Library/Application Support`.

Use `--once` to index what changed and exit. Use `--no-sync` to index
without upload. `flopwire agent run -h` lists every flag.

Use `--sync-only` (or `"mode": "sync-only"` in the client config) to upload
without a local message index. The index file then keeps only sources,
watermarks, conversations, placements and the sync spool. It has no
message rows and no FTS shard files. Local `grep`, `search`, `read` and
`sessions` fail with `this device is sync-only; use --server`. The flag
overrides the config; `--sync-only=false` forces a full index. The mode
needs a configured server. A change of mode rebuilds the index at the next
start: a switch to full parses every transcript again, and a switch to
sync-only drops the message rows and the shard files.

The agent sets a soft limit of 192MiB on Go memory (`--mem-limit`, or
`GOMEMLIMIT`).

Only one agent writes an index. A second `flopwire agent run` on the same
index exits with `agent already running (pid N)`. `flopwire agent run --once`
asks the running agent for a pass over the control socket and waits for it
to finish. If the index is locked and no agent answers yet, `--once` waits.
The search commands open the index read-only and never take the lock.

## Check the agent

Run `flopwire agent status`. It asks the running agent over the control
socket and prints these parts:

| Line | Means |
|---|---|
| `agent: running` | The agent answered. |
| `path rules removed N sessions from the local index` | A new `deny` rule purged local rows. The first 20 sessions follow. Their server copies stay unless an admin rule covers them; see [path rules](#keep-sessions-out-with-path-rules). |
| `sessions placed by (for path rules):` | How many sessions each placement method placed. See [Where a session ran](#where-a-session-ran). |
| `sync: off (no server configured)` | The device is not enrolled, or the agent runs with `--no-sync`. |
| `sync: ok` | Uploads work. |
| `sync: server unreachable, retry at T: ERR` | The server did not answer. The agent retries by itself, at most 30 seconds apart. |
| `sync: stopped until the server is re-pinned` | The server's certificate does not match the pinned fingerprint. Uploads stop. Run `flopwire login --fingerprint <new>` after you confirm the new fingerprint. |
| `queued: N sources; spool: N bytes` | Sources waiting to upload, and the spool size. `(full: …)` means the spool reached `--spool-cap` and captures of rewritten sources pause. |
| `failing sources (N)` | Each source that fails, its error and its attempts. A failing source retries on its own and does not delay the others. |
| `server refused N sources by admin path rule` | The server did not store these sources. Each shows its path and the rule. The device learns of a refusal on its next upload of that source. |
| `messaging: connected` | The agent holds its poll to the server. See [Messaging](#messaging). |
| `messaging: local` | No server is configured. Messages go between the sessions on this device. |
| `messaging: server unreachable, retry at T: ERR` | The poll failed. The agent retries by itself, at most 30 seconds apart. |
| `messaging: stopped: ERR` | The server refused the credential or the certificate. Run `flopwire login`. Messaging resumes when the agent sees the new credential. |
| `messaging: disabled: ERR` | The server has no message bus, or the credential is not an enrolled device. The agent asks again every 10 minutes. |
| `messages: N pending delivery, N receipts unsent, N held for your acceptance` | Messages in the local inbox that no hook took yet, deliveries the server has not confirmed, and messages from people you have not accepted. |

See [extraction diagnostics](extraction.md) for parser issue counts, source
inspection, and JSON output.

## Connect the harness hooks

A hook tells the running agent to index one transcript and upload it at
once. Without hooks, a new line is still findable in about one second. The
hooks make the upload immediate and cover a session that was idle for more
than two days.

A hook never fails because of the agent. When `flopwire agent flush` reads
hook input, it exits 0 if the agent is not running, is still busy after the
timeout, stops during the call, or does not track the file yet. It prints
the reason on stderr. With `--path` or `--session` it reports these errors,
except a stopped agent.

### Claude Code

1. Open `~/.claude/settings.json`.
2. Add these entries under `hooks`:

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [{ "type": "command", "command": "flopwire agent flush", "timeout": 10 }] }
    ],
    "PostToolUse": [
      { "matcher": "*", "hooks": [{ "type": "command", "command": "flopwire agent flush", "timeout": 10 }] }
    ]
  }
}
```

Claude Code sends the hook input on stdin. The agent reads
`transcript_path` from it.

### Codex

1. Open `~/.codex/config.toml`.
2. Add this line at the top level:

```toml
notify = ["flopwire", "agent", "flush"]
```

Codex passes a JSON argument that names the thread (`thread-id`). The agent
finds the rollout by that session id.

### Manual flush

Run `flopwire agent flush --path <transcript>` or
`flopwire agent flush --session <session id>`.

## Messaging

The agent carries messages between agent sessions
([design](../notes/message-bus/plan.md)). The `flopwire hook` command and
the `peers`, `send` and `inbox` commands use it. They are not built yet.

With a server configuration, the agent does these things:

- It holds one long poll to the server. The poll reports the live sessions
  on this device: session id, harness, repo, branch, and busy or idle.
- It checks the sessions every 2 seconds. When one starts, ends, or turns
  busy or idle, it sends a new poll at once.
- It leaves out each session that a path rule keeps off the server. Such a
  session cannot send or receive messages.
- It keeps the messages for this device in the local inbox. A message that
  the server no longer lists is removed, unless a hook already took it.
- It claims each message to `@you` for one live session on this device.
  It prefers a session on the message's repo, then a busy session.
- It confirms each delivery to the server, in batches.

Without a server configuration, or with `--no-sync`, the agent routes
messages between the sessions on this device. To address yourself, use
`@` and your account name. The server's limits apply: duplicates within 10 minutes,
8 messages per thread per hour, 30 sends per session per hour, and 50
undelivered messages per recipient.

A session is live when `flopwire sessions` shows it as live. The agent
also reads these harness files. It never writes them or locks them:

| Harness | File | Gives |
|---|---|---|
| Claude Code | `~/.claude/sessions/<pid>.json` | Open while the process runs. Busy when `status` is `busy`. |
| Codex | `~/.codex/thread-writer-locks/<thread>.lock` | Open while the file exists. Busy from the last task event in the rollout. |
| Devin | `session_locks/<session>.lock` beside `sessions.db` | Open while the named process runs. Always idle. |

A message waits for 24 hours. Then it expires.

To see the messaging state, run `flopwire agent status`.

## How the agent finds changes

- A full sweep runs every 45 seconds (`--sweep`). It lists the harness
  directories and reads the file identity, size and change time of each
  file. It parses only files whose tuple changed.
- The fast lane runs every 500ms. It reads the tuple of each file that
  changed in the last 10 minutes, and every 5 seconds of each file that
  changed in the last 48 hours. It also checks Devin's `sessions.db` and
  its WAL.
- Directory events (kqueue on macOS, inotify on Linux) report new files in
  active directories. The agent never watches single files.

## Keep sessions out with path rules

A path rule matches where a session ran. It has one of three modes:

| Mode | Local index | Upload |
|---|---|---|
| `allow` (default) | yes | yes |
| `local` | yes | no |
| `deny` | no | no |

To add your own rules:

1. Open `path-rules` in the config directory (beside `config.json`).
2. Write one rule per line, as `MODE PATTERN`. A line with only a pattern
   is a `deny` rule. Lines that start with `#` are comments.
3. Save the file. The agent reads it again at the next sweep.

```
# never index or upload
deny ~/personal
# index here, never upload
local ~/clients/acme/**
# any directory named secret-client, at any depth
deny secret-client
# every repository of the acme organization on GitHub
deny repo:github.com/acme/*
```

- A path pattern covers the directory it names and everything below it.
- `*` and `?` match within one path segment. `**` matches across
  segments. `~` is your home directory.
- A `repo:` pattern matches the repository's `origin` remote, written as
  host/owner/name. `https://github.com/acme/web.git` and
  `git@github.com:acme/web.git` are both `github.com/acme/web`.
- Matching ignores case. A path is also matched with its symlinks
  resolved. For example, on macOS `deny /tmp/x` also covers a session that
  recorded `/private/tmp/x`.
- When rules disagree, the most restrictive rule wins (`deny`, then
  `local`, then `allow`).
- `config.json` can also hold rules, as a `"denylist"` array of strings
  in the same syntax. The agent adds them to the `path-rules` file.
- An admin sets rules for everyone on the server (`path_rules` in
  `PUT /v1/admin/policy` or the console's Collection policy page, written
  `local:PATTERN`, `local:repo:PATTERN` or a bare deny pattern). The
  agent fetches them at start and every 10 minutes, and keeps the last
  copy in `admin-path-rules.json`, so an offline start still applies
  them. Your rules can make an admin rule stricter but not looser.
- The server also applies the admin rules when it parses an upload. It
  does not store a session that an admin `deny` or `local` rule covers.
  The server sees only the directory and remote that the transcript
  recorded, so a rule on a main checkout does not cover a worktree there.
  The agent is still the primary check. `flopwire agent status` lists the
  sources that the server refused.
- The agent reports your home directory to the server with each upload.
  The server uses it for `~` in admin rules.
- When an admin changes the rules, the server hides the stored sessions
  that the new rules cover. Search and read no longer show them. If the
  rules change again within seven days and no longer cover a hidden
  session, the server restores it. After seven days, or when an admin
  confirms the purge (`flopwire admin policy purge --yes`), the server
  deletes them permanently.
- A new `deny` rule removes the matching sessions from the local index.
  Your own rules never reach the server, so copies already uploaded stay
  there unless an admin rule covers them. `flopwire agent status` and
  the agent log name the sessions; ask the owner or an admin to delete
  them there.
- A session can move into another directory after it starts. The agent
  applies the most restrictive rule across every directory the session
  names. When a later directory moves an uploaded session to `local` or
  `deny`, the agent stops the upload and asks the server to delete the
  session and its subagents. The server keeps a tombstone, so the session
  does not come back, and audits the request as `conversation.withheld`
  with the rule. The server records it even before an upload commits. A
  member's enrolled credential with read and upload scopes withholds all
  of that member's device copies. Minted tokens, service credentials, and
  upload-only device credentials withhold only their bound device's copy
  and its subagents. A failed request, including an older server's `404`,
  stays owed and retries at each policy refresh and at start.
- If you remove a `deny` rule, the agent indexes those sessions again.

### Where a session ran

The agent finds these facts when it first sees a session:

- the working directory the transcript names;
- the git worktree root that holds it;
- the main checkout root. For a linked worktree (`git worktree add`), this
  is the checkout the worktree belongs to. For a worktree of a bare
  repository, it is the bare repository's directory;
- the `origin` remote, normalized.

A path pattern is checked against the working directory, the worktree root
and the main checkout root. A `repo:` pattern is checked against the
remote. A rule on `~/Code/app` therefore also covers the worktree
`~/Code/app-fix-login`.

The agent reads the git files (`.git`, `commondir`, `config`) directly and
does not run git. It stores the result in the local index, and keeps it
when the index is rebuilt. Rules still match a session after its worktree
is deleted, and a rule change is checked against the stored facts. When
the rules change, the agent resolves the facts again for each directory
that still exists, so it sees a remote that was added. It only fills in a
main checkout or remote that was unknown. It never replaces a stored one,
because the directory can be deleted and made again as another
repository.

A relative working directory (for example `.`) does not count as a
directory.

### Sessions in deleted worktrees

The agent can first see a session after its worktree was deleted. Then git
cannot give its main checkout. A background pass looks for the checkout
among the repositories the agent already knows (the main checkouts of
other sessions). It records a main checkout only when exactly one
repository fits these signals:

- `remote`: the session recorded a git remote, and exactly one local
  checkout has that `origin`. A rule on `~/Code/app` then covers an old
  Codex session from a deleted `app` worktree.
- `branch`: a branch the session recorded (not `main`, `master` or
  `HEAD`) is a branch, a remote branch or a reflog checkout of the
  repository.
- `worktree-add`: a session in a live checkout ran `git worktree add` for
  the directory. With `git -C DIR`, DIR's repository counts only when the
  agent already knows it.
- `commit`: a commit the session printed, such as `[fix abc1234]`, is in
  the repository.

When more than one repository fits, the agent records no main checkout.
It keeps every repository that fits as a candidate and applies the rules
once with each candidate as the main checkout. The most restrictive result
wins. A transcript can claim anything, so a false claim can only make the
result stricter. Candidates stay across later checks.

The agent does not upload such a session until the pass has checked it.
If a `deny` rule could match, the agent does not index it until then
either. A session in a temporary directory with none of these signals
keeps its working directory only. The agent checks a session that it did
not place again after a day.

`flopwire agent status` shows how many sessions each method placed: `cwd`,
`worktree`, `folder`, `remote`, `branch`, `worktree-add`, `commit` or
`unplaceable`. `ambiguous` counts the sessions that have candidates. Each
of them is also counted under its method.

When the transcript does not name its directory:

- For Claude Code, the project folder name stands for it. For example,
  `~/.claude/projects/-Users-me-Code-app` is `/Users/me/Code/app`. The
  name is lossy, so the agent looks for the longest path that exists.
- For old Codex rollouts, the git remote they recorded places the session.
- The agent waits 2 minutes for a new transcript to name its directory
  before it uses these fallbacks. It does not upload the session while it
  waits. If a `deny` rule could match, it does not index the session
  either.

### Sessions with no place

A session is unplaceable when the agent finds no directory and no remote
for it. The `unplaceable` setting decides what happens to it:

| Value | Local index | Upload |
|---|---|---|
| `local` (default) | yes | no |
| `upload` | yes | yes |
| `exclude` | no | no |

To set it on a device, add `"unplaceable": "upload"` (or another value) to
`config.json`. Restart the agent.

An admin sets a floor in the server policy (`unplaceable` in
`PUT /v1/admin/policy`, or the admin console). The more restrictive of
the two settings applies.
