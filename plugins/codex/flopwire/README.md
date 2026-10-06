# Flopwire plugin for Codex

The plugin connects Codex to Flopwire. It adds these parts:

| Part | What it does |
|---|---|
| MCP server `flopwire` | Runs `flopwire mcp` through the shim `bin/flopwire-hook`: the tools `flopwire_grep`, `flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`. |
| Hooks | Run `flopwire hook` through the shim `bin/flopwire-hook` on `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and `SessionEnd`, with a 5-second timeout (3 seconds for `SessionEnd`, the most Codex allows). The `Stop` hook is `async`, so Codex ignores text that your shell's startup files print, unless that text starts with `{` or `[`. They print the standing instruction and pending messages into the session, tell the device agent when the session ends, and ask it to index the transcript. |
| Skill `flopwire:messaging` | Tells the model how to find the session behind a change, check that it is live, and write a message to it. The text is the same as in the Claude Code plugin. |

## Requirements

- A `flopwire` binary the shim can find: the path `flopwire setup`
  recorded, `PATH` or a known directory. The hooks and the MCP server
  both run it through the shim.
- The device agent runs (`flopwire agent run`). Messages and capture go
  through it. See [docs/agent.md](../../../docs/agent.md).
- You approve the plugin's hooks once in Codex. See below.

## How the hooks find flopwire

Codex runs each hook command in your login shell (`zsh -lc` in Codex
0.160), so a bare `flopwire` resolves through the `PATH` your login files
set, not the `PATH` Codex started with. Each hook runs
`/bin/sh "${PLUGIN_ROOT}/bin/flopwire-hook" hook`. The shim runs the
first of these:

1. The binary path that `flopwire setup` recorded in
   `<config dir>/binary-path`. A `flopwire` command you run updates it
   when the recorded binary is gone. A newer `flopwire` on the hook's
   `PATH` wins over an older recorded one.
2. `flopwire` on the hook's `PATH`.
3. `flopwire` in `/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin` or
   `~/.local/bin`.

If it finds none, or the binary fails (for example one too old to know
`flopwire hook`), the hook exits 1 with one line on stderr that names the
fix, and Codex reports a failed hook. It never exits 2.
`flopwire setup --check` shows which binary the hooks find
(`hook binary:`). See
[docs/agent.md](../../../docs/agent.md#how-the-hooks-find-the-binary).

If the agent is not running, the hooks print nothing and the messaging
tools return `agent_not_running`. After enrollment, the search tools use the shared
server by default and need no local index. Before enrollment, they use the
local index and return "no index yet" until the agent builds it. CLI queries can use
`--local` to select this device. A server failure returns an error; it
does not switch to local search.

## Install

1. Run `flopwire setup`.
2. Read the report. Do each `todo` item.
3. Approve the hooks (next section).
4. Restart your Codex sessions.

`flopwire setup` runs Codex's own commands:

```sh
codex plugin marketplace add flopwire/flopwire --sparse .agents/plugins --sparse plugins/codex
codex plugin add flopwire@flopwire-plugins
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
4. Trust the five Flopwire hooks.
5. Run `flopwire setup --check`.
6. Confirm that the Codex entry shows `hook_trust.trusted: 5`.

You can also type `/hooks` in a running session.

Each hook runs the shim outside the Codex sandbox. Codex asks again for a
hook when its event, matcher, command, timeout, `async` flag,
`statusMessage`, `additionalContextLimit` or position in its event's list
changes. Flopwire changes these only when it must. A plugin update that
changes a hook asks once more, for that hook. The update that added the
shim changed all five commands from `flopwire hook || true`, so Codex
asks once more for all five. Codex hashes the command with
`${PLUGIN_ROOT}` unexpanded, so a reinstall to another path does not ask
again.

Codex asks before each `flopwire_send` call. `codex exec` cannot ask. To
allow the tool without a question, add this to `~/.codex/config.toml`:

```toml
[plugins."flopwire@flopwire-plugins".mcp_servers.flopwire.tools.flopwire_send]
approval_mode = "approve"
```

## Shell commands and git commits

A shell command in Codex's default sandbox (no network access) cannot
reach the device agent: `flopwire send` fails with `sandbox_blocked`. Use
the `flopwire_send` tool. The same sandbox keeps `.git` read-only, so
`git commit` fails there, also in a linked git worktree. See
[docs/agent.md](../../../docs/agent.md#codex-and-git-commits) for what was
verified on each OS and the workarounds.

## Update

1. Run `flopwire setup` again.
2. Restart your Codex sessions.

`flopwire setup` refreshes the marketplace
(`codex plugin marketplace upgrade flopwire-plugins`) and runs
`codex plugin add flopwire@flopwire-plugins` again, which copies the current files
into Codex's plugin cache. The plugin has no pinned version, so Codex
lists it as `local`.

A disabled plugin stays disabled: `codex plugin add` would enable it, so
setup skips the update and tells you.

## Remove

1. Run `flopwire setup --remove`.
2. Restart your Codex sessions.

`flopwire setup --remove` runs `codex plugin remove flopwire@flopwire-plugins` and
`codex plugin marketplace remove flopwire-plugins`. It keeps the marketplace while
another plugin from it is installed. The hook approvals stay in
`~/.codex/config.toml`, so a later install does not ask again.

## Upgrading an existing install

The marketplace was named `flopwire` before issue #162, so older installs
have `flopwire@flopwire` from the marketplace `flopwire`. There is no
upgrade path (pre-release): `flopwire setup --check` names the stale
marketplace and the commands below; run them, then install again:

```sh
codex plugin remove flopwire@flopwire
codex plugin marketplace remove flopwire
flopwire setup
```

The hook approvals are keyed by plugin id, so approve the hooks once more
(the "Hooks need review" prompt, or `/hooks`), then restart your Codex
sessions.

setup trusts only a `flopwire-plugins` marketplace from its source (`--source`,
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
and the shim in `bin/flopwire-hook` are copies of the Claude Code
plugin's; edit those and copy them here. A test fails when they differ.
