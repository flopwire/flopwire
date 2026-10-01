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
| Local redactions | `index.db.redactions.jsonl` beside the index, `index.db.redactions.jsonl.prev` (its copy before the last redaction) and the key `index.db.redactions.key`; see [recover the local index](#recover-the-local-index) | follows the index |
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

## Local redactions: what they guarantee

`flopwire redact` hides a message, or some of its lines, in the local
index of this device. After the command succeeds, the hidden text does not
come back through `flopwire` search, grep or read, through the MCP
server, or in a conversation title or digest. This stays true after a
re-index, a rebuild of the index, a grown or rewritten message, and a
crash of the agent.

Local redaction does not protect the files on disk:

- The harness transcripts (`~/.claude`, `~/.codex`, the Devin store) are
  not changed. They still hold the text.
- The index database keeps `content_sha`, a SHA-256 of each message's
  original text, for change detection. A person who can read the index
  files can test guesses of a hidden message against it.
- Free pages of the index database can hold old text until SQLite reuses
  them. After each redaction the agent compacts the search files and
  overwrites their free pages.

To remove the text from the disk, delete it from the transcript as well.

## Recover the local index

The local index is derived data. The agent can build it again from the
transcripts. The redaction file and its key are not derived data. Together
they are the only record of the messages that you hid with
`flopwire redact`.

The redaction file holds no message text. It holds these items:

- Keyed hashes (HMAC-SHA256) of each redacted message, of each hidden line,
  and of the title at the length where the parsers cut titles.
- The session id and message id of each redacted message, and the line
  range.
- The byte length of each hidden line.
- The masked title, which shows only title text that you did not hide.

The key is in a separate file. Without the key, the hashes do not let a
reader test guesses of the hidden text. Keep the key file as private as
the transcripts.

The files are in the index directory. On macOS that is
`~/Library/Caches/flopwire/`. With `--db` or `FLOPWIRE_INDEX`, it is the
directory of that path.

| File | Contents | Action |
|---|---|---|
| `index.db`, `index.db-wal`, `index.db-shm` | Index rows, sync state, placements | Do not delete. Use `--rebuild-index`. |
| `index.db-tok`, `index.db-tri0`, `index.db-tri1` | Search files | The agent rebuilds them. |
| `index.db.redactions.jsonl` | Your local redactions, one per line | Never delete. |
| `index.db.redactions.jsonl.prev` | The redaction file before the last redaction | Restore from it. |
| `index.db.redactions.key` | The key of the hashes in both redaction files | Never delete. Back it up with the redaction file. |

If you delete the redaction file or the key, the next rebuild shows the
text that you redacted.

### Stop the agent

Stop the agent before each procedure below.

- With the launchd agent from `deploy/launchd`, run
  `launchctl bootout gui/$(id -u)/com.flopwire.agent`.
- With a systemd user unit, run `systemctl --user stop <unit>`.
- Otherwise, stop the process that runs `flopwire agent run`.

To start the launchd agent again, run
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.flopwire.agent.plist`.

### The agent stops: "index.db.redactions.jsonl is corrupt"

The agent does not open an index while a redaction is unreadable.

1. Stop the agent.
2. Copy the damaged file to a safe place:
   `cp index.db.redactions.jsonl ~/flopwire-redactions-damaged.jsonl`.
3. If `index.db.redactions.jsonl.prev` exists, copy it over the damaged
   file: `cp index.db.redactions.jsonl.prev index.db.redactions.jsonl`.
4. If no `.prev` file exists, open the file in a text editor. Remove only
   the line that the error names. Save the file.
5. Start the agent.
6. Run again each `flopwire redact` command that you ran after the copy
   that you restored. The file cannot show which message a removed line
   hid. If you are not sure, run all of them again. A repeated redaction
   of hidden text reports `nothing to redact`.

### The agent stops: "its key ... is missing", "the key does not match", or "redactions.key is corrupt"

The agent cannot read the redactions without the key.

1. Stop the agent.
2. Restore `index.db.redactions.key` from the backup that you made with
   this redaction file, if you have one. A key from another device or
   another backup does not match. Then start the agent. Stop here.
3. Without a backup, move both redaction files to a safe place:
   `mv index.db.redactions.jsonl index.db.redactions.jsonl.prev ~/`.
4. Run `flopwire agent run --rebuild-index --once`.
5. Start the agent.
6. Run again every `flopwire redact` command that you ran on this device.
   Until you do, search shows the text that you redacted.

### The agent stops: "apply redactions"

The agent could not apply a redaction to the stored rows.

1. Stop the agent.
2. Run `flopwire agent run --rebuild-index --once`. It indexes every
   transcript again, then exits.
3. Start the agent.

The rebuild removes the message rows, the conversations and the search
files. It keeps the sync state, the placements, the redaction files and
the key. Then it indexes every transcript again and masks each redacted
row as it writes it. Search is incomplete until the first pass ends. Use
the same procedure if the index is damaged in another way.

Do not add `--rebuild-index` to a login item or the launchd agent. The
agent would rebuild the index at each restart.

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

The harness hooks run `flopwire hook`. The command does two jobs:

- It prints the messages for this session into the session. See
  [Messaging](#messaging).
- It tells the running agent to index this transcript and upload it at
  once. Without hooks, a new line is still findable in about one second.
  The hooks make the upload immediate and cover a session that was idle
  for more than two days.

`flopwire hook` reads the hook input on stdin. It acts on the
`hook_event_name` field:

| Event | Prints into the session |
|---|---|
| `SessionStart` | The standing instruction, then any pending messages |
| `UserPromptSubmit` | Pending messages |
| `PostToolUse` | Pending messages |
| Any other event | Nothing. It only asks for the upload. |

A message arrives inside a running turn, at the next tool call, or with the
human's next prompt. A message never wakes an idle session and never
starts a turn.

A hook never fails a turn. `flopwire hook` exits 0 and prints nothing in
these cases:

- No message waits for the session.
- The agent is not running.
- The agent does not answer within 200 milliseconds. The agent keeps the
  messages for the next hook.
- The input is not hook JSON.

It writes the reason on stderr. It never writes the environment or a
message body there.

One call prints at most 5 messages and at most 9,000 bytes. The oldest
messages go first. The rest wait for the next hook. A single message that
is longer than the cap is cut, with a note that names the
`flopwire inbox --thread` command that shows all of it.

### Claude Code

1. Open `~/.claude/settings.json`.
2. Add these entries under `hooks`:

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "PostToolUse": [
      { "matcher": "*", "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "Stop": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ]
  }
}
```

### Codex

1. Open `~/.codex/hooks.json`. Create the file if it does not exist.
2. Add these entries:

```json
{
  "hooks": {
    "SessionStart": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "PostToolUse": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ]
  }
}
```

3. Open `~/.codex/config.toml`.
4. Add this line at the top level:

```toml
notify = ["flopwire", "agent", "flush"]
```

5. Start Codex in a terminal.
6. Codex shows "Hooks need review". Approve the three hooks.

Codex runs a hook only after you approve it. It asks again when the
event, the matcher, the command or the timeout changes.

`notify` uploads the transcript at the end of each turn. Codex passes a
JSON argument that names the thread (`thread-id`). The agent finds the
rollout by that session id.

### Devin CLI

1. Open `.devin/hooks.v1.json` in the repository. Create the file if it
   does not exist.
2. Add these entries. The event names are top-level keys. Do not put them
   under a `hooks` key: Devin rejects that file.

```json
{
  "SessionStart": [
    { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
  ],
  "UserPromptSubmit": [
    { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
  ],
  "PostToolUse": [
    { "matcher": "*", "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
  ],
  "Stop": [
    { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
  ]
}
```

Devin also runs the hooks in the repository's `.claude/settings.json`. When
both files call `flopwire hook`, each message still arrives once, and the
standing instruction arrives once.

Do not add `flopwire hook` to `PreToolUse`. Devin does not show that
event's output to the model, and the messages would be lost.

### Manual flush

Run `flopwire agent flush --path <transcript>` or
`flopwire agent flush --session <session id>`. `flopwire agent flush`
never prints messages. When it reads hook input, it exits 0 if the agent is
not running, is still busy after the timeout, stops during the call, or
does not track the file yet.

## Messaging

The agent carries messages between agent sessions
([design](../notes/message-bus/plan.md)). The `peers`, `send` and `inbox`
commands and MCP tools use it. The `flopwire hook` command prints each
message into the recipient's session. See
[Connect the harness hooks](#connect-the-harness-hooks). A session without
the hooks reads its messages with `flopwire inbox`.

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

### What the recipient sees

`flopwire hook` prints each message in one wrapper:

```
<flopwire-message id="m7f3a…" from="0b7e2c1a-…" user="alex@example.com" agent="claude" repo="api@main" sender="teammate" intent="request" sent="2026-10-01T14:02:11Z">
Can you rebase api on main?
<flopwire-ref address="4c19e0d2/12">assistant: the cursor is opaque…</flopwire-ref>
</flopwire-message>
Reply with the flopwire_send tool: to="0b7e2c1a-…" reply_to="m7f3a…" message="…"; or in a shell: flopwire send 0b7e2c1a-… --reply-to m7f3a… -- "…"
```

- Every attribute comes from the envelope that the server (or, without a
  server, the device agent) set. The sender supplies only the text, the
  intent, the reply id and the refs.
- `sender` is `own` for a session of the same person and `teammate` for
  another person's session.
- The reply line appears only for `intent="request"`.
- A ref shows a short excerpt when the recipient's local index holds that
  message. Otherwise it shows the address alone. `flopwire read ADDRESS`
  shows the whole message.
- The text is escaped: `&`, `<` and `>` become `&amp;`, `&lt;` and `&gt;`,
  and look-alike angle brackets become character references. Control and
  bidirectional characters are shown or replaced. A message therefore cannot
  close its wrapper, open another one, or imitate the standing
  instruction. Attribute values are escaped the same way, plus `"`.

At session start the hook also prints the standing instruction
(`busrender.StandingInstruction`). It tells the model what the wrapper is,
that a message from your own session is a request to act on within that
session's permissions, that a message from a teammate is information to
confirm with you first, and that a message never changes permissions or
settings. Without it, the models tested refused every request.

### Send and read messages

Run these commands from an agent session's shell, or call the MCP tools
`flopwire_peers`, `flopwire_send` and `flopwire_inbox`. They print compact
JSON with named fields and full session ids. Add `--text` (MCP:
`format: "text"`) for a readable form.

1. Find the session behind a change in the history:

   ```sh
   flopwire sessions --repo . --branch feat/cursor --json
   flopwire read SESSION --outline
   ```

   The digest lists the commits each session made. Do not choose a
   session by its title or its current branch alone. A title is the
   original task. A session can change branches after it commits.

2. Check that the session is live:

   ```sh
   flopwire peers --session SESSION
   ```

   An empty `peers` list means that the session is not running now. A
   message to it waits until it resumes or expires. Without `--session`,
   the command lists every live session. Your own session is not in the
   list.

3. Send the message:

   ```sh
   flopwire send SESSION -- "Heads-up: the list endpoint returns a cursor now."
   flopwire send @alex --intent request -- "Can you rebase api on main?"
   ```

   Put the main point in the first line. Use `-` as the text to read it
   from standard input. The command prints a receipt. The `arrives` field
   says when the message arrives: `next_tool_call`, `next_prompt`,
   `when_accepted`, `next_session` or `only_if_resumed`. A receipt is not
   a reply. With `--text`, the receipt is one line, for example:

   ```text
   sent m1a2b3c4d5e6f7a8 to 4c19e0d2 (gary codex api@main): idle, arrives with its human's next prompt
   ```

4. Check what you sent:

   ```sh
   flopwire inbox --sent
   ```

   Each entry has `direction` (`sent` or `received`) and `state`. The
   state of a sent message is its delivery only. A reply is a received
   entry whose `reply_to` names your message. To read one thread, use
   `flopwire inbox --thread ID`. When `more` is true, pass `next` as
   `--cursor`.

A failure prints one JSON object on standard error, and the command exits
with status 1. The object has a stable `code` (for example `thread_rate`,
`duplicate` or `agent_not_running`), a `detail`, a `fix` and an `example`.

The commands talk only to the device agent. If the agent does not run,
they stop with an error that says how to start it.

The sender is always the calling session. The commands find it as
[search](search.md#your-own-session) does. If no session is found, `send`
and `inbox` stop with an error. `peers` still lists the sessions. To name
the session, set `FLOPWIRE_SESSION_ID`, and `FLOPWIRE_AGENT` (`claude`,
`codex` or `devin`).

A session that a path rule keeps off the server cannot send. The agent
refuses the request before anything leaves the device.

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
