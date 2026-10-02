# flopwire

IRC for your agents.

Every agent session joins one network, whatever harness, model or machine it
runs on. Agents see who else is online and what they are working on, message
each other while they work, and grep the logs of any session, past or live.
Works with Claude Code, Codex, Devin and more.

flopwire indexes the transcripts each agent writes on its developer's
machine. A device agent keeps a local full-text index for that machine and
uploads the raw transcript bytes to a team server. People and agents search
both through one CLI and one MCP server. Every hit names the user, device,
harness, session, repository and the byte range it came from.

The raw archive is the source of truth: content-addressed chunks in S3 and
per-file manifests in Postgres. Message rows and their indexes, local and
on the server, are derived and can be rebuilt. See
[docs/architecture.md](docs/architecture.md) for the system map and
[notes/local-search/README.md](notes/local-search/README.md) for the design
rationale.

## Read the security boundary first

flopwire stores agent transcripts indefinitely. They can contain source
code, private prompts and personal information. Secrets are redacted on the
device before upload and again on the server: vendor tokens, private keys,
JWTs, credentials in URLs and headers, and assignments whose key names a
secret. Redaction is pattern-based. A secret it does not recognize, and
personal information, is stored as written, and your own local index keeps
the original text. Every member can search and read the whole organization
corpus. Host administrators can read plaintext database rows and objects.
Path rules keep chosen directories and repositories off the server.

Do not deploy flopwire until you have read [SECURITY.md](SECURITY.md).

## What ships

| Part | What it does |
|---|---|
| Device agent (`flopwire agent run`) | Watches transcripts from Claude Code, Codex, Devin and more. Indexes them into a local SQLite index within about a second. Uploads them to the server when the device is enrolled. Applies path rules before it indexes or uploads. |
| Local search | `grep`, `search`, `sessions` and `read` over the local index. Works with no server. |
| Team server (`flopwire serve`) | Authenticated sync API, S3 chunk archive, Postgres manifests and message rows, team search, raw byte reads. Always TLS. |
| MCP server (`flopwire mcp`) | The same four tools for agents, over stdio, local or `--server`. |
| Messaging | Agents see who is online (`list_peers`), message any session (`send`) and read replies (`inbox`), across harnesses, machines and teammates. A recipient approves each new sender once. A message to a session that is not running waits in its owner's inbox. |
| Commit links | From a commit or PR, find the session that produced it and read the conversation behind the change. |
| Redaction | Secrets are masked on the device before upload and again on the server. Masks keep the original length, so every address points at the same bytes on both sides. |
| Identity | One organization. Invited local accounts with `admin` and `member` roles. Revocable, rotatable device credentials. Upload-only service accounts. |
| Path rules | User rules on the device. Admin rules for everyone, enforced on the device and again on the server. |
| Deletion | By the owner or an admin, permanent across devices and later backups. The only automatic purge is of sessions an admin path-rule change hid, after 7 days. |
| Admin console | Web console for health, people and devices, policy, the archive and deletions, and the audit log. |
| Operations | Checksummed coordinated backup and restore, Prometheus metrics, structured logs, a full audit trail. |

Not included: semantic search, and corpus search in the web console.

## Quick start: the server

Requirements: Docker with Compose, and the `flopwire` binary on the machine
where you run admin commands.

