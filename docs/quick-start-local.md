# Start locally

Search session history and connect agents on one machine. No account or
server is required. Supported tools are Claude Code, Codex, Devin CLI and
opencode. Sessions can contain any kind of work.

## Install

If Flopwire is installed, run `flopwire setup --check` and use the existing
installation. Keep its configuration.

Otherwise, install Git and Go 1.26.6 or newer, then build Flopwire:

```sh
git clone https://github.com/flopwire/flopwire.git
cd flopwire
mkdir -p ~/.local/bin
go build -trimpath -ldflags="-X main.version=$(git rev-parse --short=12 HEAD)" \
  -o ~/.local/bin/flopwire ./cmd/flopwire
export PATH="$HOME/.local/bin:$PATH"
```

Add `~/.local/bin` to your shell's persistent `PATH`.

## Connect your agents

Install the integrations for the agent tools on this machine:

```sh
flopwire setup
```

Complete the actions it reports. Codex requires you to approve plugin
hooks. Reconnect existing agent sessions to load their new tools.

Start the daemon in a separate terminal:

```sh
flopwire agent run
```

It indexes session files and delivers messages between agents on this
machine. Keep it running. For background startup, see
[Run the device agent](agent.md#run-the-agent).

## Check it

```sh
flopwire agent status
flopwire sessions --text
flopwire search "a phrase from an earlier session"
```

The first indexing pass can take several minutes. From an agent session,
run `flopwire peers --text` to find other live agents. Use
`flopwire send SESSION -- "MESSAGE"` to message one.

## Add a server later

[Run your server](../README.md#quick-start-the-server), or
[join an existing server](../README.md#quick-start-a-device).
Enrollment makes eligible history visible to your organization. Review
[data access and path rules](../SECURITY.md) before enrolling.

Connected devices default to minimal local storage. If you already have a
full local index, choose whether to retain it when connecting: restart the
daemon with `--sync-only=false` to keep local search, or `--sync-only` to
remove the local message index and search the server. See
[Device agent storage](agent.md#run-the-agent).
