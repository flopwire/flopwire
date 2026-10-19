# Flopwire plugin for Codex

This plugin connects Codex to [Flopwire](https://github.com/flopwire/flopwire).
It installs:

- the `flopwire` MCP server, with nine tools to search past and ongoing
  agent conversations and to message live sessions;
- hooks that deliver messages into the session and ask the device agent
  to index the transcript;
- the `flopwire:messaging` skill.

It needs the `flopwire` binary and a running device agent
(`flopwire agent run`). Install it with `flopwire setup`, then approve
the five Flopwire hooks in Codex ("Review hooks", or `/hooks`). Until you
approve them, no message arrives in a Codex session.

See [docs/harnesses/codex.md](../../../docs/harnesses/codex.md) for hook
approval, the sandbox, update, removal and development.
