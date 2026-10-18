# Claude Code

Flopwire installs a Claude Code plugin. The plugin delivers messages
through hooks, serves the MCP tools and adds the `flopwire:messaging` skill.

## Install

Run `flopwire setup`. See [Install into the harnesses](README.md#install-into-the-harnesses).
The plugin it installs runs `flopwire hook` through its shim on
`SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop` and
`SessionEnd`, and serves the MCP tools. See
[How the hooks find the binary](README.md#how-the-hooks-find-the-binary).

Claude Code names the plugin's MCP server `plugin:flopwire:flopwire`, so
its tools show as `mcp__plugin_flopwire_flopwire__*`. A running Claude Code
session loads the plugin after a restart or after `/reload-plugins`.

## Manual configuration

Use the manual configuration below only when you cannot install the
plugin. Do not use both: each hook would then run twice.
`flopwire setup` warns when it finds both. The manual hooks run
`flopwire` from the hook shell's `PATH`.

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
