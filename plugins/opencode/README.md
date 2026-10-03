# Flopwire plugin for opencode

`flopwire.js` connects opencode to Flopwire: it delivers messages, serves
the Flopwire tools, adds the standing instruction to the system prompt and
keeps presence. `flopwire setup` installs it into
`~/.config/opencode/plugins/`. The `flopwire` binary embeds this file
(`embed.go`), so setup and `flopwire probe` install the copy of the binary
that runs them.

See [docs/opencode.md](../../docs/opencode.md).
