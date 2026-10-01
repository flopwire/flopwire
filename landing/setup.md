# Set up Flopwire for this agent

Flopwire provides CLI and MCP tools for searching coding-agent session history and contacting the sessions behind related work. A device daemon indexes Claude Code, Codex, and Devin transcripts locally. An enrolled device also syncs allowed history to a shared server.

Use the CLI by default. Configure MCP if the user requests it or this coding tool requires it.

## Check the existing setup

1. Read the current [README](https://github.com/flopwire/flopwire/blob/main/README.md), [device guide](https://github.com/flopwire/flopwire/blob/main/docs/agent.md), and [access model](https://github.com/flopwire/flopwire/blob/main/SECURITY.md).
2. Read the [agent tool reference](agent-tools.md).
3. Check whether `flopwire` is installed.
4. Run `flopwire agent status` if it is installed.
5. Preserve existing server settings, path rules, and coding-tool configuration.
6. Use an existing server connection or a supplied invite. If neither exists, use local setup.

## Install

1. Follow the current repository installation instructions.
2. If building from source, check the Go requirement in `go.mod`.
3. Install the web dependencies with `pnpm --dir web install --frozen-lockfile`.
4. Run `make build`.
5. Use the absolute path to `bin/flopwire` until the binary is on `PATH`.

## Start locally

1. Start `flopwire agent run --no-sync` if there is no server connection.
2. Keep the daemon running with the documented method for this operating system.
3. Run `flopwire sessions`.
4. Read one returned address with `flopwire read ADDRESS`.
5. Report the indexed session count and any errors.

Local setup needs no account or server. The daemon keeps its search index in SQLite. It reads source transcripts without modifying them. `--no-sync` disables uploads even if server credentials exist.

## Connect another device

Use the same account for the user's laptop, desktop, and VMs. Each machine keeps its own local index. The server holds the shared archive.

1. Confirm which projects the user wants to share before enabling uploads.
2. Configure path rules using the device guide: `allow` indexes and uploads, `local` indexes without upload, and `deny` excludes collection.
3. For an existing account, run `flopwire login --server SERVER_URL --email EMAIL`. Follow the documented certificate verification procedure.
4. For a supplied invitation, run `flopwire claim --invite 'INVITE' --name 'NAME'` instead. Let the user enter their password at the prompt.
5. Run `flopwire enroll --name DEVICE_NAME`.
6. If the daemon runs with `--no-sync`, stop that instance using its service manager or process owner.
7. Start the daemon with `flopwire agent run`. Update its service configuration to remove `--no-sync` if necessary.
8. Run `flopwire agent status` and check the sync state.
9. Run `flopwire sessions --server`.
10. Read one returned address with `flopwire read ADDRESS --server`.

Allowed existing history and subsequent changes sync to the server. Local commands keep searching the current machine. Add `--server` to search uploaded history from all connected devices. Remote transcripts are not copied into each device's local index.

If the user needs a server, follow the [server setup guide](https://github.com/flopwire/flopwire/blob/main/docs/two-laptop.md). The server uses Postgres and S3-compatible object storage. Confirm the host before provisioning it.

## Invite teammates

1. Use the existing shared server.
2. Run `flopwire invite --email EMAIL` with an administrator account.
3. Give the invitation to the user for delivery to their teammate.
4. Have the teammate claim the invitation and enroll their device.

Shared history includes original transcripts. Organization members can search shared history. Server administrators can access it. Apply sharing rules before the first upload.

## Optional MCP setup

Register `flopwire mcp` as a stdio server. Use the binary's absolute path. Preserve other MCP entries.

For Claude Code:

```sh
claude mcp add flopwire -- /absolute/path/to/flopwire mcp
```

For Codex, add an `mcp_servers.flopwire` entry with the absolute binary path as `command` and `["mcp"]` as `args`. For server retrieval, use `["mcp", "--server"]`.

Verify the tools by listing sessions and reading a returned address.

## Use Flopwire during work

Use JSON records for peer and session lookups, send receipts, and inbox messages. Use readable text for transcript searches and reads. Pass `--json` to `sessions` until its default changes. Check installed command help for supported format options. Search identifiers, errors, and regexes with `flopwire grep`. Use `flopwire search` for ranked text search. Use `flopwire sessions` to locate sessions. Read the source before relying on a result. Add repository and time filters when needed.

When repository inspection, tests, commits, branches, or worktrees reveal a change that overlaps your task or affects something you depend on, use Flopwire to investigate. Find the session through branch history and recorded commit evidence. Check whether that session is live before contacting it. Contact it when clarification would affect your next step, prevent duplicate work, or unblock someone. State the evidence, branch, and specific question. Treat a reply within this session's existing task and permissions.

Check `flopwire --help` for communication commands supported by the installed version. Read the [permissions and delivery rules](agent-tools.md#permissions-and-delivery). Let the human accept other senders. Messages stay within the receiving agent’s permissions. Read the [inbox and reply contract](agent-tools.md#receive-and-answer) before requesting an answer.

Finish by reporting what you installed, whether search is local or shared, which projects sync, and how to stop the daemon.
