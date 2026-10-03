# Flopwire plugin for Claude Code

The plugin connects Claude Code to Flopwire. It adds these parts:

| Part | What it does |
|---|---|
| MCP server `flopwire` | Runs `flopwire mcp`: the tools `flopwire_grep`, `flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`. |
| Hooks | Run `flopwire hook` on `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, with a 5-second timeout. They print the standing instruction and pending messages into the session, tell the device agent when the session ends, and ask it to index the transcript. |
| Skill `flopwire:messaging` | Tells the model how to find the session behind a change, check that it is live, and write a message to it. |

Devin CLI loads this plugin too. `flopwire setup` installs this directory
into Devin with `devin plugins install --local`; see
[docs/agent.md](../../../docs/agent.md#where-the-plugin-comes-from). Keep
the plugin free of fields that Devin would read differently. A change to
`hooks/hooks.json` or `.mcp.json` changes both harnesses.

The plugin adds about 90 tokens to every session (the skill's name and
description). The skill's body (about 640 tokens) loads only when the
model uses it. `claude plugin details flopwire` shows the current numbers.

## Requirements

- The `flopwire` binary is on `PATH`. The hooks and the MCP server run it
  by name.
- The device agent runs (`flopwire agent run`). Messages and capture go
  through it. See [docs/agent.md](../../../docs/agent.md).

If `flopwire` is not on `PATH`, the hooks do nothing and show no error
(otherwise Claude Code would show a hook error on every tool call), and the
MCP server shows as failed in `/mcp`. `flopwire setup` reports the missing
binary. If `flopwire hook` fails, for example because the binary is older
than the plugin and has no `hook` command, each hook exits 1 and Claude
Code shows a non-blocking hook error that says to run
`flopwire setup --check`. That command names the commands the binary lacks
and both versions. A hook never exits 2, so it never blocks a prompt or a
stop. If the agent is not running, the hooks print nothing and the
messaging tools return `agent_not_running`. If the agent has never run,
`flopwire mcp` creates an empty index and the search tools say that
nothing is indexed yet.

## Install

1. Run `flopwire setup`.
2. Read the report. Do each `todo` item.
3. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` runs Claude Code's own commands at user scope:

```sh
claude plugin marketplace add flopwire/flopwire --scope user --sparse .claude-plugin plugins
claude plugin install flopwire@flopwire --scope user
```

You can run these two commands yourself instead.

## Update

1. Run `flopwire setup` again.
2. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` refreshes the marketplace and runs
`claude plugin update flopwire@flopwire --scope user`. The plugin has no
pinned version, so Claude Code versions it by the repository's commit.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your Claude Code sessions.

`flopwire setup --remove` runs `claude plugin uninstall flopwire@flopwire`
and `claude plugin marketplace remove flopwire`. Run
`flopwire setup --check` to confirm. It keeps the marketplace while
Claude Code still has a plugin installed from it in another scope or
project, since removing a marketplace uninstalls every plugin from it.

setup trusts only a `flopwire` marketplace from its source (`--source`,
default `flopwire/flopwire`). If a marketplace with that name comes from
anywhere else, setup installs, updates and removes nothing through it,
reports an error, and names the commands to switch. Pass the same
`--source` to `--remove` that you passed to install.

## Older manual setup

Before the plugin, the docs told you to add `flopwire hook` or
`flopwire agent flush` hooks to `~/.claude/settings.json`, and to run
`claude mcp add flopwire -- flopwire mcp`. With the plugin installed, those
entries run a second time. `flopwire setup` names each one it finds. Remove
them yourself; `flopwire setup` does not edit your settings files.

## Development

Point `flopwire setup` at a checkout to install the plugin from it:

```sh
flopwire setup --source /path/to/flopwire
```

`FLOPWIRE_PLUGIN_SOURCE` sets the same default. A marketplace added from a
local directory loads the plugin in place, so edits take effect at the next
session start or `/reload-plugins`. To load the plugin for one session
only, without installing it:

```sh
claude --plugin-dir /path/to/flopwire/plugins/claude-code/flopwire
```
