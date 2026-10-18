# Harnesses

Flopwire reads past and ongoing conversations from supported AI-agent
harnesses, including research, planning, writing, troubleshooting and coding.
It installs hooks and tools into some of them, so their sessions can send and receive
messages. This page shows what each harness supports and how
`flopwire setup` installs Flopwire. Each harness has its own page for its
hooks.

## Support matrix

| Harness | History indexed | Search | Receives messages | Sends messages | Hooks installed by `flopwire setup` |
|---|---|---|---|---|---|
| [Claude Code CLI](claude-code.md) | Yes | Yes | Yes | Yes | Yes: plugin with hooks, MCP tools and a skill |
| [Claude Code cloud](cloud.md) | No | No | Yes, while a turn runs | No | No: Flopwire installs nothing in the cloud session |
| Claude Desktop Local Code | Yes | Yes | No | No | No |
| Cowork | Yes | Yes | No | No | No |
| [Codex CLI](codex.md) | Yes | Yes | Yes, after you approve the hooks | Yes | Yes: plugin with hooks, MCP tools and a skill |
| Codex IDE extension | Yes, parsed as Codex sessions | Yes | Untested | Untested | Untested |
| [Devin CLI](devin.md) | Yes | Yes | Yes | Yes | Yes: the Claude Code plugin, installed with `--local` |
| [Devin cloud](cloud.md) | No | No | Yes, while a turn runs | No | No |
| [opencode](opencode.md) | Yes | Yes | Yes | Yes | Yes: one plugin file in place of hooks |

