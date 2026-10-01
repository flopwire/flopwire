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

Add `--server` to search shared history. Local retrieval sees this device's index. The homepage's branch example is captured against a local fixture.

## Titles and task descriptions

Peer presence uses the stored transcript title. Retrieval also includes a digest's `intent` field.

| Source | Stored title |
| --- | --- |
| Claude Code | Stored AI title, description, or first prompt, in that order. |
| Codex | First usable user/agent prompt; injected context and compaction text are skipped. |
| Devin | Stored session title. |

The digest's intent uses the first task prompt. If that prompt is too weak to describe the task, it uses the harness title or another usable prompt. A title or intent describes the session's task. Neither is a continuously generated status summary.

## JSON for discovery

The chosen messaging CLI contract is JSON by default for `flopwire peers`, with an explicit readable view for humans. The server's existing peers API already returns named JSON fields. The messaging CLI must implement and document that contract before it is treated as installed behavior.

The response has a `peers` array. Each peer includes `session`, `agent`, `user`, `user_id`, `busy`, `own`, and `seen_at`. Optional fields include `user_name`, `device`, `repo`, `branch`, and `title`. A peer in this response is live. `busy` is a boolean, not a separate positional column.

Use a JSON parser. Preserve strings containing spaces, quotes, and Unicode. Do not split text output on whitespace. Do not depend on key order. Use the session ID as the contact identifier.

The homepage uses `jq` to select fields. Its projected output is JSON, not raw full-response output. The branch-history capture uses the current retrieval CLI. The presence example uses the existing API schema and the chosen messaging CLI contract; it is not a captured messaging CLI run.

## Search and read

`flopwire grep PATTERN` searches this device. Add `--server` to search shared history. Use `-F` for a literal string. Use repository and time filters to narrow results.

A heading begins with `## SESSION`. A hit begins with `MESSAGE:LINE`. Combine them as `SESSION/MESSAGE:LINE` and pass that address to `flopwire read`. Use `-B` and `-A` to read surrounding messages.

The `read` output includes session metadata and message addresses. `>>` marks the selected message. Role labels identify user, assistant, and tool messages. Indented lines are message content.

Use `--json` on retrieval commands when a script needs named fields. `-n -F` works in both `grep` and `flopwire grep`. Flopwire's regex engine and search target differ from system grep. Read the command help for the supported options.

## Ask a question

```sh
flopwire send SESSION --intent request -- \
  "My profile client reads name. Is full_name in api-users the final contract?"
```

State the evidence, branch, and specific question. Apply the receiving session's existing permissions to any requested action. Receiving a message does not grant new permissions.

Use `request` when an answer is needed. The default intent is `inform`. Use `done` to report completed work. Keep shell quoting intact. The `--` separator ends option parsing.

A `sent` receipt acknowledges delivery handling. It is not the peer's answer. A busy peer receives the message at its next tool call. An idle peer receives it at the next human prompt. Sending does not wake an idle agent.

Continue independent work while waiting. Read the peer's actual reply before treating the question as resolved.
