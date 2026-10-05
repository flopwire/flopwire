# opencode

Flopwire connects to opencode through one plugin file. It does everything
that the hooks and the MCP server do for the other harnesses:

| Part | What it does |
|---|---|
| Tools | `flopwire_grep`, `flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`, served as plugin tools. Each call runs `flopwire mcp --call` as the calling session. |
| Delivery | Puts pending messages into the session after each tool call and when the user sends a prompt. |
| Standing instruction | Adds Flopwire's instruction and tool guidance to the system prompt of every model call. |
| Presence | Names the process's sessions in `<flopwire config dir>/opencode/<pid>.json`, so peers sees them as live. |
| Capture | Tells the device agent when a turn starts and ends. The agent reads `opencode.db` itself ([agent.md](agent.md)). |
| Shell | Sets `FLOPWIRE_SESSION_ID` and `FLOPWIRE_AGENT` for shell commands, so `flopwire send` in a shell knows the session. |

## Requirements

- A `flopwire` binary the plugin can find. It takes the path
  `flopwire setup` recorded, then `flopwire` on `PATH`, then
  `/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin` and `~/.local/bin`,
  as the other plugins' hook shim does
  ([agent.md](agent.md#how-the-hooks-find-the-binary)).
- The device agent runs (`flopwire agent run`).
- opencode 1.18 or later.

If the plugin finds no `flopwire`, it writes one line to stderr that
names what it searched and the fix (`flopwire setup`), does nothing more
and adds no tools. If the agent is not running, no message arrives and the messaging
tools return `agent_not_running`.

## Install

1. Run `flopwire setup`.
2. Read the report. Do each `todo` item.
3. Restart your opencode sessions. opencode loads plugins at start.

opencode loads every file in its global plugin directory. `flopwire setup`
writes the plugin to `~/.config/opencode/plugins/flopwire.js`
(`$XDG_CONFIG_HOME/opencode/plugins` when that variable is set). It does
not edit `opencode.json`. opencode installs `@opencode-ai/plugin`, which
the plugin imports, into its config directory when it starts.

`flopwire setup --check` reports whether the file is there and whether it
is the plugin of the `flopwire` binary that runs the check.

## Update

1. Run `flopwire setup` again.
2. Restart your opencode sessions.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your opencode sessions.

setup writes and deletes only a file that starts with the Flopwire
header. If another file has the name `flopwire.js`, setup reports an error
and changes nothing.

Do not also add `flopwire mcp` as an MCP server in `opencode.json`. An MCP
server in opencode cannot tell which session calls it, so its messaging
tools fail. setup warns when it finds such an entry.

## How a message arrives

A message never starts a turn.

- **The session runs a turn.** After the next tool call, the plugin
  stores the message in the session with `promptAsync` and `noReply`.
  opencode answers it with one more reply before the session goes idle.
  The model does not see the message in the step that is already running:
  opencode reads the stored message only after the current model stream
  ends. This extra reply is accepted (decided 2026-10-04). The other
  harnesses add the message to the running turn and need no extra reply.
- **The session is idle.** The message waits. When the user sends the next
  prompt, the plugin adds the message to that prompt as one more text
  part. The model reads both in the same turn.

Each message is one text part. The part's text is the message wrapper, and
its metadata is `{"flopwire": {"id": "<message id>"}}`. opencode shows the
part in the conversation. The plugin confirms the delivery only after
opencode has stored the part: it gives each part an id and confirms the
message when opencode reports that part stored (`message.part.updated`).
`promptAsync` answers before opencode stores anything, so its answer does
not count. If opencode does not store the part (`promptAsync` fails, or
opencode refuses the message after it answered), the message stays leased
and comes again, marked `redelivery="true"`, after the lease ends.

`promptAsync` without `noReply` would start a turn in an idle session, so
the plugin never calls it that way.

## Read receipts

The device agent reads `opencode.db`. A text part counts as a delivery
when its metadata names a message id and its text starts with the wrapper
of that same id. The message then becomes `read` (see
[messaging.md](messaging.md#read-receipts)). The same wrapper in a prompt
that you typed, or in a reply, does not count.

## Subagents

opencode runs a subagent in a child session. The plugin never delivers a
message into a child session. The message waits for the parent session's
next tool call. A child session is not a peer. A tool call from a
subagent runs as its parent session: a `flopwire_send` from a subagent
goes out as the parent session, and the reply goes to the parent session.
The parent's human does not see the call. This is the same on every
harness (decided 2026-10-04); see [agent.md](agent.md#send-and-read-messages).

## Presence

A session is live while an opencode process with the plugin holds it. The
plugin adds a session to its process's file at the session's first event
in that process, and removes it when you delete the session. When the
process exits normally, the plugin deletes the file. When a process ends
by a signal, its file stays: the agent sees that the pid is gone, or runs
another program, and ends the session. The next opencode that starts
deletes the file.

Busy or idle is the session's last plugin event.

## Known limits

- An open session becomes live only at its first event in the process,
  for example a prompt. A session that you only open in the TUI after a
  restart is not live until then.
- The device agent finds changed rows by their `time_updated` and looks
  2 seconds back. If two processes write the same session in the same
  millisecond and one write commits more than 2 seconds after the other,
  the agent can miss that row until the session changes again.
- opencode's experimental `session_message` table (the `/api/session`
  API) is not read.
- The held-message notice is not shown in opencode.
- `flopwire probe` drives `opencode serve`, not the TUI.
