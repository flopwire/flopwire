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
| `messages: N pending delivery, N receipts unsent, N held for your acceptance` | Messages in the local inbox that no hook has confirmed yet, deliveries (and undelivered reports) the server has not taken yet, and messages from people you have not accepted. |

See [extraction diagnostics](extraction.md) for parser issue counts, source
inspection, and JSON output.

## Install into the harnesses

`flopwire setup` installs Flopwire into the coding-agent harnesses on this
device. It installs the Flopwire plugin into Claude Code, Codex and Devin
CLI: the MCP tools, the hooks and a messaging skill. It runs each harness's
own plugin commands. It never edits the harness's settings files, and it
writes no Devin config file.

`flopwire setup` prints a JSON report. Add `--text` for a readable form.
The report has these parts:

| Field | Means |
|---|---|
| `ok` | `false` when every detected harness failed. The exit status is then 1. A harness that fails while another succeeds has an `error` in its entry, and `ok` stays `true`. With no harness detected, `ok` is `true`. |
| `flopwire.path` | The `flopwire` binary that the plugin runs. Empty when `flopwire` is not on `PATH`. |
| `agent.running` | The device agent answered. Messages and capture need it. setup never starts it. |
| `server.configured` | A server is configured. Without one, messages go only between the sessions on this device. |
| `harnesses[]` | One entry per harness: `detected`, `installed`, `enabled`, `version`, `scope`, `done` (what setup changed), `warnings`, `todo` (what you must still do) and `error`. |
| `harnesses[].hook_trust` | Codex only. `hooks`: the plugin hooks Codex found. `trusted`: how many you approved. `need_review`: the events whose hooks still need your approval. `disabled`: the events whose hooks you turned off. |
| `todo` | What you must still do for the device. |

Use `--check` to report and change nothing. Use `--remove` to uninstall.
Run `flopwire setup --help` for every flag.

### Set up a device (for an agent)

Follow these steps in order.

1. Run `command -v flopwire`.
2. If the command prints nothing, stop. Tell your user to put the
   `flopwire` binary on `PATH`.
3. Run `flopwire setup --check`.
4. If `agent.running` is `false`, tell your user to start the device agent
   with `flopwire agent run` and keep it running. Do not start it
   yourself.
5. Run `flopwire setup`.
6. Read `error` in each entry of `harnesses`. Report each error to your
   user. If `ok` is `false`, stop.
7. Read `warnings` in each entry of `harnesses`. Show each warning to
   your user. A warning can name an older manual entry that now runs
   twice, or a hook that a plugin update enabled. Do not edit the settings
   file unless your user tells you to.
8. Read `todo` in each entry of `harnesses`, and the top-level `todo`.
   Tell your user each item.
