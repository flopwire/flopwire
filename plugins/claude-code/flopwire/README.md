# Flopwire plugin for Claude Code

The plugin connects Claude Code to Flopwire. It adds these parts:

| Part | What it does |
|---|---|
| MCP server `flopwire` | Runs `flopwire mcp` through the shim `bin/flopwire-hook`: the tools `flopwire_grep`, `flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`. |
| Hooks | Run `flopwire hook` through the shim `bin/flopwire-hook` on `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, with a 5-second timeout. They print the standing instruction and pending messages into the session, tell the device agent when the session ends, and ask it to index the transcript. |
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

- A `flopwire` binary the shim can find: the path `flopwire setup`
  recorded, `PATH` or a known directory. The hooks and the MCP server
  both run it through the shim.
- The device agent runs (`flopwire agent run`). Messages and capture go
  through it. See [docs/agent.md](../../../docs/agent.md).

## How the hooks find flopwire

Claude Code runs each hook command with `/bin/sh -c` and the `PATH` it
started with, which is not your terminal's when you start Claude Code from
the desktop. So each hook runs
`/bin/sh "${CLAUDE_PLUGIN_ROOT}/bin/flopwire-hook" hook`. The shim runs
the first of these:

1. The binary path that `flopwire setup` recorded in
   `<config dir>/binary-path`. A `flopwire` command you run updates it
   when the recorded binary is gone. A newer `flopwire` on the hook's
   `PATH` wins over an older recorded one.
2. `flopwire` on the hook's `PATH`.
3. `flopwire` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin` or
   `~/.local/bin`.

If it finds none, each hook exits 1 with one line on stderr that names
the recorded path, the `PATH` it searched and the fix, `flopwire setup`.
Claude Code shows it as a non-blocking hook error. If `flopwire hook`
fails, for example because the binary is older than the plugin and has no
`hook` command, the hook exits 1 with a line that says to run
`flopwire setup --check`. That command shows which binary the hooks find
(`hook binary:`), and names the commands that binary lacks. A hook never
exits 2, so it never blocks a prompt or a stop. See
[docs/agent.md](../../../docs/agent.md#how-the-hooks-find-the-binary).

If the shim finds no `flopwire`, the MCP server shows as failed in `/mcp`. If the agent is not running, the hooks print nothing and the
messaging tools return `agent_not_running`. After enrollment, the search tools use the shared
server by default and need no local index. Before enrollment, they use the
local index and report when it has no transcripts. CLI queries can use
`--local` to select this device. A server failure returns an error; it
does not switch to local search.

## Install

1. Run `flopwire setup`.
2. Read the report. Do each `todo` item.
3. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` runs Claude Code's own commands at user scope:

```sh
claude plugin marketplace add flopwire/flopwire --scope user --sparse .claude-plugin plugins
claude plugin install flopwire@flopwire-plugins --scope user
```

You can run these two commands yourself instead.

## Update

1. Run `flopwire setup` again.
2. Restart your Claude Code sessions, or run `/reload-plugins` in each.

`flopwire setup` refreshes the marketplace and runs
`claude plugin update flopwire@flopwire-plugins --scope user`. The plugin has no
pinned version, so Claude Code versions it by the repository's commit.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your Claude Code sessions.

`flopwire setup --remove` runs `claude plugin uninstall flopwire@flopwire-plugins`
and `claude plugin marketplace remove flopwire-plugins`. Run
`flopwire setup --check` to confirm. It keeps the marketplace while
Claude Code still has a plugin installed from it in another scope or
project, since removing a marketplace uninstalls every plugin from it.

## Upgrading an existing install

The marketplace was named `flopwire` before issue #162, so older installs
have `flopwire@flopwire` from the marketplace `flopwire`. There is no
upgrade path (pre-release): `flopwire setup --check` names the stale
marketplace and the commands below; run them, then install again:

```sh
claude plugin marketplace remove flopwire --scope user
flopwire setup
```

Then restart your Claude Code sessions.

setup trusts only a `flopwire-plugins` marketplace from its source (`--source`,
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
