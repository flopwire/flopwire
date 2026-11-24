# Google Antigravity CLI

The device agent reads session history from the `agy` CLI. It looks for
`~/.gemini/antigravity-cli/brain/<conversation-id>/.system_generated/logs/transcript.jsonl`.
Set `FLOPWIRE_ANTIGRAVITY_ROOT` or pass `flopwire agent run --antigravity-root PATH`
to use another `brain` directory. Use `--antigravity-root -` to disable collection.

Search covers user prompts, model replies and supported tool activity.
Flopwire skips system checkpoints. It reads the adjacent
`conversation_summaries.db` in read-only mode and uses workspace metadata
to apply [path rules](../path-rules.md). When the catalog has no row,
Flopwire can use `cache/conversation_metadata.json`. Sessions without a
known workspace follow the device's unplaceable policy, which defaults
to local indexing. The catalog stays on the device. The server evaluates
its own rules against the evidence available in the archive. When that
evidence cannot place a session, the server applies the admin's
unplaceable floor. The server permits unplaceable sessions by default.

Search Antigravity history from the CLI or use `agent: "antigravity"` in
the MCP retrieval tools.

```sh
flopwire sessions --agent antigravity --text
flopwire grep --agent antigravity 'text from the session'
```

## Plugin setup

Run `flopwire setup` with `agy` on your `PATH`, then restart your
Antigravity session. Setup installs the Flopwire plugin through the
`agy` CLI. Use `flopwire setup --check` to inspect the installation
or `flopwire setup --remove` to remove the plugin. To enable a disabled
installation, run `agy plugin enable flopwire`.

## Messaging

Flopwire identifies a calling session when its `agy` process holds exactly
one conversation lock. On macOS this requires `lsof`. On Linux Flopwire
reads process file descriptors through `/proc`. An absent or ambiguous
lock leaves the caller unidentified.

The `PreInvocation` hook inserts pending messages as context before a
model invocation. Start a turn in an idle session to receive a message.
Antigravity has no Flopwire wake support or separate user-only hook notice
for held messages.

Antigravity CLI 1.5.16 supplies the full trajectory path to its hooks.
Flopwire flushes the adjacent canonical transcript and delivers messages
through a system step in the current invocation. The delivery receipt
confirms the hook wrote the message. Antigravity read receipts are unavailable.