9. If the Codex entry has a `hook_trust.need_review` list that is not
   empty, tell your user to approve the Flopwire hooks in Codex. See
   [Approve the Codex hooks](#approve-the-codex-hooks). Do not approve
   them yourself, and do not edit `~/.codex/config.toml`.
10. Tell your user to restart their Claude Code, Codex and Devin
    sessions. A running Claude Code session loads the plugin after a
    restart or after `/reload-plugins`. A running Codex or Devin session
    loads it after a restart.
11. Run `flopwire setup --check`.
12. Confirm that each detected harness has `installed: true` and
    `enabled: true`.

Run `flopwire setup` again at any time. It updates the plugin when a
new version exists. It changes nothing when the plugin is current.

### Approve the Codex hooks

Codex runs a plugin's hooks only after you approve them. `flopwire setup`
installs the plugin but never approves its hooks for you. Until you
approve them, Codex skips them: no message arrives in a Codex session and
the session gets no standing instruction. The MCP tools work without the
approval.

1. Start `codex` in a terminal.
2. Codex shows "Hooks need review".
3. Select "Review hooks".
4. Trust the five Flopwire hooks. They are on `SessionStart`,
   `UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, and each
   runs `flopwire hook || true`.
5. Run `flopwire setup --check`.
6. Confirm that the Codex entry shows `hook_trust.trusted: 5`.

You can also type `/hooks` in a running Codex session to review the hooks.
"Trust all and continue" also trusts every other hook that waits for
review.

What you approve: each hook runs the command `flopwire hook || true`
outside the Codex sandbox. The timeout is 5 seconds, and 3 seconds for
`SessionEnd`, the most that Codex allows for that event. `flopwire hook` reads the
hook input, asks the device agent for this session's messages, prints
them into the session, and asks the agent to index the transcript. `|| true`
keeps Codex from reporting a failed hook when `flopwire` is missing or too
old.

Codex asks again when a hook's event, matcher, command or timeout
changes, and when the plugin adds, removes or reorders a hook. Flopwire
keeps these fixed, so plugin updates do not ask again. The exception: the plugin version that added the `SessionEnd`
hook asks once more, for that new hook. Until you trust it, a Codex
session that exits leaves presence when its writer lock is released,
which the agent notices within seconds. Codex records the approval in `~/.codex/config.toml` under
`hooks.state`, keyed by the plugin and the hook, not by the install path.
After `flopwire setup --remove` the approval stays there, so a later
install does not ask again.

Codex also asks before each `flopwire_send` call, because a message leaves
the session. `codex exec` cannot ask, so the call fails there. To allow
`flopwire_send` without a question, add this to `~/.codex/config.toml`
yourself:

```toml
[plugins."flopwire@flopwire".mcp_servers.flopwire.tools.flopwire_send]
approval_mode = "approve"
```

A shell command in Codex's `workspace-write` sandbox cannot reach the
device agent, so `flopwire send` fails there with `sandbox_blocked`. Use
the `flopwire_send` tool in Codex.

### Remove

1. Run `flopwire setup --remove`.
2. Run `flopwire setup --check`.
3. Confirm that each detected harness has `installed: false`.
4. Restart your Claude Code, Codex and Devin sessions.

`--remove` removes only what setup installed. It does not remove a Devin
plugin that you installed without `--local`, or a plugin from another
source. It reports each one in `warnings`.

### Where the plugin comes from

The repository is a plugin marketplace for both harnesses. Claude Code
reads `.claude-plugin/marketplace.json`; its plugin is in
`plugins/claude-code/flopwire`. Codex reads
`.agents/plugins/marketplace.json`; its plugin is in
`plugins/codex/flopwire`. Both marketplaces are named `flopwire`, and both
plugins are `flopwire@flopwire`. `flopwire setup` adds the marketplace
from `flopwire/flopwire` on GitHub. Claude Code installs the plugin at user
scope; Codex has only user installs. Use `--source` or
`FLOPWIRE_PLUGIN_SOURCE` to install from a local checkout. See the
[Claude Code plugin README](../plugins/claude-code/flopwire/README.md) and
the [Codex plugin README](../plugins/codex/flopwire/README.md).

Devin CLI has no marketplace of its own: `devin plugins install` takes
one plugin source. Given the root of a Claude Code marketplace
repository, Devin 3000.10.31 and later recognizes the marketplace and
offers to install one of its plugins. Devin loads a Claude Code plugin as
it is, so setup names the plugin's directory in the repository and
installs `plugins/claude-code/flopwire` into Devin directly:

```sh
devin plugins install --local flopwire/flopwire#plugins/claude-code/flopwire -y
```

With a local checkout as the source, setup installs the directory
`<checkout>/plugins/claude-code/flopwire` instead. Devin then links the
directory, so edits apply in the next session.

- `--local` installs on this device only. Without it, Devin also adds the
  plugin to your personal plugins in Devin Cloud. They then load on every
  device you sign in to and in cloud sessions, where `flopwire` is not
  installed.
- `-y` answers Devin's install prompt, which lists the skill, the five
  hooks and the MCP server.
- Devin keeps the plugin in `~/.local/share/devin/cli/plugins`. setup
  updates it with `devin plugins update flopwire` and removes it with
  `devin plugins remove flopwire --local -y`.
- Devin cannot install a branch or tag: it reads `#` as a path in the
  repository. setup refuses a `--source owner/repo#ref` for Devin. Use a
  local checkout of that ref.
- Devin shows the plugin's skill as `/flopwire:messaging`.
- Every `devin plugins` command needs a Devin login. When you are not
  logged in, setup reports `not logged in to Devin` in the Devin entry and
  continues with the other harnesses. Run `devin auth login`, then run
  setup again.
- `devin plugins update` enables the hooks of a new plugin version without
  a prompt. setup compares the plugin's hooks before and after the update.
  It reports each hook that the update enabled in `warnings`, and each hook
  that the update removed in `done`.

Devin reads hooks from Claude Code's settings files, but not from Claude
Code's plugins. The Claude Code plugin and the Devin plugin therefore never
run in the same Devin session.

The Codex desktop app, the IDE extension and Devin Desktop were not tested
with the plugin.

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
| `SessionStart` | The standing instruction if the session is owed it, then any pending messages |
| `UserPromptSubmit` | The same |
| `PostToolUse` | The same |
| `SessionEnd` | Nothing. It tells the agent that the session ended. |
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
- The hook process started so long ago that it cannot finish asking for
  messages within 3 seconds of its start. Its harness may have stopped
  waiting for it. Such a hook also does not report its event to the
  agent, because a later event may have arrived first. A late
  `SessionEnd` hook still reports its event.
- The input is not hook JSON.

It writes the reason on stderr. It never writes the environment or a
message body there.

Delivery has two steps, so a hook that is killed cannot lose a message:

1. The hook asks the agent for the session's messages. The agent leases
   them to that hook for 10 seconds. While the lease runs, no other hook
   of the session gets messages, so the messages keep their order.
2. The hook prints the messages. Then it confirms them to the agent. The
   agent marks them delivered and sends the receipt to the server.

If no confirmation arrives before the lease ends, the agent offers the
messages again at the session's next hook. A harness timeout, a killed
hook, a closed output pipe or a lost confirmation all end this way. A
message that is offered again carries `redelivery="true"`, because the
model may have seen it already. The hook does not confirm a print that
ends more than 3 seconds after its start: a harness that timed the hook
out may not have read it.

After 3 leases without a confirmation, the message is `undelivered` with
the reason `unconfirmed`. The sender's `inbox` shows this state, and the
sender can send the message again.

The standing instruction is delivered the same way. A session is owed it
until a hook confirms that it printed it. The hook prints it before any
message. While one hook holds the instruction's lease, no other hook of
the session gets messages, so no message arrives before the instruction.
If the `SessionStart` hook is killed or starts too late, the session's
next `UserPromptSubmit` or `PostToolUse` hook prints it. Two hook
configurations that both run `SessionStart` print it once. The agent keeps
the confirmation in `bus.db`, so a restart does not print it again. After
3 unconfirmed leases the agent stops offering it.

A `SessionStart` with the source `resume`, `compact` or `clear` that
starts after the confirmation makes the session owed the instruction
again. A compaction summarizes the earlier context, so the instruction is
likely gone from it. Flopwire cannot see whether a resumed session kept
it. A second copy costs some context; a missing copy leaves the model
without the trust rules.

When a session ends before a hook delivers its message, the message is
`undelivered` with the reason `session_ended`. It is never given to
another session. See [Ended sessions](#ended-sessions).

One call prints at most 5 messages and at most 9,000 bytes. The oldest
messages go first. The rest wait for the next hook. A single message that
is longer than the cap is cut, with a note that names the
`flopwire inbox --thread` command that shows all of it.

### Claude Code

Run `flopwire setup`. See [Install into the harnesses](#install-into-the-harnesses).
The plugin it installs runs `flopwire hook` on `SessionStart`,
`UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, and serves the
MCP tools.

Use the manual configuration below only when you cannot install the
plugin. Do not use both: each hook would then run twice.
`flopwire setup` warns when it finds both.

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
    ],
    "SessionEnd": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ]
  }
}
```

3. Run `claude mcp add --scope user flopwire -- flopwire mcp`.

### Codex

Run `flopwire setup`, then approve the hooks. See
[Install into the harnesses](#install-into-the-harnesses) and
[Approve the Codex hooks](#approve-the-codex-hooks). The plugin runs
`flopwire hook` on `SessionStart`, `UserPromptSubmit`, `PostToolUse`,
`Stop` and `SessionEnd`, and serves the MCP tools.

The plugin's `Stop` hook replaces the older
`notify = ["flopwire", "agent", "flush"]` line: it asks the agent to index
each finished turn. It prints nothing and never blocks or extends a turn.
`notify` holds one command only, so the hook also leaves `notify` free for
other tools.

Use the manual configuration below only when you cannot install the
plugin. Do not use both: each hook would then run twice.
`flopwire setup` warns when it finds both.

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
    ],
    "Stop": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
    ],
    "SessionEnd": [
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 3 }] }
    ]
  }
}
```

3. Run `codex mcp add flopwire -- flopwire mcp`.
4. Start Codex in a terminal.
5. Codex shows "Hooks need review". Approve the five hooks.

Codex runs a hook only after you approve it. It asks again when the
event, the matcher, the command or the timeout changes.

### Devin CLI

Run `flopwire setup`. See [Install into the harnesses](#install-into-the-harnesses).
The plugin it installs runs `flopwire hook || true` on `SessionStart`,
`UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, and serves the
MCP tools. Devin has no hook approval step.