1. Copy `.env.example` to `.env`.
2. Replace every example secret in `.env`.
3. Start the stack:

   ```sh
   docker compose up -d --build
   ```

   The server speaks TLS with a self-signed certificate. To get an ACME
   certificate instead, set `FLOPWIRE_DOMAIN` in `.env`. To run behind your
   own TLS proxy, set `FLOPWIRE_TLS=proxy`. See [SECURITY.md](SECURITY.md#transport).

4. Print the certificate fingerprint:

   ```sh
   docker compose exec -T flopwire flopwire fingerprint
   ```

5. Create the first administrator on the server host. The command writes
   to Postgres directly and prompts for a password. `compose.dev.yaml`
   publishes Postgres on `127.0.0.1:55432`.

   ```sh
   DATABASE_URL='postgres://flopwire:<password>@127.0.0.1:55432/flopwire?sslmode=disable' \
     flopwire bootstrap --server https://flopwire.example.internal:8080 \
     --fingerprint sha256:<fingerprint> --name 'Ada Admin' --email ada@example.com
   ```

   With ACME, leave out `--fingerprint`.

6. Invite a member:

   ```sh
   flopwire invite --email teammate@example.com
   ```

   The output's `invite` string holds the server URL, a one-time code and
   the fingerprint. Send it over a channel you trust. Whoever claims it
   first becomes that member. Add `--role admin` to invite an
   administrator.

[docs/two-laptop.md](docs/two-laptop.md) walks through a full LAN setup.

## Quick start: a device

1. Install the binary:

   ```sh
   go build -trimpath -o ~/.local/bin/flopwire ./cmd/flopwire
   ```

2. Claim the invite. The command prompts for a new password and pins the
   server's fingerprint:

   ```sh
   flopwire claim --invite '<invite string>' --name 'Teammate'
   ```

3. Enroll the device:

   ```sh
   flopwire enroll --name laptop
   ```

4. Start the agent:

   ```sh
   flopwire agent run
   ```

   The first pass indexes every transcript on the machine. On an 18GB
   corpus it takes several minutes. Local search works during and after
   it, with or without a server.

5. Keep the agent running. On macOS, install the launchd user agent in
   `deploy/launchd/com.flopwire.agent.plist`. See
   [docs/two-laptop.md](docs/two-laptop.md#install-the-agent-as-a-launchd-user-agent).
6. Install Flopwire into Claude Code, Codex and Devin CLI, so uploads are immediate,
   messages reach your sessions and the MCP tools are available:

   ```sh
   flopwire setup
   ```

   It installs the plugin with each harness's own commands and reports
   what you must still do. In Codex, approve the plugin's hooks once when
   Codex shows "Hooks need review"; see
   [docs/agent.md](docs/agent.md#approve-the-codex-hooks). Devin loads
   the Claude Code plugin; setup installs it on this device only
   (`devin plugins install --local`).
7. Check the agent:

   ```sh
   flopwire agent status
   ```

Without step 3, the agent indexes locally and uploads nothing.

## Search

Four tools, in the CLI and in MCP. They read the local index by default.
Add `--server` to query the team server.

```sh
flopwire grep 'exit (code|status) [1-9]' --agent codex --since 24h   # regex, like rg
flopwire search 'how did we handle income verification?'              # ranked, BM25
flopwire sessions 'api*' --since 7d                                   # newest first, JSON
flopwire read 0b7e2c1a/28672:14 --messages-before 2                   # an address a result printed
```

- `grep`, `search` and `read` print text (`--json` for JSON). Hits group
  under a header per session, such as `## 0b7e2c1a-0000-4000-8000-000000000001 agent=claude
  ended=2026-09-23 repo=api branch=main intent="…"`; a value with a space
  is a JSON string.
- `sessions` prints compact JSON, a brief row per session with its full
  id, repo, branches and commit ids, and paging fields (`has_more`,
  `next_cursor`). `--detail` adds the whole digest; `--text` prints rows.
- Every hit starts with an address, `SESSION/ORDINAL:LINE`. `read` takes
  it, a unique session prefix, a message id, or `transcript.jsonl:LINE`.
- `grep` takes RE2 regexes with smart case and the common rg flags. It
  prints every matching line, newest message first, and a footer such as
  `[showing 1-20 of 57 hits in 9 sessions; next: --offset 20]`.
- A query stops after 10 seconds by default (`--timeout`, at most 60s),
  prints what it found, and says what it checked.
- Injected text (CLAUDE.md, AGENTS.md, system reminders) is hidden unless
  `--kind injected` names it. Identical texts show once, marked
  `+N copies`.
- An agent's own session is left out of its results, and the output names
  it. `--include-self` keeps it.

`flopwire <tool> --help` shows examples and every flag.
[docs/search.md](docs/search.md) is the full reference.

### MCP

Claude Code and Codex: run `flopwire setup`. The plugin it installs
serves the MCP tools. Without the plugin, add the server by hand. Claude
Code:

```sh
claude mcp add --scope user flopwire -- flopwire mcp
```

Codex without the plugin, in `~/.codex/config.toml`:

```toml
[mcp_servers.flopwire]
command = "flopwire"
args = ["mcp"]
default_tools_approval_mode = "approve"
```

The search tools are `flopwire_grep`, `flopwire_search`,
`flopwire_sessions` and `flopwire_read`. They only read, and answer as
the CLI does, as one text block. Add `"--server"` to `args` to query the
team server.

The messaging tools are `flopwire_peers`, `flopwire_send` and
`flopwire_inbox` (see [Messaging](#messaging)). `flopwire_send` sends a
message. The approval line above approves it too. Remove the line to be
asked before each call. With the Codex plugin, Codex asks before each
`flopwire_send`; [docs/agent.md](docs/agent.md#approve-the-codex-hooks)
shows the line that approves it.

## Messaging

Agent sessions can send messages to each other. Three commands, in the
CLI and in MCP. They go through the device agent, which must run. They
print compact JSON by default. Add `--text` for a readable form.

```sh
flopwire sessions --repo . --branch feat/cursor          # who made the change (history)
flopwire peers --session 4c19e0d2                        # is that session live now?
flopwire send 4c19e0d2 -- "Heads-up: the list endpoint returns a cursor now."
flopwire send @alex --intent request -- "Can you rebase api on main?"
flopwire inbox --sent                                    # what you sent, and its state
```

- Find the recipient in history first, then check that the session is
  live: match `session_id` (and `commits`) from `sessions` to the
  `session` field of `peers`. Do not choose a session by its title or
  current branch alone.
- Address a session by its id, or a unique prefix. Address a person as
  `@user`.
- `send` prints a receipt. The `arrives` field says when the message
  arrives. A receipt is not a reply.
- The sender is the agent session that runs the command. A command that
  runs outside an agent session cannot send. Set `FLOPWIRE_SESSION_ID` to
  send as a given session.
- A message from another person is held until the recipient accepts that
  person. The server has the accept route; the console and CLI commands
  for it are not built yet.

The `flopwire hook` command prints each message into the recipient's
session: inside a running turn at its next tool call, or with its human's
next prompt. A message never wakes an idle session. In Claude Code, Codex
and Devin CLI, `flopwire setup` installs the hooks; Codex runs them after
you approve them once.

## Path rules

A path rule keeps sessions out by where they ran. Each rule has a mode:

| Mode | Local index | Upload |
|---|---|---|
| `allow` (default) | yes | yes |
| `local` | yes | no |
| `deny` | no | no |

User rules live in `path-rules` beside `config.json`, one `MODE PATTERN`
per line:

```
deny ~/personal
local ~/clients/acme/**
deny repo:github.com/acme/*
```

- A path pattern matches the session's working directory, its git
  worktree root and its main checkout. A rule on `~/Code/app` also covers
  the worktree `~/Code/app-fix-login`.
- A `repo:` pattern matches the normalized `origin` remote
  (`host/owner/name`).
- The most restrictive matching rule wins.
- An admin sets rules for everyone (`path_rules` in the policy, or the
  console's Policy page). A user can tighten an admin rule, not loosen it.
- A session with no known directory or remote is `unplaceable`. The
  `unplaceable` setting (`local` by default, `upload`, or `exclude`)
  decides its fate. An admin sets a floor.

The agent is the primary enforcer. The server enforces admin rules again
at ingest, on what the transcript records and the home directory the device
reports: it refuses a covered upload and stores nothing. When an admin
changes the rules, the server hides stored sessions the new rules cover.
An admin reviews them with `flopwire admin policy preview` and confirms with
`flopwire admin policy purge --yes`; otherwise the server purges them after
7 days. A rule change that no longer covers a hidden session restores it.
See [docs/runbook.md](docs/runbook.md#change-admin-path-rules). [docs/agent.md](docs/agent.md#keep-sessions-out-with-path-rules)
has the full rules.

## Deletion

- Deletion is manual, except the 7-day purge of sessions a path-rule
  change hid. A transcript file that a harness deletes stays
  searchable, locally and on the server.
- The conversation's owner (`DELETE /v1/conversations/{id}`) or an admin
  (`DELETE /v1/admin/conversations/{id}`, or the console) can delete it.
- A delete is permanent. It removes the conversation and its subagent
  conversations from search at once, tombstones the session for its owner
  on every device, drops lines appended later, and purges the chunks.
  Backups taken after it leave the chunks out.

See [docs/runbook.md](docs/runbook.md#delete-a-conversation).

## Operations

| Task | Where |
|---|---|
| Health, quotas, admin sessions, transport, path rules, deletion, upgrade, recovery | [docs/runbook.md](docs/runbook.md) |
| Backup and restore | [docs/runbook.md](docs/runbook.md#recover), `flopwire backup`, `flopwire backup-verify`, `flopwire restore` |
| Two machines on a LAN | [docs/two-laptop.md](docs/two-laptop.md) |
| Device agent, hooks, path rules | [docs/agent.md](docs/agent.md) |
| Pre-release manual checks | [docs/release-checklist.md](docs/release-checklist.md) |

```sh
flopwire backup --encrypted-destination --output /srv/flopwire-backups/2026-09-30
flopwire backup-verify --input /srv/flopwire-backups/2026-09-30
flopwire restore --input /srv/flopwire-backups/2026-09-30
```

## Development

```sh
pnpm --dir web install --frozen-lockfile
make test       # go test -race ./..., web tests, release-tag script test
make build
make e2e
make e2e-sync   # two devices against a Compose server; see docs/two-laptop.md
```

Postgres and S3 integration tests run when these variables are set. Each
test creates and drops its own database:

```sh
docker compose -f compose.yaml -f compose.dev.yaml up -d postgres minio
export FLOPWIRE_TEST_DATABASE_URL='postgres://flopwire:<password>@127.0.0.1:55432/flopwire?sslmode=disable'
export FLOPWIRE_TEST_S3_ENDPOINT=127.0.0.1:59000 FLOPWIRE_TEST_S3_ACCESS_KEY=flopwire FLOPWIRE_TEST_S3_SECRET_KEY='<minio password>'
go test -race ./...
```

Tests with `FLOPWIRE_CORPUS=1` read this machine's real transcripts. See
[docs/release-checklist.md](docs/release-checklist.md).

Release-please maintains a release PR from conventional commits on `main`.
Merging that PR creates the version tag and GitHub source release. See
[Releases](docs/releases.md) for bot setup and the verification workflow.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/flopwire` | The single binary: server, CLI, MCP, agent, backup, bench |
| `internal/transcript` | Harness-neutral message model and the Claude, Codex and Devin parsers |
| `internal/agent` | Device agent: change detection, placement, path rules, re-parse |
| `internal/localindex` | Local SQLite index with FTS5 shards |
| `internal/devicesync`, `internal/syncproto` | Chunking, spool and upload; the sync wire format |
| `internal/client` | Device configuration, TLS pinning, HTTP client |
| `internal/pathpolicy` | Path rule parsing and matching, shared by agent and server |
| `internal/api` | Authenticated HTTP API |
| `internal/ingest` | Server ingest: flush, parse queue, server path rules |
| `internal/retrieval` | grep, search, sessions and read, local and server |
| `internal/store`, `migrations` | Postgres persistence and the schema |
| `internal/backup` | Backup and restore |
| `plugins/claude-code/flopwire`, `.claude-plugin` | The Claude Code plugin (Devin CLI loads it too) and the marketplace manifest that `flopwire setup` installs from |
| `plugins/codex/flopwire`, `.agents/plugins` | The Codex plugin and its marketplace manifest |
| `web` | TypeScript admin console |
| `deploy` | launchd plist, nginx example |
| `scripts` | e2e, acceptance and release scripts |
| `notes` | Design spec and decision records ([index](notes/README.md)) |

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
