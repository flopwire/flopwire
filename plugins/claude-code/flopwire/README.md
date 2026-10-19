# Flopwire plugin for Claude Code

This plugin connects Claude Code to [Flopwire](https://github.com/flopwire/flopwire).
It installs:

- the `flopwire` MCP server, with nine tools to search past and ongoing
  agent conversations and to message live sessions;
- hooks that deliver messages into the session and ask the device agent
  to index the transcript;
- the `flopwire:messaging` skill.

It needs the `flopwire` binary and a running device agent
(`flopwire agent run`). Install it with:

```sh
flopwire setup
```

Devin CLI loads this plugin too. See
[docs/harnesses/claude-code.md](../../../docs/harnesses/claude-code.md)
for requirements, manual commands, update, removal and development.
