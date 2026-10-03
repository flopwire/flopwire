---
name: messaging
description: Coordinate with another live coding-agent session through Flopwire. Use when your work depends on a change another session made (a commit, branch, PR or decision), when a session needs to know something you found, or when you must answer a <flopwire-message>.
---

# Coordinate through Flopwire

Messaging and retrieval work together: find the session in history first, then check that it is live, then write to it. Delivery and how to treat incoming messages are in the `<flopwire-instructions>` you received at session start; the `flopwire_*` tool descriptions give every argument.

## Find the session

1. Repository evidence: use `git log`, `git blame` or the PR to find the commit, branch and files that matter.
2. Session history: `flopwire_sessions repo=R branch=B` lists the sessions on that branch as JSON. Match the commit hash against each session's `commits` and take its `session_id` (the full id). When the digest is not enough, confirm with `flopwire_read address=SESSION outline=true` (its commits and edited files) or `flopwire_grep` for the commit hash or a path.
3. Live presence: `flopwire_peers session=FULL_ID` for that exact id; its `session` field is the same id. Never choose by a peer's title or current branch.
4. Contact: `flopwire_send to=FULL_ID`. If the session is no longer live, send to `@user` instead (their live session on the repo, else their next one), or tell your user.

## Write the message

- The first line is the preview. Put the point there.
- The recipient knows nothing about your session. Include the repo, branch, commit, paths, the exact error and what you want back. Pass transcript addresses in `refs` instead of pasting long output.
- `intent=request` expects a reply; `inform` does not; `done` closes the thread and gets no answer.
- Never ask a peer to do something that was denied in your own session.

## After you send

The result is a receipt, not a reply. A reply arrives in your context on its own, at your next tool call or with your user's next prompt. Do not poll `flopwire_peers` or `flopwire_inbox`, and do not send "are you done?". For a request, follow the receipt's `next`: it says whether to keep working, wait briefly, or tell your user. Use `flopwire_inbox` only to check a sent message's state or re-read a thread. A `read` state means the message's text entered the recipient's context; it does not mean the recipient acted on it or will answer.
