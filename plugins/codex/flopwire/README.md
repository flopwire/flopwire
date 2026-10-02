# Flopwire plugin for Codex

The plugin connects Codex to Flopwire. It adds these parts:

| Part | What it does |
|---|---|
| MCP server `flopwire` | Runs `flopwire mcp`: the tools `flopwire_grep`, `flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`. |
| Hooks | Run `flopwire hook \|\| true` on `SessionStart`, `UserPromptSubmit`, `PostToolUse` and `Stop`, with a 5-second timeout. They print the standing instruction and pending messages into the session, and ask the device agent to index the transcript. |
| Skill `flopwire:messaging` | Tells the model how to find the session behind a change, check that it is live, and write a message to it. The text is the same as in the Claude Code plugin. |

## Requirements

- The `flopwire` binary is on `PATH`. The hooks and the MCP server run it
  by name.
- The device agent runs (`flopwire agent run`). Messages and capture go
  through it. See [docs/agent.md](../../../docs/agent.md).
- You approve the plugin's hooks once in Codex. See below.

If `flopwire` is not on `PATH`, or is too old to know `flopwire hook`, the
hooks do nothing: `|| true` keeps Codex from reporting a failed hook. If
the agent is not running, the hooks print nothing and the messaging tools
return `agent_not_running`. If the agent has never run on this device, the
search tools return "no index yet".

## Install

1. Run `flopwire setup`.
2. Read the report. Do each `todo` item.
3. Approve the hooks (next section).
4. Restart your Codex sessions.

`flopwire setup` runs Codex's own commands:

```sh
codex plugin marketplace add flopwire/flopwire --sparse .agents/plugins --sparse plugins/codex
codex plugin add flopwire@flopwire
```

You can run these two commands yourself instead.

## Approve the hooks

Codex runs a plugin's hooks only after you approve them. `flopwire setup`
does not approve them for you, and it does not edit
`~/.codex/config.toml`. Until you approve them, no message arrives in a
Codex session.

1. Start `codex` in a terminal.
2. Codex shows "Hooks need review".
3. Select "Review hooks".
4. Trust the four Flopwire hooks.
5. Run `flopwire setup --check`.
6. Confirm that the Codex entry shows `hook_trust.trusted: 4`.

You can also type `/hooks` in a running session.

Each hook runs `flopwire hook || true` outside the Codex sandbox. Codex
asks again only when a hook's event, matcher, command or timeout changes.
These values stay fixed, so updates of the plugin do not ask again.

Codex asks before each `flopwire_send` call. `codex exec` cannot ask. To
allow the tool without a question, add this to `~/.codex/config.toml`:

```toml
[plugins."flopwire@flopwire".mcp_servers.flopwire.tools.flopwire_send]
approval_mode = "approve"
```

## Update

1. Run `flopwire setup` again.
2. Restart your Codex sessions.

`flopwire setup` refreshes the marketplace
(`codex plugin marketplace upgrade flopwire`) and runs
`codex plugin add flopwire@flopwire` again, which copies the current files
into Codex's plugin cache. The plugin has no pinned version, so Codex
lists it as `local`.

A disabled plugin stays disabled: `codex plugin add` would enable it, so
setup skips the update and tells you.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your Codex sessions.

`flopwire setup --remove` runs `codex plugin remove flopwire@flopwire` and
`codex plugin marketplace remove flopwire`. It keeps the marketplace while
another plugin from it is installed. The hook approvals stay in
`~/.codex/config.toml`, so a later install does not ask again.

setup trusts only a `flopwire` marketplace from its source (`--source`,
default `flopwire/flopwire`). If a marketplace with that name comes from
anywhere else, setup installs, updates and removes nothing through it, and
reports an error.

## Older manual setup

Before the plugin, the docs told you to add `flopwire hook` hooks to
`~/.codex/hooks.json`, to set `notify = ["flopwire", "agent", "flush"]`,
and to add an `[mcp_servers.flopwire]` entry. With the plugin installed,
those entries run a second time. The plugin's `Stop` hook does the work of
the `notify` line. `flopwire setup` names each entry it finds. Remove them
yourself.

## Not tested

The Codex desktop app and the IDE extension.

## Development

Point `flopwire setup` at a checkout to install the plugin from it:

```sh
flopwire setup --source /path/to/flopwire
```

Codex copies the plugin into its cache at install. Run `flopwire setup`
again after you edit the plugin. The skill in `skills/messaging/SKILL.md`
is a copy of the Claude Code plugin's skill; edit that one and copy it
here. A test fails when the two differ.