On `Stop`, `flopwire hook` prints nothing. Devin continues a turn when a
`Stop` hook prints `"decision": "block"`, so a `Stop` hook that printed
output could extend a turn. `flopwire hook` never does.

`devin -r ID` on a session that another Devin process runs fires the
`SessionStart` hooks, then Devin refuses the session. `flopwire hook`
delivers nothing to such a process: the session's lock names a running
`devin` that is not its parent. The messages wait for the running session.

Devin also runs Flopwire hooks that it finds in these files:
`~/.config/devin/config.json` and `.devin/` files in the repository, and
Claude Code's `~/.claude/settings.json`, `~/.claude/settings.local.json`,
`~/.claude.json` and the repository's `.claude/settings.json`. Each such
hook then runs twice in a Devin session. Each message and the standing
instruction still arrive once. `flopwire setup` reports each such entry in
`warnings` and does not edit the file. Keep an entry in a Claude Code
settings file when Claude Code has no Flopwire plugin and needs it.

Use the manual configuration below only when you cannot install the
plugin.

1. Open `~/.config/devin/config.json`. To connect one repository only,
   open `.devin/hooks.v1.json` in the repository instead.
2. Add these entries. In `~/.config/devin/config.json`, put the events
   under a `hooks` key and keep the other keys. In `.devin/hooks.v1.json`,
   the event names are top-level keys: Devin rejects that file with a
   `hooks` key.

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
  ],
  "SessionEnd": [
    { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5 }] }
  ]
}
```

3. Run `devin mcp add flopwire --scope user -- flopwire mcp`.

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
- It confirms each delivery to the server, in batches, after the hook
  confirms that it printed the message. It reports each message that
  became `undelivered` (`unconfirmed` or `session_ended`) in the same
  batches.

Without a server configuration, or with `--no-sync`, the agent routes
messages between the sessions on this device. To address yourself, use
`@` and your account name. The server's limits apply: duplicates within 10 minutes,
8 messages per thread per hour, 30 sends per session per hour, and 50
undelivered messages per recipient.

A session is live when `flopwire sessions` shows it as live and it has
not ended. The agent also reads these harness files. It never writes
them:

| Harness | File | Gives |
|---|---|---|
| Claude Code | `~/.claude/sessions/<pid>.json` | Open while the process runs and started when `procStart` says. Busy when `status` is `busy`. |
| Codex | `~/.codex/thread-writer-locks/<thread>.lock` | Open while a Codex process holds the file's lock. The file stays after the process exits. Busy from the last task event in the rollout. |
| Devin | `session_locks/<session>.lock` beside `sessions.db` | Open while the named process runs and is `devin`. A Devin session without such a lock is not live, even when it wrote a moment ago. |

To see whether a Codex process holds a thread's lock, the agent does what
Codex's own cleanup does. It takes `.coordination.lock` in the same
directory without waiting, tries a shared lock on the thread's file
without waiting, and releases both at once. A Codex that starts in that
moment waits microseconds for the coordination lock; it is never refused
its thread. When the coordination lock is busy, or the directory has no
`.coordination.lock` (an older Codex), the agent counts the file's
existence, as before.

Devin's store does not show whether a turn runs. The agent uses the hook
events instead: each `flopwire hook` call tells the agent its event. After
`UserPromptSubmit` or `PostToolUse` the Devin session is busy. After `Stop`
it is idle. A session with no hook event for 15 minutes is idle. A session
is idle until its first hook event after the agent starts.

A message waits for 24 hours. Then it expires.

To see the messaging state, run `flopwire agent status`.

### Ended sessions

A session ends when its harness shows it, never because it is idle. The
agent takes these signals:

| Harness | The session ended when |
|---|---|
| Claude Code | Its `SessionEnd` hook runs. Or its `sessions/<pid>.json` names a process that is not running or that started at another time (a killed process leaves the file). Or the file that named it is gone at two reads 1 second apart (a clean exit removes it). |
| Codex | Its `SessionEnd` hook runs. Or no process holds its writer lock (the kernel releases the lock when the process exits, also when it is killed). Or the lock file is gone at two reads 1 second apart. |
| Devin | Its `SessionEnd` hook runs. Or its `session_locks/<session>.lock` names a process that is not running or is not `devin`. |

The agent reads the harness files on each 2-second presence check and on
each `peers` call (presence is cached for 1 second). An ended session
leaves presence at that read, and its messages are marked then. On this
device, `peers` stops listing it within about 1 second of a `SessionEnd`
hook or a killed process, and within about 3 seconds of a clean exit
without the hook (the second read). The server learns it from the next
poll, which the next 2-second presence check starts.

One window remains. A session that ran and exited between two presence
checks (a `claude -p` of under 2 seconds) and whose harness ran no
`SessionEnd` hook never showed in its harness file. It stays live for 10
minutes after its last write, as `flopwire sessions` shows it. With the
plugin's `SessionEnd` hook installed, it ends at once.

When a session ends, each message that waits for it (queued for it, or an
`@user` message claimed for it) becomes `undelivered` with the reason
`session_ended`. The sender's `inbox` shows it, so the sender can send it
again. The message is never given to another session. A message that a
hook took and has not confirmed keeps its lease: a late confirmation still
counts. If the lease ends unconfirmed, the message becomes
`undelivered` with `session_ended`.

A message that arrives after the session ended is marked the same way
only when its sender could still have been told that the session runs.
A message sent later went to a session that `peers` no longer listed. Its
receipt said `only_if_resumed`, and it waits for a resume.

- Without a server, the receipt comes from this agent's presence, which
  is at most 1 second old. A message sent within 1 second of the last
  time the agent saw the session live is marked. The receipt and the
  mark always agree.
- With a server, the server learns of the end from the next poll, about
  2 seconds later, and its clock may differ. A message sent within 10
  seconds of the last live sighting is marked. In that window a message
  whose receipt said `only_if_resumed` can be marked `undelivered` too.
  The sender then sees it and can send it again.

A session resumes when a hook of it runs that started after the end, or
when its harness file names a newer process. A resumed session receives
its new messages. Messages that were already `undelivered` stay so.

The agent keeps this state in `bus.db`. A session that its harness file
showed before an agent restart, and that is gone after it, ends at the
first presence checks after the restart.

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
- `redelivery="true"` appears on a message that an earlier hook took and
  never confirmed. The model may have seen it. The standing instruction
  tells the model not to act again on a message id that is already in
  its context.
- A ref shows a short excerpt when the recipient's local index holds that
  message. Otherwise it shows the address alone. `flopwire read ADDRESS`
  shows the whole message.
- The text is escaped: `&`, `<` and `>` become `&amp;`, `&lt;` and `&gt;`,
  and look-alike angle brackets and the invisible Unicode tag characters
  become character references. Control and
  bidirectional characters are shown or replaced. A message therefore cannot
  close its wrapper, open another one, or imitate the standing
  instruction. Attribute values are escaped the same way, plus `"`.

