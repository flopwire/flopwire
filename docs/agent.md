# Device agent

The device agent keeps the local index of this device current. It reads
Claude Code, Codex, Devin, opencode, local Cowork and Claude Desktop Local
Code transcripts. It never writes to the harness directories. When the
device has a server configuration, the agent also uploads the transcripts.
To install Flopwire into the harnesses, see [harnesses](harnesses/README.md).

## Run the agent

1. Run `flopwire agent run`.
2. Keep the process running. Use a login item, a launchd agent, or a
   terminal multiplexer.

The first pass indexes transcripts in the configured collection roots. On the reference
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
| Sync spool | `<config dir>/flopwire/spool/<queue key>`, at most 1GiB per queue | `--spool-cap` (bytes) |
| Message inbox | `<config dir>/flopwire/bus-<authority hash>.db` | none |
| User path rules | `<config dir>/flopwire/path-rules` | none |
| Admin path rules cache | `<config dir>/flopwire/admin-path-rules.json.<authority hash>` | none |
| Claude projects | `~/.claude/projects` | `--claude-projects` or `CLAUDE_CONFIG_DIR` |
| Desktop Local Code container | `~/Library/Application Support/Claude/claude-code-sessions` on macOS; no default on other platforms | `--desktop-code-root`; `-` disables |
| Cowork container | `~/Library/Application Support/Claude/local-agent-mode-sessions` on macOS; no default on other platforms | `--cowork-root`; `-` disables |
| Codex home | `~/.codex` | `--codex-home` or `CODEX_HOME` |
| Devin store | `~/.local/share/devin/cli/sessions.db` | `--devin-db` or `FLOPWIRE_DEVIN_DB`; `-` disables |
| opencode store | `~/.local/share/opencode/opencode.db` (`opencode db path`) | `--opencode-db`, `FLOPWIRE_OPENCODE_DB`, opencode's `OPENCODE_DB`, or `XDG_DATA_HOME`; `-` disables |

Message inboxes belong to one normalized server URL and enrolled device ID.
Switching servers or devices opens a separate inbox. Returning to the same
server and device resumes its pending messages and receipts. Token and TLS
pin rotation preserve an enrolled device's inbox. Credentials without a
device ID use a token fingerprint to distinguish principals. Standalone
messaging uses a separate inbox.

The agent does not read a `bus.db` file in the config dir. That file does
not record its server or device owner. Do not rename or copy it into an
authority inbox: that would assign unverified message bodies and receipts
to that authority.

On macOS the user cache dir is `~/Library/Caches` and the config dir is
`~/Library/Application Support`.

### Storage mode

After `flopwire enroll`, the agent defaults to minimal storage (sync-only).
It uploads transcripts and keeps no local message index. Local search then
fails: `flopwire grep`, `search`, `sessions` and `read` with `--local`
report `this device is sync-only; use --server`. Search the server
instead.

To keep a local index on an enrolled device, do one of these:

- Set `"mode": "full"` in `config.json` in the config directory.
- Start the agent with `flopwire agent run --sync-only=false`.

The index mode comes from these sources, in order:

1. The `--sync-only` flag. `--sync-only=false` forces a full index.
2. The `mode` field of the client config: `"full"` or `"sync-only"`.
3. When `mode` is empty: sync-only when the config has a server and a
   token, else a full index. `--no-sync` selects a full index.

The same rule applies to `FLOPWIRE_TOKEN` / `FLOPWIRE_SERVER` launches.
Login preserves an explicit mode across server changes.

A sync-only index keeps only sources, watermarks, conversations,
placements and the sync spool. It has no message rows and no search shard
files. Sync-only mode needs a configured server. Use
`--sync-only=false --no-sync` to run standalone with a full index when the
config selects sync-only.

An existing full index with message content needs an explicit choice
before it switches to sync-only. Use `--sync-only=false` to keep it, or
`--sync-only` to remove its message rows and search shards. An empty index
that `flopwire mcp` created can switch without a choice. A change of mode
rebuilds the index at the next start: a switch to full parses every
transcript again, and a switch to sync-only drops the message rows and the
shard files.

