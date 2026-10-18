# Codex

Flopwire installs a Codex plugin. The plugin runs `flopwire hook` through
its shim on `SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and
`SessionEnd`, and serves the MCP tools. Codex runs the hooks only after you
approve them.

## Install

1. Run `flopwire setup`. See
   [Install into the harnesses](README.md#install-into-the-harnesses).
2. Approve the hooks. See [Approve the Codex hooks](#approve-the-codex-hooks).

setup runs `codex plugin marketplace add flopwire/flopwire --json
--sparse .agents/plugins --sparse plugins/codex`, then
`codex plugin add flopwire@flopwire-plugins --json`. Codex then has these
entries:

- `config.toml` has `[marketplaces.flopwire-plugins]` with
  `source_type = "git"`, `source = "https://github.com/flopwire/flopwire.git"`
  and both sparse paths, and `[plugins."flopwire@flopwire-plugins"]` with
  `enabled = true`.
- Codex copies the plugin to
  `plugins/cache/flopwire-plugins/flopwire/local` and lists five plugin
  hooks, all waiting for review.

A second `flopwire setup` runs `codex plugin marketplace upgrade
flopwire-plugins` and reinstalls the plugin.

## What setup --check reads

For Codex, `--check` runs only `codex plugin marketplace list` and
`codex plugin list --marketplace flopwire-plugins`, which read local state. It
does not start `codex app-server`: the app server upgrades the
configured plugin marketplaces and fetches the plugin catalog in the
background when it starts. Instead, `--check` reads Codex's files in
`$CODEX_HOME` (default `~/.codex`): `config.toml`, `hooks.json` and the
plugin's cached hooks file. It computes each plugin hook's hash as Codex
0.160.0 does and compares it with the `trusted_hash` that Codex stored
when you approved the hook. When Codex is newer than 0.160.0 and a hook
looks unapproved, `--check` warns that Codex may have changed its hash and
says to run `flopwire setup`, which asks Codex. It looks for older manual Flopwire entries in
the user config only, not in a project's `.codex` folder. The Codex entry
says this in `note`. `flopwire setup` without `--check` asks the app
server.

## Approve the Codex hooks

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
   runs `/bin/sh "${PLUGIN_ROOT}/bin/flopwire-hook" hook`.
5. Run `flopwire setup --check`.
6. Confirm that the Codex entry shows `hook_trust.trusted: 5`.

You can also type `/hooks` in a running Codex session to review the hooks.
"Trust all and continue" also trusts every other hook that waits for
review.

What you approve: each hook runs the plugin's shim,
`/bin/sh "${PLUGIN_ROOT}/bin/flopwire-hook" hook`, outside the Codex
sandbox. The shim finds the `flopwire` binary and runs `flopwire hook`
(see [How the hooks find the binary](README.md#how-the-hooks-find-the-binary)).
The timeout is 5 seconds, and 3 seconds for `SessionEnd`, the most that
Codex allows for that event. The `Stop` hook is `async`: Codex does not
wait for it. `flopwire hook` reads the hook input, asks the device agent
for this session's messages, prints them into the session, and asks the
agent to index the transcript. When the shim finds no binary, or the
binary fails, the hook exits 1 with one line on stderr, and Codex reports
a failed hook. It never exits 2, which would block the turn.

Codex asks again for a hook when any of these change: its event, its
matcher, its command, its timeout, its `async` flag, its `statusMessage`,
its `additionalContextLimit`, or its position in the list of hooks for its
event. Codex stores the approval under a key that holds the event and the
position, and a hash of the other fields. A plugin update that adds,
removes or reorders hooks therefore asks again for each hook that moved.
Flopwire changes these only when it must. A plugin update that changes a
hook asks once more, for that hook. Codex hashes the command as written,
with `${PLUGIN_ROOT}` unexpanded, so a reinstall to another path does not
ask again. Until you trust a new `SessionEnd`
hook, a Codex session that exits leaves presence when its writer lock is
released, which the agent notices within seconds. Codex records the
approval in `~/.codex/config.toml` under `hooks.state`, keyed by the
plugin and the hook, not by the install path. After
`flopwire setup --remove` the approval stays there, so a later install
does not ask again.

Codex also asks before each `flopwire_send` call, because a message leaves
the session. `codex exec` cannot ask, so the call fails there. To allow
`flopwire_send` without a question, add this to `~/.codex/config.toml`
yourself:

```toml
[plugins."flopwire@flopwire-plugins".mcp_servers.flopwire.tools.flopwire_send]
approval_mode = "approve"
```

A shell command in Codex's sandbox reaches the device agent only when
the sandbox has network access. By default (`workspace-write` without
`network_access`, and `read-only`) the sandbox refuses the connect to the
agent's socket, and `flopwire send` fails with `sandbox_blocked`. Use the
`flopwire_send` tool in Codex: the MCP server runs outside the sandbox.
With `sandbox_workspace_write.network_access = true` in
`~/.codex/config.toml`, `flopwire send` in a shell command works.

The sandbox refuses the connect on macOS and Linux:

| OS | Sandbox | Network off | `network_access = true` |
|---|---|---|---|
| macOS | Seatbelt | `connect` fails with `EPERM`; `sandbox_blocked` | `connect` works |
| Linux | bubblewrap and seccomp | `connect` fails with `EPERM` (seccomp denies `connect`); `sandbox_blocked` | `flopwire send` reaches the agent |

`flopwire send` reports `sandbox_blocked` when the connect fails with
`EPERM`, or with `EACCES` in a shell that Codex marks as sandboxed
(`CODEX_SANDBOX_NETWORK_DISABLED` or `CODEX_SANDBOX` is set). A plain
`EACCES` outside a sandbox is `permission_denied`: the socket belongs to
another user, or its mode changed.

Codex's sandbox also keeps `.git` read-only, so `git commit` fails in
a sandboxed shell command. See [Codex and git commits](#codex-and-git-commits).

## Hooks

The plugin's `Stop` hook asks the agent to index each finished turn. It
prints nothing and never blocks or extends a turn. Do not also set
`notify = ["flopwire", "agent", "flush"]`: the `Stop` hook does that job,
and `notify` holds one command only, so it stays free for other tools.

The `Stop` hook is `async`. Codex runs each hook command through your
shell: `zsh -c` reads `~/.zshenv`, `bash -c` reads the file that
`BASH_ENV` names, and when Codex knows no shell it runs `$SHELL -lc`,
which reads your login files. Text that these files print goes into the
hook's output before `flopwire hook` runs. Codex fails a synchronous
`Stop` hook whose output is not JSON, on every turn. It ignores the
output of an async `Stop` hook unless that output starts with `{` or `[`.
At exit, Codex drops the async `Stop` hook of the last turn; the
`SessionEnd` hook indexes the transcript and ends the session then.
Keep your shell startup files silent when they are not interactive:
on the other events the printed text goes into the session as context.

Codex 0.160 runs hook commands with `zsh -lc`, so a bare `flopwire`
resolves through the `PATH` your login profile sets, not the `PATH` Codex
started with. The plugin's shim tries the recorded binary path first (see
[How the hooks find the binary](README.md#how-the-hooks-find-the-binary)). An
isolated test must set `FLOPWIRE_CONFIG` to a directory whose
`binary-path` names the binary under test, or the shim runs the binary
your own setup recorded.

## Codex and git commits

Codex's `workspace-write` sandbox keeps `.git` read-only, so a sandboxed
`git commit` fails in every checkout. In a linked git worktree, the
worktree's `.git` file points to `<main>/.git/worktrees/<name>/`, where
git writes `index.lock`, and the commit also writes objects and refs in
`<main>/.git`. Adding `<main>/.git/worktrees/<name>` as a writable root
does not help: the commit still fails. This applies on macOS (Seatbelt)
and Linux (bubblewrap).

This limits which harness can be the one that commits in a setup with
worktrees. To commit:

- Commit from the main checkout, outside the Codex sandbox: in your own
  terminal, or from a Claude Code or Devin session.
- Or approve Codex's request to run `git commit` outside the sandbox.
  Interactive Codex asks for it; `codex exec` cannot ask, so the commit
  fails there.
- Or run Codex with the sandbox off: `codex --sandbox danger-full-access`.
  The model's commands then run without any sandbox.

## Manual configuration

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
      { "hooks": [{ "type": "command", "command": "flopwire hook", "timeout": 5, "async": true }] }
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
event, the matcher, the command, the timeout, `async`, `statusMessage`,
`additionalContextLimit` or the hook's position in the list changes.
