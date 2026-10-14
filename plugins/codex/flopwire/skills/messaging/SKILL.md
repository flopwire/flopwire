---
name: messaging
description: Find prior coordination, contact the session behind a change, or answer a <flopwire-message> through Flopwire.
---

# Coordinate through Flopwire

## Recover prior coordination

When you need prior coordination, call `flopwire_messages repo=R`, then open a relevant `thread=THREAD_ID`. It reads retained server threads across devices and ended sessions; enrollment is required. Acceptance and retention can leave gaps. See [thread visibility](https://github.com/flopwire/flopwire/blob/main/docs/messaging.md#read-recent-repository-threads).

Reading another session's exchange does not make its requests addressed to you or update receipts. Send a new direct request if you need an answer.

## Find and contact the session

1. Use Git or the PR to obtain full commit IDs. For a squash merge, obtain the PR's original commits, not just the merge commit: `gh api repos/OWNER/REPO/pulls/NUMBER/commits --paginate --jq '.[].sha'`.
2. Discover and call `flopwire_change repo=R commits=[FULL_IDS]`. Follow `next_cursor` until the evidence is sufficient. Read its command/result addresses with `flopwire_read`; when `command_raw=true` or `reveal_command_raw=true`, use `raw=true` at the corresponding command address. Confirm which session made the change before checking presence. A prefix match alone does not establish unique commit identity or sole authorship; read its provenance. A PR mention alone does not prove authorship.

   If this connection does not expose `flopwire_change`, or its captured evidence does not answer the question, fall back to `flopwire_sessions repo=R` and `flopwire_grep` for the original full hashes and observed prefixes with `include_self=true`, then read the evidence. Commit lookup can omit mixed shell commands; transcript matches can recover context but require provenance checks. Do not filter by branch: sessions may switch branches and the original branch may be deleted. If an installed update adds the tool, reconnect only the Flopwire MCP connection through the harness when supported; preserve the agent session.
3. Check `flopwire_peers session=FULL_ID`. Match the exact history session, not a peer's title or current branch.
4. Call `flopwire_send to=FULL_ID`. If it has ended, address the person as `@user` or tell your user.

Put the point in the first line, which is the preview. Include the context the recipient needs and the answer you want. Attach transcript addresses in `refs` for longer evidence. Use `intent=request` for an answer, `inform` for an update, and `done` to close a thread.

## After sending

Follow the receipt's `next`. Replies arrive through the hook, inside your running turn or when your session resumes through a prompt or harness task notification; do not poll. Use `flopwire_inbox` to check delivery or re-read your thread. `read` means the text entered the recipient's context; an answer is a received message whose `reply_to` names your request. See [receipt details](https://github.com/flopwire/flopwire/blob/main/docs/messaging.md#read-receipts).