- Claude Desktop Local Code and Cowork are history only. The agent reads
  their transcripts on macOS. See
  [Cowork and Desktop Local Code](../agent.md#cowork-and-desktop-local-code).
- Cloud sessions run on the vendor's machines. The device agent pushes
  messages into them while they run a turn. Their transcripts are not in
  the archive. See [cloud.md](cloud.md).
- The Codex IDE extension writes the same rollout files as the Codex CLI.
  The agent indexes them as Codex sessions. Hook delivery and tools in the
  IDE extension, the Codex desktop app and Devin Desktop are untested. To
  check them by hand, see [manual-checks.md](../dev/manual-checks.md).

## What agents discover

Flopwire exposes nine tools and one skill, `flopwire:messaging`. The table
below summarizes the tools; it is not a verbatim copy of their descriptions.

| Tool | Purpose |
|---|---|
| `flopwire_search` | Find prior research, plans, decisions, attempts and results with ranked keyword matching. |
| `flopwire_grep` | Find exact text or regex matches in captured conversations. |
| `flopwire_sessions` | Discover past and ongoing sessions by repository, person, time or title. |
| `flopwire_read` | Read source messages and surrounding context to verify evidence. |
| `flopwire_change` | Find captured command/result evidence for full commit IDs. The CLI calls this `trace`. |
| `flopwire_messages` | Read retained repository coordination across devices and ended sessions. Requires enrollment. |
| `flopwire_peers` | Find live sessions available for contact. |
| `flopwire_send` | Send an authorized message to a session or to a person's current or next session. |
| `flopwire_inbox` | Check this session's message delivery or re-read its threads. |

History is available after a session ends and while it is ongoing. Search
uses this device before enrollment and the shared server after enrollment.
Configured servers connect machines and team members; results report their
scope. Presence is needed for live contact, not for reading history.

The MCP server supplies common instructions plus each tool's description
and argument schema. Harnesses control how and when those definitions
reach the model. The skill's name and description support discovery; its
body supplies evidence-recovery and coordination workflows when loaded.
Hooks deliver incoming messages and the instructions for handling them.
Descriptions guide tool selection; they do not guarantee a call or grant
permission to send a message.

Claude Code, Codex and Devin CLI use the skill. opencode has plugin tools
and system-prompt guidance instead of a separate skill; its plugin reads
the same tool definitions from the binary.

Exact definitions in this checkout:

- [Retrieval tools and MCP server instructions](../../cmd/flopwire/mcp.go).
- [Commit-evidence tool](../../cmd/flopwire/change.go).
- [Messaging tools and MCP messaging instructions](../../cmd/flopwire/mcp_bus.go).
- [Skill description and full instructions](../../plugins/claude-code/flopwire/skills/messaging/SKILL.md),
  also copied into the [Codex plugin](../../plugins/codex/flopwire/skills/messaging/SKILL.md).

These links describe this source revision. An installed binary or cached
plugin may be older. Upgrade the binary for new MCP descriptions, run
`flopwire setup` to refresh plugins, then reload the integration or restart
the session as its harness requires. A running MCP process can retain the
old definitions until it reconnects.

## Install into the harnesses

`flopwire setup` configures supported harnesses whose CLI commands it finds
on this device's `PATH`. Installing the binary alone does not configure a
harness. It installs the Flopwire plugin into Claude Code, Codex and Devin
CLI: the MCP tools, the hooks and a messaging skill. It runs each harness's
own plugin commands. It never edits the harness's settings files, and it
writes no Devin config file. For opencode, which loads every file in its
plugin directory, it writes the one plugin file there
([opencode.md](opencode.md)).

`flopwire setup` prints a JSON report. Add `--text` for a readable form.
The report has these parts:

| Field | Means |
|---|---|
| `ok` | Whether setup completed its checks or changes. This does not mean the current session can use the tools or receive messages. Read `todo` and `warnings` even when `ok` is `true`. `false` when a harness command failed. The exit status is then 1. A harness that you are not logged in to (Devin) is skipped: its entry has `skipped`, and `ok` does not change. |
| `flopwire.path` | The `flopwire` on `PATH`. Empty when `flopwire` is not on `PATH`. The plugins run the binary their shim finds (`hook_binary`). |
| `flopwire.recorded` | The binary path recorded for the plugins' hooks. See [How the hooks find the binary](#how-the-hooks-find-the-binary). |
| `agent.running` | The device agent answered. Messages and capture need it. setup never starts it. |
| `server.configured` | A server is configured. Without one, messages go only between the sessions on this device. |
| `harnesses[]` | One entry per harness: `detected`, `installed`, `enabled`, `version`, `scope`, `done` (what setup changed), `warnings`, `todo` (what you must still do), `error` and `skipped` (why setup left the harness alone). |
| `harnesses[].note` | How setup learned what it reports, when that limits the report. |
| `harnesses[].hook_binary` | The `flopwire` that the installed plugin's hooks run: `path` and `via` (`recorded path`, `PATH` or `known dir`), or `error` with the fix. `shell` says how setup ran the plugin's shim: as the harness runs a hook. |
| `harnesses[].hook_trust` | Codex only. `hooks`: the plugin hooks Codex found. `trusted`: how many you approved. `need_review`: the events whose hooks still need your approval. `disabled`: the events whose hooks you turned off. |
| `todo` | What you must still do for the device. |
| `check_note` | In `--check` mode, distinguishes configuration inspection from current-session tool access and hook delivery, which remain unverified. |

Use `--check` to report and change nothing. Use `--remove` to uninstall.
An installed and enabled plugin is configuration evidence. An observed MCP
process is process evidence. Neither proves that your current session can
discover or call a tool. Codex hook trust permits hook execution. It does
not grant permission to call MCP tools. The session's tool approval policy
still applies.

A stopped device agent prevents capture and live messaging. Existing local
history can still be searched. Shared search uses the configured server
and does not require a local index. An empty local index means no local
history has been indexed yet; an empty search result alone does not prove
that setup or permissions failed.

For Codex, `--check` reads local state only. See
[Codex](codex.md#what-setup---check-reads).
Run `flopwire setup --help` for every flag.

## Set up a device (for an agent)

Follow these steps in order.

1. Run `command -v flopwire`.
2. If the command prints nothing, stop. Tell your user to put the
   `flopwire` binary on `PATH`.
3. Run `flopwire setup --check`.
4. If `agent.running` is `false`, tell your user to start the device agent
   with `flopwire agent run` and keep it running. Do not start it
   yourself.
5. Run `flopwire setup`.
6. Read `error` and `skipped` in each entry of `harnesses`. Report each
   one to your user. If `ok` is `false`, stop.
7. Read `warnings` in each entry of `harnesses`. Show each warning to
   your user. A warning can name an older manual entry that now runs
   twice, or a hook that a plugin update enabled. Do not edit the settings
   file unless your user tells you to.
8. Read `todo` in each entry of `harnesses`, and the top-level `todo`.
   Tell your user each item.
9. If the Codex entry has a `hook_trust.need_review` list that is not
   empty, tell your user to approve the Flopwire hooks in Codex. See
   [Approve the Codex hooks](codex.md#approve-the-codex-hooks). Do not approve
   them yourself, and do not edit `~/.codex/config.toml`.
10. Tell your user to restart their Claude Code, Codex, Devin and opencode
    sessions. A running Claude Code session loads the plugin after a
    restart or after `/reload-plugins`. A running Codex or Devin session
    loads it after a restart.
11. Run `flopwire setup --check`.
12. Confirm that each detected harness has `installed: true` and
    `enabled: true`.
13. Confirm that each installed harness has a `hook_binary.path`. If it
    has `hook_binary.error`, tell your user the error. It names the fix.


## Verify tools in the current session

Use these checks after you address the report's `todo` items. They test the
session you run them in. They do not grant future send permission.

1. Start a fresh harness session, or reload its Flopwire MCP connection.
2. Ask the agent to list the available Flopwire tools. Confirm that
   `flopwire_grep`, `flopwire_read`, `flopwire_peers` and `flopwire_send`
   appear. Tool discovery alone does not verify tool access.
3. Choose a known indexed transcript and a distinctive phrase in it. Ask
   the agent to call `flopwire_grep` with that phrase and then
   `flopwire_read` with a returned address. Confirm the expected text.
   Check the reported search scope. If no history is indexed, capture a
   synthetic transcript first. An absent phrase is not a useful probe.
4. For live messaging, open a second test session. Keep both test sessions
   open throughout the messaging check. Identify the receiving session's
   full session ID. Explicitly authorize one test request/reply roundtrip.
5. Ask the first session to call `flopwire_peers` for that ID, then
   `flopwire_send` with the authorized test text. Review any tool approval
   prompt. If denied, report the denied tool and stop this check.
6. Run a turn in the receiving session. Confirm that its hook inserts the
   test message. A send receipt alone does not verify delivery to context.
7. Send the authorized reply from the receiving session with `reply_to`
   set to the test message ID. Run a turn in the first
   session and confirm that its hook inserts the reply.

Run `flopwire setup` again at any time. It updates the plugin when a
new version exists. It changes nothing when the plugin is current.

## What the hooks do

The harness hooks run `flopwire hook`. The command does two jobs:

- It prints the messages for this session into the session. See
  [Messaging](../messaging.md).
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

For how the hooks deliver messages, see
[Messaging](../messaging.md#how-a-hook-delivers-a-message).

## How the hooks find the binary

Every harness runs a hook command through a shell, and that shell's
`PATH` is often not your terminal's:

| Harness | Runs a hook command with | `PATH` |
|---|---|---|
| Claude Code | `/bin/sh -c` | The `PATH` Claude Code started with. A Claude Code started from the desktop has the desktop's. |
| Codex | Your login shell, `zsh -lc` (Codex 0.160) | What your login files (`~/.zprofile`, `/etc/zprofile`) set, not the `PATH` Codex started with. |
| Devin CLI | `bash -c` | What Devin read from your login shell. |
| opencode | No shell: the plugin starts `flopwire` itself | The `PATH` opencode started with. |

So a bare `flopwire` in a hook can find another binary than your
terminal does, or none. Each failure stops delivery. The hooks therefore
run a shim, `bin/flopwire-hook` in the Claude Code and Codex plugins
(Devin loads the Claude Code plugin). The shim takes the first of these:

1. The path in `<config dir>/binary-path`, if that file names an
   executable file. `flopwire setup` writes this binary's absolute path
   there, with symlinks resolved. Another `flopwire` command that you
   run writes it only when no binary is recorded or the recorded one is
   gone. So an upgrade that moves the binary corrects it at your next
   command, and a scratch or CI build never replaces your install.
   `flopwire hook`, `flopwire mcp`,
   `flopwire version`, `flopwire probe`, `flopwire agent flush`, the
   server commands and `flopwire setup --check` do not write it. The
   config directory is the directory of `FLOPWIRE_CONFIG`, else
   `~/Library/Application Support/flopwire` on macOS and
   `~/.config/flopwire` (or `$XDG_CONFIG_HOME/flopwire`) on Linux.
   When `flopwire` on the hook shell's `PATH` is a newer file than the
   recorded one (an old `go install` or a kept Homebrew keg still exists),
   the shim takes the one on `PATH`.
2. `flopwire` on the hook shell's `PATH`.
3. `flopwire` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin` or
   `~/.local/bin`.

The shim runs that binary with its own arguments and sets
`FLOPWIRE_HOOK_VIA` to `recorded`, `path` or `known`. When it finds no
binary, it writes one line to stderr that names the recorded path, the
`PATH` it searched and the fix (`flopwire setup`), and exits 1. When the
binary fails, for example a binary older than the plugin, it writes one
line that says to run `flopwire setup --check`, and exits 1. It never
exits 2, which blocks a prompt or a stop. The harness shows the failed
hook. The opencode plugin searches in the same order and writes the
same line.

The hook commands start the shim by the harness's plugin root:
`/bin/sh "${CLAUDE_PLUGIN_ROOT}/bin/flopwire-hook" hook` in Claude Code
and Devin, `/bin/sh "${PLUGIN_ROOT}/bin/flopwire-hook" hook` in Codex.
`/bin/sh` runs the file, so its mode does not matter.

The MCP server runs through the same shim, `flopwire-hook mcp`, so it
finds the same binary. Claude Code and Devin expand `${CLAUDE_PLUGIN_ROOT}`
in the plugin's `.mcp.json`. Codex 0.160 does not expand `${PLUGIN_ROOT}`
there, so its `.mcp.json` sets `cwd` to `.` (Codex resolves it to the
plugin directory) and runs `./bin/flopwire-hook mcp`. The shim `exec`s
the binary for the MCP server. A missing binary shows as a failed MCP
server in the harness.

`flopwire setup --check` runs each installed plugin's shim the way its
harness runs a hook: `/bin/sh -c` for Claude Code, `$SHELL -lc` for Codex
and Devin, and the shim's search in setup itself for opencode. It reports
the result as `hook_binary`, in the text form
`hook binary: <path> (via recorded path)`, or the shim's error. setup
compares the plugin with that binary, not with the one on `PATH`, and
warns when the binary lacks a command the plugin runs. A plugin from
before the shim gets a todo to update it.

The hook shim keeps one private, atomic
`hook-last-failure` receipt in the resolved Flopwire config directory.
It records only a UTC timestamp, a fixed reason (`missing-binary` or
`hook-exit`) and an exit status. It does not record stdin, arguments,
environment, message bodies or stderr. `flopwire setup --check` shows a
valid receipt once as a historical warning (`hook_failure_warning` in JSON). It may come from Claude Code,
Codex or Devin; it does not establish current hook health. A successful
hook or setup run keeps the receipt. Checks and MCP execution do not
write it. Unsafe or unwritable config directories can prevent recording;
this does not change the hook's original exit behavior.

`flopwire probe --as-installed` runs the plugins' own hook commands in
each harness and checks which binary they ran. See
[probe.md](../probe.md).

## Remove

1. Run `flopwire setup --remove`.
2. Run `flopwire setup --check`.
3. Confirm that each detected harness has `installed: false`.
4. Restart your Claude Code, Codex, Devin and opencode sessions.

`--remove` removes only what setup installed. It does not remove a Devin
plugin that you installed without `--local`, or a plugin from another
source. It reports each one in `warnings`.


## Where the plugin comes from

The repository is a plugin marketplace for Claude Code and Codex. Claude Code
reads `.claude-plugin/marketplace.json`; its plugin is in
`plugins/claude-code/flopwire`. Codex reads
`.agents/plugins/marketplace.json`; its plugin is in
`plugins/codex/flopwire`. Both marketplaces are named `flopwire-plugins`
(publisher-collection over product, like `openai-bundled/browser`), both
plugins are `flopwire@flopwire-plugins`, and each harness caches the
plugin at `plugins/cache/flopwire-plugins/flopwire/<version>`. The Go
constants `marketplaceName` and `pluginName` in `cmd/flopwire/setup.go`
must match the manifests; `TestClaudeMarketplaceManifest` and
`TestCodexMarketplaceManifest` check that. `flopwire setup` adds the marketplace
from `flopwire/flopwire` on GitHub. Claude Code installs the plugin at user
scope; Codex has only user installs. Use `--source` or
`FLOPWIRE_PLUGIN_SOURCE` to install from a local checkout. See the
[Claude Code plugin README](../../plugins/claude-code/flopwire/README.md) and
the [Codex plugin README](../../plugins/codex/flopwire/README.md).

The source's Claude Code marketplace must be named `flopwire-plugins`. When it
has another name, Claude Code adds the marketplace under that name.
setup names it in `done`, sets `error`, and installs nothing from it.
When setup added that marketplace, setup removes it again. When a
marketplace with that name was already configured, setup keeps it. Run
`claude plugin marketplace remove NAME` to remove it.

Devin CLI has no marketplace of its own. setup installs the Claude Code
plugin into Devin directly. See [Devin CLI](devin.md#install).
opencode loads one plugin file. See [opencode](opencode.md#install).
