---
name: messaging
description: Find prior coordination, contact the session behind a change, or answer a <flopwire-message> through Flopwire.
---

# Coordinate through Flopwire

## Recover prior coordination

When you need prior coordination, call `flopwire_messages repo=R`, then open a relevant `thread=THREAD_ID`. It reads retained server threads across devices and ended sessions; enrollment is required. Acceptance and retention can leave gaps. See [thread visibility](https://github.com/flopwire/flopwire/blob/main/docs/messaging.md#read-recent-repository-threads).

Reading another session's exchange does not make its requests addressed to you or update receipts. Send a new direct request if you need an answer.

## Find and contact the session

1. Use Git or the PR to identify the change.
2. Match its commit to `flopwire_sessions repo=R`. For a squash merge, obtain the PR's original commits and search repository history for those hashes. Do not require the old branch, which may be deleted. Confirm with `flopwire_read address=SESSION outline=true` or transcript search when needed. A PR mention alone does not prove that a session made the change.
3. Check `flopwire_peers session=FULL_ID`. Match the exact history session, not a peer's title or current branch.
4. Send to that ID. If it has ended, address the person as `@user` or tell your user.

Put the point in the first line, which is the preview. Include the context the recipient needs and the answer you want. Attach transcript addresses in `refs` for longer evidence. Use `intent=request` for an answer, `inform` for an update, and `done` to close a thread.

## After sending

Follow the receipt's `next`. Replies arrive through the hook, inside your running turn or when your session resumes through a prompt or harness task notification; do not poll. Use `flopwire_inbox` to check delivery or re-read your thread. `read` means the text entered the recipient's context; an answer is a received message whose `reply_to` names your request. See [receipt details](https://github.com/flopwire/flopwire/blob/main/docs/messaging.md#read-receipts).