### Local search files

New and rebuilt local indexes keep token search at `full` detail and use
`column` detail for trigram search. This includes explicit
`--rebuild-index` requests and rebuilds already required by a schema or
storage-mode change. Healthy indexes keep their recorded formats on reopen
and when the agent repairs a missing search file. The defaults do not add
a rebuild trigger.

Stop the agent before a rebuild. Keep its `--db` path and storage options.
See the [recovery procedure](troubleshooting.md#the-agent-stops-apply-redactions).
The rebuild discards indexed message rows and reparses the available
harness transcripts. It cannot restore content from unavailable transcripts.
It preserves sync state and ownership, placements, and local redaction
files and keys. Search is incomplete until the pass finishes.

### Upload workers

Upload concurrency defaults to one. Start the collector with
`flopwire agent run --sync-workers=2` to request two upload workers.
Start the server with `flopwire serve --flush-per-device=2` to permit them.
See [parallel sync](parallel-sync.md).

### Cowork and Desktop Local Code

Cowork collection reads native Claude evidence under
`<container>/<account>/<workspace>/<app-session>/.claude/projects`.
It preserves native session IDs and original paths. It uses adjacent app
metadata to apply path rules across verified selected and approved Mac folders.
The strictest rule anywhere within each selected folder wins, including
nested directory and glob rules. Logical and physical folder paths both apply.
An unresolved nested repository scope holds sharing at local mode; an actual
matching deny rule excludes indexing. Folders that the agent observed earlier keep
protecting the session after app permissions change or metadata disappears.

Cowork uploads remain held when the server does not support stored host-folder
policy evidence. The hold also covers identified CLI copies of those sessions.
Status reports the server support hold separately from unknown folder mapping.
VM paths, unresolved mount identifiers and missing mapping leave an unknown
scope. Metadata-only sessions have a current mapping hold without marking
historical evidence unknown. Evidence indexed under unknown mapping retains
a historical unknown scope. A later valid metadata file does not clear that scope.
App metadata and audit streams are excluded from transcript collection.

Desktop Local Code collection links verified app session metadata to native
Claude session IDs in the configured Claude projects root. It also reads
matched native transcripts under
`<code-container>/<account>/<organization>/<app-session>/.claude/projects`.
Native children and companions keep their original IDs and paths. The agent
watches new app metadata and nested native history directories. It excludes
unrelated native parents, ordinary chat, SSH sessions and WSL sessions.

Normal-root Local Code history uses existing Claude collection and sharing
rules. Scoped Code uploads require current verified app linkage and a contained
source lease for exactly the indexed bytes. Each captured generation stores
its file identity, byte bound and companion digest. The source retains its
protected container boundary after generation cleanup or a restart. Unknown
linkage, changed rules, unavailable indexed proof and earlier unproven captures
hold sharing. Disabling or changing the Code root does not release old pending
captures through the ordinary CLI path.
Imported Cowork session IDs retain Cowork folder-policy holds, including
copies whose original Cowork container is absent. Code metadata supplies
identity evidence only. It does not establish Cowork Mac-folder mapping.
Historical unknown imports follow the configured unplaceable policy, including
`exclude`, even when current Mac-folder mapping is complete.

### Other options

Use `--once` to index what changed and exit. Use `--no-sync` to index
without upload. `flopwire agent run -h` lists every flag.

The agent sets a soft limit of 192MiB on Go memory (`--mem-limit`, or
`GOMEMLIMIT`).

Only one agent writes an index. A second `flopwire agent run` on the same
index waits 3 seconds for the lock, then exits with
`agent already running (pid N)`. The wait covers `flopwire mcp`, which
holds the lock for a moment when it creates an empty index on a device
whose agent never ran. `flopwire agent run --once`
asks the running agent for a pass over the control socket. It waits for local
commit and upload acknowledgment. `--sync-timeout` bounds the upload wait
(default 5 minutes), as it does for a fresh process. The running agent must
match the requested retention mode and local or upload connection. Upload
requests must also match its server and device. A token without a device ID
must match the running agent's credential. A mismatch fails without
changing the running agent or rebuilding the index. Stop the agent before
changing these settings. If the index is locked and no agent
answers yet, `--once` waits.
The search commands open the index read-only and never take the lock.

## Local redactions: what they guarantee

`flopwire redact` hides a message, or some of its lines, in the local
index of this device. After the command succeeds, the hidden text does not
come back through `flopwire` search, grep or read, through the MCP
server, or in a conversation title or digest. This stays true after a
re-index, a rebuild of the index, a grown or rewritten message, and a
crash of the agent.

Local redaction does not protect the files on disk:

- The harness transcripts (`~/.claude`, `~/.codex`, the Devin and opencode stores) are
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

To recover from an agent that stops with a redaction or key error, see
[troubleshooting](troubleshooting.md).

## Check the agent

Run `flopwire agent status`. It asks the running agent over the control
socket and prints these parts:

| Line | Means |
|---|---|
| `agent: running` | The agent answered. |
| `version: vX.Y.Z (server vA.B.C)` | This binary's version and the one the server last reported. |
| `upgrade: flopwire vA.B.C is available; …` | The server, or the newest published release (checked daily), is ahead of this device. The same line reaches each new session's context at its SessionStart hook and `flopwire setup --check`. See [Upgrade a device](releases.md#upgrade-a-device). |
| `Claude Desktop Code (Local): STATE` | Local Code discovery, scoped native sessions, normal-root links, missing linked transcripts and excluded backend records. |
| `scoped shared uploads held: contained sync authorization support pending` | The active sync implementation cannot acquire scoped Code authorization. Native collection continues locally. |
| `scoped shared authorization: each capture requires fresh contained indexed proof` | The sync authorization hook is available. Each capture must pass its current linkage, folder rules, indexed byte bound and contained source checks. Cowork origins keep their folder-policy holds. |
| `Cowork: STATE` | Current local discovery state, native sessions, metadata-only sessions and excluded paths. JSON status also includes mapping reasons, historical unknown scopes and watch candidates. |
| `shared uploads held: server host-folder policy support pending` | Cowork evidence remains local. Empty sync queues do not establish shared Cowork coverage. |
| `path rules removed N sessions from the local index` | A new `deny` rule purged local rows. The first 20 sessions follow. Their server copies stay unless an admin rule covers them; see [path rules](path-rules.md). |
| `sessions placed by (for path rules):` | How many sessions each placement method placed. See [Where a session ran](path-rules.md#where-a-session-ran). |
| `sync: off (no server configured)` | The device is not enrolled, or the agent runs with `--no-sync`. |
| `sync: ok` | No current scheduler transport failure is reported. It does not prove discovery, upload or parse completeness. |
| `sync: server unreachable, retry at T: ERR` | The server did not answer. The agent retries with exponential backoff. Server `Retry-After` can extend the configured backoff ceiling. |
| `sync: stopped until the server is re-pinned` | The server's certificate does not match the pinned fingerprint. Uploads stop. Run `flopwire login --fingerprint <new>` after you confirm the new fingerprint. |
| `upload workers: N` | Effective upload concurrency. The default is one. |
| `upload capability check: ERR (using one worker; retrying)` | The check failed. Serial uploads continue and the check retries. |
| `queued: N sources; spool: N bytes` | Scheduled source checks and active turns, plus spool storage usage. A check may find no new bytes. These are not totals of undiscovered or unuploaded history. `(full: …)` means the spool reached `--spool-cap` and captures of rewritten sources pause. |
| `failing sources (N)` | Each source that fails, its error and its attempts. A failing source retries on its own and does not delay the others. |
| `server refused N sources by admin path rule` | The server did not store these sources. Each shows its path and the rule. The device learns of a refusal on its next upload of that source. |
| `messaging: connected` | The agent holds its poll to the server. See [Messaging](messaging.md). |
| `messaging: local` | No server is configured. Messages go between the sessions on this device. |
| `messaging: server unreachable, retry at T: ERR` | The poll failed. The agent retries with exponential backoff. Server `Retry-After` can extend the configured backoff ceiling. |
| `messaging: stopped: ERR` | The server refused the credential or the certificate. Run `flopwire login`. Messaging resumes when the agent sees the new credential. |
| `messaging: disabled: ERR` | The server has no message bus, or the credential is not an enrolled device. The agent asks again every 10 minutes. |
| `messages: N pending delivery, N receipts unsent, N held for your acceptance` | Messages in the local inbox that no hook has confirmed yet, deliveries (and undelivered reports) the server has not taken yet, and messages from people you have not accepted. |
| `credential: SOURCE` | The credential the agent uses. `device login`: an enrolled device. `FLOPWIRE_TOKEN`: a minted token from the agent's environment. `legacy login`: a login with no device credential. `none`: no server. A device login also shows `ok`, a deadline warning or `re-login required`. |
| `messaging: off: REASON: FIX` | The credential cannot use the message bus. The agent does not poll. Do the fix; messaging resumes when the agent sees the new credential. |
| `warning: FLOPWIRE_TOKEN in the environment hides the device login saved for URL` | `FLOPWIRE_TOKEN` wins over the saved config. Unset it in the agent's environment to use the device login. |
| `note: this shell's credential is X, the running agent's is Y: WHY` | This shell and the agent have different environments: `FLOPWIRE_TOKEN` is set in only one of them. The agent's credential decides uploads and messaging. |

`flopwire setup --check` shows the same `credential`, `messaging: off`,
`warning` and `note` lines in its server section, for the running agent when
it answers.

The agent reads the server credential at startup. When a login is saved
while it runs without one, or the saved credential is removed, the agent
restarts itself within 2 seconds. It then uploads and polls the server, or
runs local. Automatic storage mode follows this transition: connected
uses minimal storage; credential removal restores a full standalone index.
An explicitly selected sync-only agent keeps its last credential because
it requires a server. Stop it before changing to standalone mode.

A login to a different server while the agent runs does not restart it.
Restart the agent yourself after you switch servers (see
[Stop the agent](#stop-the-agent)).

Each sync index records its server and enrolled device. A restart under a
different server or device refuses connected agent startup, including
indexing and messaging. Restore the original enrollment to resume its queue.
Use a separate `--db` path to start a fresh queue for another enrollment. Same-device token rotation keeps
the queue. Each new index gets a separate spool key, including indexes for
the same enrollment. A copied index retains its queue identity; do not run
copies concurrently against the same config directory.

After an automatic standalone restart, the index can contain new local
content. Returning to the original login can then require an explicit full
or minimal storage choice before the agent starts. Retention selection and
queue ownership are separate checks.

An environment or unenrolled token has no proven device identity. Its token
hash owns the queue. Changing that token requires a fresh queue, including
rotations that the operator expects to represent the same device. The agent
does not borrow a saved device ID for an environment token.

The authority key identifies a normalized server URL and enrolled device.
It does not identify a server installation. A replacement server at the same
URL with the same device identity cannot be distinguished by this key.

The spool cap applies to the active queue. Inactive queue spools still use
disk space outside that cap. A fresh `--db` path leaves old captures pending. `--rebuild-index` preserves sync ownership and
queue tables; it cannot assign or recover a refused queue.
See [troubleshooting](troubleshooting.md#inspect-extraction-diagnostics)
for parser issue counts, source inspection, and JSON output.

### Manual flush

Run `flopwire agent flush --path <transcript>` or
`flopwire agent flush --session <session id>`. `flopwire agent flush`
never prints messages. When it reads hook input, it exits 0 if the agent is
not running, is still busy after the timeout, stops during the call, or
does not track the file yet.

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


## See also

- [Harnesses](harnesses/README.md): support matrix, `flopwire setup`, and
  the hooks of each harness.
- [Messaging](messaging.md): send, receive, accept and revoke messages.
- [Path rules](path-rules.md): keep sessions out of the index or off the
  server.
- [Troubleshooting](troubleshooting.md): an agent that stops, and
  extraction diagnostics.
- [Parallel sync](parallel-sync.md): two upload workers.
- [Coverage reporting](coverage-reporting.md): read the coverage reports.
- [Daemon diagnostics](agent-diagnostics.md): measure the agent's work.
