# Claude Code

Flopwire installs a Claude Code plugin. The plugin delivers messages
through hooks, serves the MCP tools and adds the `flopwire:messaging` skill.
Devin CLI loads the same plugin ([devin.md](devin.md)).

## What the plugin installs

| Part | What it does |
|---|---|
| MCP server `flopwire` | Runs `flopwire mcp` through the shim `bin/flopwire-hook`: the [nine retrieval and messaging tools](README.md#what-agents-discover). |
| Hooks | Run `flopwire hook` through the shim on `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, with a 5-second timeout. They print the standing instruction and pending messages into the session, tell the device agent when the session ends, and ask it to index the transcript. |
| Skill `flopwire:messaging` | Tells the model when and how to recover prior work from past or ongoing agent conversations, and how to contact a live session. |

Claude Code names the plugin's MCP server `plugin:flopwire:flopwire`, so
its tools show as `mcp__plugin_flopwire_flopwire__*`. The skill's name and
description are available for discovery in every session. Its body loads
only when the model uses it. `claude plugin details flopwire` shows the
current numbers.

## Requirements

- A `flopwire` binary the shim can find: the path `flopwire setup`
  recorded, `PATH` or a known directory. See
  [How the hooks find the binary](README.md#how-the-hooks-find-the-binary).
- The device agent runs (`flopwire agent run`). Messages and capture go
  through it. See [agent.md](../agent.md).

## Install

1. Run `flopwire setup`. See
   [Install into the harnesses](README.md#install-into-the-harnesses).
2. Read the report. Do each `todo` item.
3. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` runs Claude Code's own commands at user scope. You can
run them yourself instead:

```sh
claude plugin marketplace add flopwire/flopwire --scope user --sparse .claude-plugin plugins
claude plugin install flopwire@flopwire-plugins --scope user
```

## Update

1. Run `flopwire setup` again.
2. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` refreshes the marketplace and runs
`claude plugin update flopwire@flopwire-plugins --scope user`. The plugin
has no pinned version, so Claude Code versions it by the repository's
commit. When the plugin is disabled, setup adds a `todo` with the
`claude plugin enable` command.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your Claude Code sessions.

`flopwire setup --remove` runs
`claude plugin uninstall flopwire@flopwire-plugins` and
`claude plugin marketplace remove flopwire-plugins`. It keeps the
marketplace while Claude Code still has a plugin from it installed in
another scope or project, since removing a marketplace uninstalls every
plugin from it. Run `flopwire setup --check` to confirm.

## Failures

- The shim finds no `flopwire`: each hook exits 1 with one line on
  stderr that names the recorded path, the `PATH` it searched and the
  fix, `flopwire setup`. Claude Code shows it as a non-blocking hook
  error. The MCP server shows as failed in `/mcp`.
- `flopwire hook` fails, for example because the binary is older than the
  plugin: the hook exits 1 with a line that says to run
  `flopwire setup --check`. That command shows which binary the hooks
  find (`hook binary:`) and names the commands that binary lacks.
- The device agent is not running: the hooks print nothing and the
  messaging tools return `agent_not_running`.

A hook never exits 2, so it never blocks a prompt or a stop.

## Manual configuration

Use the manual configuration below only when you cannot install the
plugin. Do not use both: each hook would then run twice. `flopwire setup`
names each manual entry it finds next to the plugin, including a
`flopwire agent flush` hook or a `claude mcp add flopwire` server. Remove
them yourself; setup does not edit your settings files. The manual hooks
run `flopwire` from the hook shell's `PATH`.

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

## Development

The plugin is in `plugins/claude-code/flopwire`. Point `flopwire setup`
at a checkout to install it from there:

```sh
flopwire setup --source /path/to/flopwire
```

`FLOPWIRE_PLUGIN_SOURCE` sets the same default. A marketplace added from a
local directory loads the plugin in place, so edits take effect at the
next session start or `/reload-plugins`. To load the plugin for one
session only, without installing it:

```sh
claude --plugin-dir /path/to/flopwire/plugins/claude-code/flopwire
```

Devin CLI loads this plugin too. Keep the plugin free of fields that
Devin would read differently. A change to `hooks/hooks.json` or
`.mcp.json` changes both harnesses. The Codex plugin has copies of the
skill and the shim; see [Codex development](codex.md#development).

The source's Claude Code marketplace must be named `flopwire-plugins`.
When it has another name, Claude Code adds the marketplace under that
name. setup names it in `done`, sets `error`, and installs nothing from
it. When setup added that marketplace, setup removes it again. When a
marketplace with that name was already configured, setup keeps it. Run
`claude plugin marketplace remove NAME` to remove it.
