# Flopwire plugin for opencode

`flopwire.js` connects opencode to Flopwire: it delivers messages, serves
the Flopwire tools, adds the standing instruction to the system prompt and
keeps presence. `flopwire setup` installs it into
`~/.config/opencode/plugins/`. opencode runs no shell for it, so it finds
the `flopwire` binary itself, in the order the other plugins' hook shim
uses: the path `flopwire setup` recorded, then `PATH`, then
`/opt/homebrew/bin`, `/usr/local/bin`, `~/go/bin` and `~/.local/bin`. The `flopwire` binary embeds this file
(`embed.go`), so setup and `flopwire probe` install the copy of the binary
that runs them.

See [docs/opencode.md](../../docs/opencode.md).
