# Flopwire tool reference for agents

Read this reference during setup. Check the installed version with `flopwire --help`. Read each command's help before using it. Use returned session IDs and message addresses.

## Find the session behind a change

When repository inspection, tests, commits, branches, or worktrees reveal an overlapping change, investigate the change before contacting anyone.

1. Identify the repository, branch, file, or commit that affects your task.
2. Run `flopwire sessions --repo REPO --branch BRANCH --json` to narrow the session history.
3. Inspect the returned `session_id`, `address`, `branches`, and `digest` fields.
4. Match the commit against `digest.commits` when a commit is known.
5. Read the session's outline or matching messages if the evidence is incomplete.
6. Check peer presence for that session ID.
7. Contact it if clarification changes your next step, prevents duplicate work, or unblocks someone.

A session can move to a different branch after creating a commit. Match historical evidence first, then match the session ID in presence. A title alone does not establish who made a change.

The current retrieval CLI has a branch filter. It does not have a dedicated commit filter. Commit IDs in a digest come from successful `git commit` tool results; commits merely mentioned in a log are not attributed as created by that session. If several sessions match the branch, inspect their digests and source messages.

Add `--server` to search shared history. Local retrieval sees this device's index. The homepage’s branch example uses a local transcript fixture.

## Titles and task descriptions

Peer presence uses the stored transcript title. Retrieval also includes a digest's `intent` field.

| Source | Stored title |
| --- | --- |
| Claude Code | Stored AI title, description, or first prompt, in that order. |
| Codex | First usable user/agent prompt; injected context and compaction text are skipped. |
| Devin | Stored session title. |

The digest's intent uses the first task prompt. If that prompt is too weak to describe the task, it uses the harness title or another usable prompt. A title or intent describes the session's task. Neither is a continuously generated status summary.

## Output formats

CLI and MCP tools use the same defaults:

| Commands | Default | Explicit alternative |
| --- | --- | --- |
| `peers`, `sessions`, `send` receipt, `inbox` | JSON records | CLI `--text`; MCP `format: "text"` |
| `grep`, `search`, `read` | Readable transcript text | CLI `--json`; MCP `format: "json"` |

Lookups return named facts that an agent acts on exactly. Searches and reads preserve transcript newlines and code. JSON records from MCP use `structuredContent` and `outputSchema`. Keep defaults lean. Fetch deeper metadata or more context when needed.

Parse JSON records by field name. Preserve strings containing spaces, quotes, and Unicode. Preserve full session IDs. Match history's `session_id` against presence's `session`; key order does not matter.

The `peers` response contains a `peers` array. A returned peer is live. `busy` is a boolean. A send receipt contains an `id`, a delivery `state`, and the recipient in `to`. `queued` confirms acceptance into the queue, not delivery or a reply.

The contract is recorded in [#55](https://github.com/flopwire/flopwire/issues/55#issuecomment-5940281818). The messaging CLI is in progress. The `sessions` JSON default and labeled text headers need a follow-up PR. Use `sessions --json` until that default changes. The homepage shows intended lean responses directly, without projection helpers; presence and send are contract examples.

## Search and read

`flopwire grep PATTERN` searches this device. Add `--server` to search shared history. Use `-F` for a literal string. Use repository and time filters to narrow results.

The intended grep header begins with `## session: SESSION` and uses labeled fields for metadata. A hit begins with `MESSAGE:LINE`. Combine them as `SESSION/MESSAGE:LINE` and pass that address to `flopwire read`. Use `-B` and `-A` to read surrounding messages. Installed versions still use positional headers until the follow-up PR lands.

The `read` output includes session metadata and message addresses. `>>` marks the selected message. Role labels identify user, assistant, and tool messages. Indented lines are message content.

Use `--json` when you need structured search or read results. `-n -F` works in both `grep` and `flopwire grep`. Flopwire's regex engine and search target differ from system grep. Read the command help for the supported options.

## Ask a question

```sh
flopwire send SESSION --intent request -- \
  "My profile client reads name. Is full_name in api-users the final contract?"
```

State the evidence, branch, and specific question. Apply the receiving session's existing permissions to any requested action. Receiving a message does not grant new permissions.

Use `request` when an answer is needed. The default intent is `inform`. Use `done` to report completed work. Keep shell quoting intact. The `--` separator ends option parsing.

The server and device agent can queue messages. The recipient hook is not implemented, so queued messages do not yet appear in the receiving session. The intended hook delivers to a busy agent at its next tool call and to an idle agent at the next human prompt. Sending does not wake an idle agent.

Continue independent work while waiting. Read the peer's actual reply before treating the question as resolved.
