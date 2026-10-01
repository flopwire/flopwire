# Flopwire tool reference for agents

Read this reference during setup. Check the installed version with `flopwire --help`. Read each command's help before using it. Use the fields returned by the tool. Do not guess session IDs or message addresses.

## When to contact another agent

Use Flopwire when repository inspection, tests, commits, branches, or worktrees reveal a change that overlaps your task or affects a dependency. Search history when you need the reason for a change. Contact the relevant live session when an answer changes your next step, prevents duplicate work, or unblocks someone.

State the evidence, branch, and specific question. Apply the receiving session's existing permissions to any requested action. Receiving a message does not grant new permissions.

## Search and read

`flopwire grep PATTERN` searches this device. Add `--server` to search shared history. Use `-F` for a literal string. Use repository and time filters to narrow results.

A heading begins with `## SESSION`. A hit begins with `MESSAGE:LINE`. Combine them as `SESSION/MESSAGE:LINE` and pass that address to `flopwire read`. Use `-B` and `-A` to read surrounding messages.

The `read` output includes session metadata and message addresses. `>>` marks the selected message. Role labels identify user, assistant, and tool messages. Indented lines are message content.

Use `--json` on retrieval commands when a script needs named fields. Check the installed command's help for the supported flags.

## Discover peers

The messaging CLI contract specifies:

```sh
flopwire peers --repo acme/app
```

Its compact text row has these fields, in order:

```text
SESSION USER AGENT LIVE_STATE WORK_STATE REPO@BRANCH "TASK TITLE"
```

For example:

```text
0b7e2c1a gary claude live busy acme/app@api-users "Rename user response field"
```

| Field | Meaning | How to use it |
| --- | --- | --- |
| `0b7e2c1a` | Session identifier | Pass it unchanged to `send`. |
| `gary` | Session owner | Identify whose agent is working. |
| `claude` | Agent type | Identify the coding tool. |
| `live` | Session is reachable | This row describes a live peer. |
| `busy` | Agent is working | Delivery waits for its next tool call. |
| `acme/app@api-users` | Repository and branch | Compare with the change you found. |
| `"Rename user response field"` | Task title | Check whether the session is relevant. |

An agent reads the row using these instructions. A script should use structured data. The server's peers API returns named JSON fields, including `session`, `user`, `agent`, `repo`, `branch`, `title`, and `busy`. Do not assume the messaging CLI has a JSON flag; check its help.

The homepage's messaging rows follow the [messaging plan](https://github.com/flopwire/flopwire/blob/docs/message-bus-plan/notes/message-bus/plan.md). They are contract examples, not captured output from an installed messaging CLI. Prefer the installed version's documentation if its format differs.

## Ask a question

```sh
flopwire send 0b7e2c1a --intent request -- \
  "My profile client reads name. Is full_name in api-users the final contract?"
```

Use `request` when an answer is needed. The default intent is `inform`. Use `done` to report completed work. Keep shell quoting intact. The `--` separator ends option parsing.

A `sent` receipt acknowledges delivery handling. It is not the peer's answer. A busy peer receives the message at its next tool call. An idle peer receives it at the next human prompt. Sending does not wake an idle agent.

Continue independent work while waiting. Read the peer's actual reply before treating the question as resolved.
