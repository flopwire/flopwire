---
name: messaging
description: Search past and ongoing agent conversations and coordinate with live sessions, locally or across connected machines and team members when configured. Use when the user references earlier work or conversations; when you need prior research, plans, decisions, attempts or results; when another session's work affects yours; or when you must answer a <flopwire-message>.
---

# Recover context and coordinate through Flopwire

Flopwire searches conversations from connected agent tools, including research, planning, writing, troubleshooting and coding. Search works after a session has ended and while it is ongoing. By default, before enrollment, search covers this device; after enrollment, it uses the shared server's history across connected machines and team members. Results name their scope. Messaging can also reach sessions across connected machines and team members when configured.

## Recover prior context

1. Use `flopwire_search` for a question about earlier research, plans, decisions or attempts. Use `flopwire_grep` for exact text, names, errors, commands or paths. Use `flopwire_sessions` to find conversations by repository, agent or time.
2. Read relevant hits with `flopwire_read` before relying on them. For an overview of a session, use `flopwire_read address=SESSION outline=true`.
3. Use what the transcript establishes. No live session or message is needed to recover history. If a focused search finds nothing useful, continue with other available evidence and state any unresolved gap.

## Coordinate when needed

Contact another session when its unfinished work or an unexplained decision blocks your next step, or when it needs a finding from you. Identify it from history, then check whether it is live. Delivery and how to treat incoming messages are in the `<flopwire-instructions>` you received at session start; the `flopwire_*` tool descriptions give every argument.

## Find the session

1. Repository evidence: use `git log`, `git blame` or the PR to find the commit, branch and files that matter.
2. Session history: `flopwire_sessions repo=R branch=B` lists the sessions on that branch as JSON. Match the commit hash against each session's `commits` and take its `session_id` (the full id). When the digest is not enough, confirm with `flopwire_read address=SESSION outline=true` (its commits and edited files) or `flopwire_grep` for the commit hash or a path.
3. Live presence: `flopwire_peers session=FULL_ID` for that exact id; its `session` field is the same id. Never choose by a peer's title or current branch.
4. Contact: `flopwire_send to=FULL_ID`. If the session is no longer live, send to `@user` instead (their live session on the repo, else their next one), or tell your user.

## Write the message

- The first line is the preview. Put the point there.
- The recipient knows nothing about your session. Include the repo, branch, commit, paths, the exact error and what you want back. Pass transcript addresses in `refs` instead of pasting long output. Write whole sentences: a line like `Retention prepares privately on46b538+frozen4f4 now.` needs the sender's transcript to decode, while `I am preparing the retention change (#2021) locally, on your 46b538ec deploy and the frozen 4f4ecb head; nothing of mine runs on the shared stack.` does not.
- `intent=request` expects a reply; `inform` does not; `done` closes the thread and gets no answer.
- Never ask a peer to do something that was denied in your own session.

## After you send

The result is a receipt, not a reply. A reply arrives in your context on its own, at your next tool call or with your user's next prompt. Do not poll `flopwire_peers` or `flopwire_inbox`, and do not send "are you done?". For a request, follow the receipt's `next`: it says whether to keep working, wait briefly, or tell your user. Use `flopwire_inbox` only to check a sent message's state or re-read a thread. A `read` state means the message's text entered the recipient's context; it does not mean the recipient acted on it or will answer.
