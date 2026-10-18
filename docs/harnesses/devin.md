# Devin CLI

Devin CLI loads the Claude Code plugin as it is. `flopwire setup` installs
that plugin into Devin. The plugin delivers messages through hooks, serves
the MCP tools and adds the messaging skill. Devin has no hook approval step.

## Install

Run `flopwire setup`. See
[Install into the harnesses](README.md#install-into-the-harnesses).

Devin CLI has no marketplace of its own: `devin plugins install` takes
one plugin source. Given the root of a Claude Code marketplace
repository, Devin 3000.10.21 and later recognizes the marketplace and
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
  logged in, setup skips Devin: the Devin entry has `skipped: not logged
  in to Devin`, and `ok` and the exit status do not change. Run
  `devin auth login`, then run setup again.
- `devin plugins update` enables the hooks of a new plugin version without
  a prompt. setup compares the plugin's hooks before and after the update.
  It reports each hook that the update enabled in `warnings`, and each hook
  that the update removed in `done`.

Devin reads hooks from Claude Code's settings files, but not from Claude
Code's plugins. The Claude Code plugin and the Devin plugin therefore never
run in the same Devin session.

Devin caches the plugin. An update of only the Flopwire binary does not
replace the cached shim. Run `flopwire setup` to install or update the
plugin through Devin, then start a new Devin session.

## Hooks

The plugin runs `flopwire hook` through its shim on
`SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and
`SessionEnd`, and serves the MCP tools. Devin sets `CLAUDE_PLUGIN_ROOT`
for the plugin's hooks. When the shim finds no `flopwire`, or
`flopwire hook` fails (a binary older than the plugin), the hook exits 1
with one line on stderr that names the fix. Devin does not show that
line, in the TUI or in print mode. It writes only `Command exited with code 1` to its
own log, and the session continues without messages. After you update the
plugin or the binary, run `flopwire setup --check`: it reports a binary on
`PATH` that lacks a command the plugin runs.

To find a past failure, read the hook failure receipt that `flopwire setup --check` reports. See
[How the hooks find the binary](README.md#how-the-hooks-find-the-binary).

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

## Manual configuration

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

## How Devin holds sessions

This describes Devin CLI 3000.11.1.


- The TUI runs each session in a child `devin acp` process. That process
  writes the session's lock and keeps the lock file open, with an
  exclusive `flock`. `/clear` and `/resume` start a new child for the next
  session. The old child exits, and the old session's `SessionEnd` hook
  runs (reason `clear` or `resume`).
- One `devin acp` process holds several sessions when an ACP client calls
  `session/new` more than once. Each session has its own lock that names
  the same pid. ACP has no `session/close`.
  `session/delete` closes the session's lock file and removes the session
  from `sessions.db`. The lock file stays, and no `SessionEnd` hook runs.
  The agent reads both signals: `peers` drops the deleted session within
  about 1 second and keeps the other session live.
- An interrupted turn runs no `Stop` hook. `peers` shows it idle at the
  next presence check from the store's marker, about 1 second after
  Esc twice, Ctrl-C or an ACP `session/cancel`.
- Esc during a foreground `run_subagent` call cancels the call (its
  result is `Canceled due to user interrupt`). The subagent continues in
  the background, and the TUI stays in its working state: it queues new
  prompts until the subagent ends. `peers` shows the session idle for
  that time: the call's result is an interrupt marker, and the hooks of
  the background subagent are not the session's own, so they do not mark
  it busy.
- When the `devin acp` process dies while a subagent runs, the
  `run_subagent` call keeps no result. `devin -r` continues the session
  from the call's parent, so the next node is the call's sibling. The
  hook then treats the call as abandoned, and the session's `Stop` makes
  it idle.
