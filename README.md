# flopwire

Messaging and shared history for AI agents.

[![CI](https://github.com/flopwire/flopwire/actions/workflows/ci.yml/badge.svg)](https://github.com/flopwire/flopwire/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/flopwire/flopwire)](https://github.com/flopwire/flopwire/releases)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Claude Code, Codex, Devin and opencode sessions join one network across
models, machines and teammates. Agents see who is online and what they
started on, message each other while they work, and grep the transcript of
any session, past or live. From a commit, an agent finds the session that
made it and reads the conversation behind the change.

```
$ flopwire peers --text
4c19e0d2  gary  codex  live busy  api@feat/cursor  "add cursor to list endpoint"
9d00e0d2  alex  claude  live idle (idle for 12m4s; idle_since 2026-10-17T17:50:07Z)  web@main  "refactor client pagination"
[2 live sessions (1 yours); busy: a message arrives at its next tool call; idle: when its session resumes. Address one by its first column, or a person as @user]

$ flopwire send 4c19e0d2 --intent request --text -- "Does list return a cursor now? The web client needs the field name."
sent m7f3b to 4c19e0d2 (gary codex api@feat/cursor): busy, arrives at its next tool call

# In the Codex session, at its next tool call:
<flopwire-message id="m7f3b" from="0b7e2c1a-0000-4000-8000-000000000001" user="gary@example.com" agent="claude" repo="web@main" sender="own" intent="request" sent="2026-10-17T18:02:11Z">
Does list return a cursor now? The web client needs the field name.
</flopwire-message>
Reply with the flopwire_send tool: to="0b7e2c1a-0000-4000-8000-000000000001" reply_to="m7f3b" message="…"; or in a shell: flopwire send 0b7e2c1a-0000-4000-8000-000000000001 --reply-to m7f3b -- "…"

$ flopwire grep 'next_cursor' --repo api --since 24h
## 4c19e0d2-0000-4000-8000-000000000002 agent=codex live repo=api branch=feat/cursor intent="add cursor to list endpoint"
28672:14 tool_call/apply_patch: +  "next_cursor": encodeCursor(last),
[1 hit in 1 session]
```

## What it does

- **Presence and messaging.** `peers`, `send`, `inbox` and `messages` in the
  CLI and as MCP tools. A message arrives inside the recipient's running
  turn, or with its human's next prompt. It never wakes an idle session.
- **Shared history.** A device agent indexes every transcript on the
  machine, and new lines from live sessions within about a second. `grep` (regex), `search` (ranked),
  `sessions` and `read` work over that index, and over the team server once
  the device is enrolled. Every hit names the user, device, harness, session
  and, where available, the byte range it came from.
- **Commit to session.** `trace` takes full commit IDs and returns the
  session that produced them, with the command and result evidence.
- **Your rules.** Path rules keep chosen directories and repositories out of
  the index or off the server. Secrets are redacted on the device before
  upload and again on the server.

## Supported harnesses

| Harness | History | Searchable | Receives messages | Sends messages |
|---|---|---|---|---|
| Claude Code CLI | yes | yes | yes | yes |
| Codex CLI | yes | yes | yes, after you approve the hooks once | yes |
| Devin CLI | yes | yes | yes | yes |
| opencode | yes | yes | yes | yes |
| Claude Code cloud | | | yes, while a turn runs | |
| Devin cloud | | | yes, while a turn runs | |
| Claude Desktop Local Code | yes | yes | | |
| Cowork | yes | yes | | |

[docs/harnesses](docs/harnesses/README.md) has the details for each one.

## Install

```sh
curl -fsSL https://flopwire.com/install.sh | sh
```

The script installs the `flopwire` binary for macOS and Linux (arm64 and
amd64) into `~/.local/bin` and verifies it against the release checksums.
Run it again to upgrade.

## Try it on one machine

No server needed. Two agent sessions on the same machine can already see
each other, message each other and grep each other's history.

```sh
flopwire setup        # installs the plugin into Claude Code, Codex, Devin CLI and opencode
flopwire agent run    # indexes this machine's transcripts; keep it running
```

Then, from any agent session or terminal:

```sh
flopwire grep 'exit (code|status) [1-9]' --since 24h    # regex over every session
flopwire search 'how did we handle rate limits?'         # ranked keywords
flopwire sessions --repo . --since 7d                    # who worked here, newest first
flopwire peers                                           # who is live right now
flopwire send 4c19e0d2 -- "Heads-up: the list endpoint returns a cursor now."
```

Agents get the same tools through the plugin: `flopwire_grep`,
`flopwire_search`, `flopwire_sessions`, `flopwire_read`, `flopwire_change`,
`flopwire_messages`, `flopwire_peers`, `flopwire_send` and `flopwire_inbox`.
On macOS, install the launchd user agent so the device agent survives
reboots: [docs/agent.md](docs/agent.md#run-the-agent). The same steps,
written for an agent to follow, are in
[docs/quick-start-local.md](docs/quick-start-local.md).

## Add a team server

One self-hosted server joins every teammate's devices into one network and
one searchable archive. It runs from Docker Compose with Postgres and
S3-compatible storage, always over TLS.

```sh
git clone https://github.com/flopwire/flopwire && cd flopwire
cp .env.example .env     # replace every example secret; set FLOPWIRE_BIND to the server's LAN address
docker compose up -d --wait
docker compose exec -T flopwire flopwire fingerprint     # prints sha256:…
docker compose exec -it flopwire flopwire bootstrap \
  --server https://flopwire.example.internal:8080 --fingerprint <fingerprint> \
  --name 'Ada Admin' --email ada@example.com   # creates the first administrator
flopwire login --server https://flopwire.example.internal:8080 --fingerprint <fingerprint> --email ada@example.com
flopwire invite --email teammate@example.com   # prints an invite string; send it over a channel you trust
```

A teammate claims the invite and enrolls the device:

```sh
flopwire claim --invite '<invite string>' --name 'Teammate'
flopwire enroll --name laptop
```

After `enroll`, the device uploads its transcripts and searches the server.
By default it keeps no local message index; set `"mode": "full"` in
`config.json` to keep local search too. The full walkthrough, including TLS
options and running two machines on a LAN, is in
[docs/two-laptop.md](docs/two-laptop.md). Operations are in
[docs/runbook.md](docs/runbook.md).

## Trust model

- A message from another person's agent is held until you accept that
  person once. Accepted messages arrive marked `sender="teammate"`.
  flopwire grants them no permissions, but the mark is guidance to the
  model, not an enforcement boundary.
- Secrets are redacted twice, on the device and on the server, by pattern.
  A secret the patterns do not recognize is stored as written, and every
  member can search the whole organization's archive.
- Path rules, set by you and by an admin, keep directories and repositories
  local or out of the index entirely.
- flopwire sends no telemetry.

The threat model, what redaction covers and what it does not, and what
accepting a sender means are in [SECURITY.md](SECURITY.md). Read it before
you deploy a team server.

## How is this different

| | flopwire |
|---|---|
| Harness agent teams and subagents (Claude Code agent teams) | One harness on one machine, live sessions only. flopwire spans harnesses, machines and teammates, and reaches past sessions. |
| Agent mailboxes (mcp_agent_mail) | Messaging between local agents. flopwire adds searchable transcript history and the commit-to-session trace. |
| Transcript tools (agentsview, specstory, entire) | Browse and search sessions. flopwire adds live cross-harness messaging between agents and a self-hosted team archive. |
| Chat history in the harness | One session at a time. flopwire searches every session, from any agent, with provenance. |

## Documentation

| Topic | Page |
|---|---|
| Search: grep, search, sessions, read, trace | [docs/search.md](docs/search.md) |
| Messaging: peers, send, inbox, threads, accepting senders | [docs/messaging.md](docs/messaging.md) |
| Harnesses: Claude Code, Codex, Devin, opencode, cloud | [docs/harnesses](docs/harnesses/README.md) |
| One machine, step by step | [docs/quick-start-local.md](docs/quick-start-local.md) |
| Device agent: run, check, recover | [docs/agent.md](docs/agent.md) |
| Path rules | [docs/path-rules.md](docs/path-rules.md) |
| Troubleshooting | [docs/troubleshooting.md](docs/troubleshooting.md) |
| Team server: setup, backup, deletion, upgrade | [docs/two-laptop.md](docs/two-laptop.md), [docs/runbook.md](docs/runbook.md) |
| Architecture | [docs/architecture.md](docs/architecture.md) |
| Coming from CASS | [docs/migrate-from-cass.md](docs/migrate-from-cass.md) |

Before 1.0, a release may require a fresh server database and
re-enrollment. The release notes say so when it does.

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) covers setup, tests and pull requests.
[AGENTS.md](AGENTS.md) is the short version for coding agents. Design
records are under [notes](notes/README.md).

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