Before the first message of a session, the hook prints the standing
instruction (`busrender.StandingInstruction`). See
[Connect the harness hooks](#connect-the-harness-hooks) for when it is
printed. It tells the model what the wrapper is,
that a message from another session of your own user is a request to
act on within this session's permissions, that a message from a teammate is information to
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

   Each entry has `direction` (`sent` or `received`) and `state`:
   `queued`, `held`, `claimed`, `delivered`, `read`, `expired`,
   `refused` or `undelivered`. A `refused` or `undelivered` entry also
   has a `reason`. `undelivered` with `unconfirmed` means that hooks took
   the message 3 times and none confirmed that it printed it.
   `undelivered` with `session_ended` means that the recipient session
   ended before a hook delivered the message. Send it again, to another
   session if the work still needs one. The state
   of a sent message is its delivery only. `read` (with `read_at`) means
   that the message's text entered the recipient's context, not that the
   recipient acted on it ([read receipts](messaging.md#read-receipts)).
   A reply is a received
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

A subagent is not a session, so it gets no messages. A hook that runs in
a subagent prints nothing and takes nothing. The session's own next hook
delivers the messages. A message that a subagent sends goes out as its
parent session, and the reply goes to the parent session.

A session that a path rule keeps off the server cannot send. The agent
refuses the request before anything leaves the device.

### Known limits

- Only Claude Code, Codex and Devin CLI sessions send and receive.
  opencode (#62) and vendor cloud sessions, such as Claude Code cloud
  sessions and Devin cloud (#63), are not supported.
- opencode has no read receipts.
- A Devin hook finds a subagent's tool call in Devin's session store. If
  the hook cannot read the store, it delivers messages only at a prompt.
  It writes the cause to stderr.
- Without a server, the agent applies the per-session, per-thread,
  duplicate and recipient limits. It does not apply the per-device and
  per-person ceilings of the server (#71).
- `@user` messages are routed by repo name. With `--server`, `--repo`
  does not match another machine's checkout at another path (#102).
- Devin CLI shows no held-message notice. `codex exec` does not show it
  either.

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

The same facts decide which repo a session is on for `--repo` in
`sessions`, `grep`, `search` and `peers`. A session's repository is its
main checkout (for a worktree of a bare repository, the bare repository)
and its remote. All worktrees of one repository are then one repo, with
or without a remote. Two repositories with the same name stay two repos.
A session in a deleted worktree stays on its repo through the stored main
checkout, or through the one that the recovery pass below records. A
session with only candidates is on no repo by its candidates. It still
matches by its directory. See [search.md](search.md#which-repo).

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
