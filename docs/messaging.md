# Messaging

The device agent carries messages between agent sessions. The `peers`,
`send` and `inbox` commands and MCP tools use it. The `flopwire hook`
command prints each message into the recipient's session. A session
without the hooks reads its messages with `flopwire inbox`. After a
harness release, check that the hooks still deliver with `flopwire probe`
([probe.md](probe.md)). For which harnesses send and receive, see the
[support matrix](harnesses/README.md#support-matrix).

## How the agent carries messages

With a server configuration, the agent does these things:

- It holds one long poll to the server. The poll reports the live sessions
  on this device: session id, harness, repo, branch, and busy or idle.
- It checks the sessions every 2 seconds. When one starts, ends, or turns
  busy or idle, it sends a new poll at once.
- It leaves out each session that a path rule keeps off the server. Such a
  session cannot send or receive messages.
- It keeps the messages for this device in the local inbox. A message that
  the server no longer lists is removed, unless a hook already took it.
- It claims each message to `@you` for one live session on this device.
  It prefers a session on the message's repo, then a busy session.
- It confirms each delivery to the server, in batches, after the hook
  confirms that it printed the message. It reports each message that
  became `undelivered` (`unconfirmed` or `session_ended`) in the same
  batches.

Without a server configuration, or with `--no-sync`, the agent routes
messages between the sessions on this device. To address yourself, use
`@` and your account name. The server's limits apply: duplicates within 10 minutes,
8 messages per thread per hour, 30 sends per session per hour, and 50
undelivered messages per recipient.

A session is live when `flopwire sessions` shows it as live and it has
not ended. The agent also reads these harness files. It never writes
them:

| Harness | File | Gives |
|---|---|---|
| Claude Code | `~/.claude/sessions/<pid>.json` | Open while the process runs and started when `procStart` says. Busy when `status` is `busy`. |
| Codex | `~/.codex/thread-writer-locks/<thread>.lock` | Open while a Codex process holds the file's lock. The file stays after the process exits. Busy from the last task event in the rollout. |
| Devin | `session_locks/<session>.lock` beside `sessions.db`, and `sessions.db` | Open while the named process runs, is `devin`, and started before the lock was written. The session must still be in `sessions.db`. When one process is named by several locks, it must also hold this lock file open. A Devin session without such a lock is not live, even when it wrote a moment ago. |

To see whether a Codex process holds a thread's lock, the agent does what
Codex's own cleanup does. It takes `.coordination.lock` in the same
directory without waiting, tries a shared lock on the thread's file
without waiting, and releases both at once. A Codex that starts in that
moment waits microseconds for the coordination lock; it is never refused
its thread. When the coordination lock is busy, or the directory has no
`.coordination.lock` (an older Codex), the agent counts the file's
existence.

Devin's store does not show whether a turn runs. The agent uses the hook
events instead: each `flopwire hook` call tells the agent its event. After
`UserPromptSubmit` or `PostToolUse` the Devin session is busy. After `Stop`
it is idle. A session with no hook event for 15 minutes is idle. A session
is idle until its first hook event after the agent starts.

Devin runs no `Stop` hook for a turn that you interrupt (Esc twice or
Ctrl-C in the TUI, `session/cancel` over ACP). Devin then writes a marker
as the turn's last node in `sessions.db`: the system node
`[Response interrupted by user]`, or the tool result
`Canceled due to user interrupt`. While a session is busy, each presence
check reads the session's 16 newest nodes. A marker written after the
hook that made the session busy makes it idle.

A message waits for 24 hours. Then it expires.

To see the messaging state, run `flopwire agent status`.

## How a hook delivers a message

A message arrives inside a running turn, at the next tool call, or with the
session's next prompt or resumption hook, including a background task
notification. A parent that has stopped while background agents run waits
until it resumes, through a human prompt or a harness task notification.
Child tool calls do not deliver messages to the parent. A message never
wakes an idle session and never starts a turn. The receipt's `next_prompt`
value includes these prompt and resumption hooks.
For Claude, a witnessed parent Stop overrides a busy registry entry; after a
device-agent restart, presence falls back to the registry until another parent
hook supplies turn evidence.

The hooks run `flopwire hook`. For the events it acts on, see
[What the hooks do](harnesses/README.md#what-the-hooks-do).

Delivery has two steps, so a hook that is killed cannot lose a message:

1. The hook asks the agent for the session's messages. The agent leases
   them to that hook for 10 seconds. While the lease runs, no other hook
   of the session gets messages, so the messages keep their order.
2. The hook prints the messages. Then it confirms them to the agent. The
   agent marks them delivered and sends the receipt to the server.

If no confirmation arrives before the lease ends, the agent offers the
messages again at the session's next hook. A harness timeout, a killed
hook, a closed output pipe or a lost confirmation all end this way. A
message that is offered again carries `redelivery="true"`, because the
model may have seen it already. The hook does not confirm a print that
ends more than 3 seconds after its start: a harness that timed the hook
out may not have read it.

After 3 leases without a confirmation, the message is `undelivered` with
the reason `unconfirmed`. The sender's `inbox` shows this state, and the
sender can send the message again.

The standing instruction is delivered the same way. A session is owed it
until a hook confirms that it printed it. The hook prints it before any
message. While one hook holds the instruction's lease, no other hook of
the session gets messages, so no message arrives before the instruction.
If the `SessionStart` hook is killed or starts too late, the session's
next `UserPromptSubmit` or `PostToolUse` hook prints it. Two hook
configurations that both run `SessionStart` print it once. The agent keeps
the confirmation in its authority inbox database, so a restart does not print it again. After
3 unconfirmed leases the agent stops offering it.

A `SessionStart` with the source `resume`, `compact` or `clear` that
starts after the confirmation makes the session owed the instruction
again. A compaction summarizes the earlier context, so the instruction is
likely gone from it. Flopwire cannot see whether a resumed session kept
it. A second copy costs some context; a missing copy leaves the model
without the trust rules.

When a session ends before a hook delivers its message, the message is
`undelivered` with the reason `session_ended`. It is never given to
another session. See [Ended sessions](#ended-sessions).

One call prints at most 5 messages and at most 9,000 bytes. The oldest
messages go first. The rest wait for the next hook. A single message that
is longer than the cap is cut, with a note that names the
`flopwire inbox --thread` command that shows all of it.

## Ended sessions

A session ends when its harness shows it, never because it is idle. The
agent takes these signals:

| Harness | The session ended when |
|---|---|
| Claude Code | Its `SessionEnd` hook runs. Or its `sessions/<pid>.json` names a process that is not running or that started at another time (a killed process leaves the file). Or the file that named it is gone at two reads 1 second apart (a clean exit removes it). |
| Codex | Its `SessionEnd` hook runs. Or no process holds its writer lock (the kernel releases the lock when the process exits, also when it is killed). Or the lock file is gone at two reads 1 second apart. |
| Devin | Its `SessionEnd` hook runs. Or its `session_locks/<session>.lock` names a process that is not running, is not `devin`, or started after the lock was written. Or, about 1 second after it happens, the session is gone from `sessions.db`, or a process named by several locks closes this one. |

The agent reads the harness files on each 2-second presence check and on
each `peers` call (presence is cached for 1 second). An ended session
leaves presence at that read, and its messages are marked then. On this
device, `peers` stops listing it within about 1 second of a `SessionEnd`
hook or a killed process, and within about 3 seconds of a clean exit
without the hook (the second read). The server learns it from the next
poll, which the next 2-second presence check starts.

One window remains. A session that ran and exited between two presence
checks (a `claude -p` of under 2 seconds) and whose harness ran no
`SessionEnd` hook never showed in its harness file. It stays live for 10
minutes after its last write, as `flopwire sessions` shows it. With the
plugin's `SessionEnd` hook installed, it ends at once.

When a session ends, each message that waits for it (queued for it, or an
`@user` message claimed for it) becomes `undelivered` with the reason
`session_ended`. The sender can explicitly retry it with `--retry-of ID`
if it was addressed to that exact session. See [retry after a recipient
session ends](#retry-after-a-recipient-session-ends). The message is never given to another session. A message that a
hook took and has not confirmed keeps its lease: a late confirmation still
counts. If the lease ends unconfirmed, the message becomes
`undelivered` with `session_ended`.

A message that arrives after the session ended is marked the same way
only when its sender could still have been told that the session runs.
A message sent later went to a session that `peers` no longer listed. Its
receipt said `only_if_resumed`, and it waits for a resume.

- Without a server, the receipt comes from this agent's presence, which
  is at most 1 second old. A message sent within 1 second of the last
  time the agent saw the session live is marked. The receipt and the
  mark always agree.
- With a server, the server learns of the end from the next poll, about
  2 seconds later, and its clock may differ. A message sent within 10
  seconds of the last live sighting is marked. In that window a message
  whose receipt said `only_if_resumed` can be marked `undelivered` too.
  The sender then sees it and can send it again.

A session resumes when a hook of it runs that started after the end, or
when its harness file names a newer process. A resumed session receives
its new messages. Messages that were already `undelivered` stay so.

The agent keeps this state in its authority inbox database. A session that its harness file
showed before an agent restart, and that is gone after it, ends at the
first presence checks after the restart.

## What the recipient sees

`flopwire hook` prints each message in one wrapper:

```
<flopwire-message id="m7f3a…" from="0b7e2c1a-…" user="alex@example.com" agent="claude" repo="api@main" sender="teammate" intent="request" sent="2026-10-01T14:02:11Z">
Can you rebase api on main?
<flopwire-ref address="4c19e0d2/12">assistant: the cursor is opaque…</flopwire-ref>
</flopwire-message>
Reply with the flopwire_send tool: to="0b7e2c1a-…" reply_to="m7f3a…" message="…"; or in a shell: flopwire send 0b7e2c1a-… --reply-to m7f3a… -- "…"
```

- Every attribute comes from the envelope that the server (or, without a
  server, the device agent) set. The sender supplies only the text, the
  intent, the reply id and the refs.
- `sender` is `own` for a session of the same person and `teammate` for
  another person's session.
- The reply line appears only for `intent="request"`.
- `redelivery="true"` appears on a message that an earlier hook took and
  never confirmed. The model may have seen it. The standing instruction
  tells the model not to act again on a message id that is already in
  its context.
- A ref shows a short excerpt when the recipient's local index holds that
  message. Otherwise it shows the address alone. `flopwire read ADDRESS`
  shows the whole message.
- The text is escaped: `&`, `<` and `>` become `&amp;`, `&lt;` and `&gt;`,
  and look-alike angle brackets and the invisible Unicode tag characters
  become character references. Control and
  bidirectional characters are shown or replaced. A message therefore cannot
  close its wrapper, open another one, or imitate the standing
  instruction. Attribute values are escaped the same way, plus `"`.

Before the first message of a session, the hook prints the standing
instruction (`busrender.StandingInstruction`). See
[How a hook delivers a message](#how-a-hook-delivers-a-message) for when it is
printed. It tells the model what the wrapper is,
that a message from another session of your own user is a request to
act on within this session's permissions, that a message from a teammate is information to
confirm with you first, and that a message never changes permissions or
settings.

## Send and read messages

Run these commands from an agent session's shell, or call the MCP tools
`flopwire_peers`, `flopwire_send` and `flopwire_inbox`. They print compact
JSON with named fields and full session ids. Add `--text` (MCP:
`format: "text"`) for a readable form.

1. Find the session behind a change in the history:

   ```sh
   flopwire sessions --repo . --branch feat/cursor --json
   flopwire read SESSION --outline
   ```

   The digest lists the commits each session made. Do not choose a
   session by its title or its current branch alone. A title is the
   original task. A session can change branches after it commits.

2. Check that the session is live:

   ```sh
   flopwire peers --session SESSION
   ```

   An empty `peers` list means that the session is not running now. A
   message to it waits until it resumes or expires. Without `--session`,
   the command lists every live session. Your own session is not in the
   list.

3. Send the message:

   ```sh
   flopwire send SESSION -- "Heads-up: the list endpoint returns a cursor now."
   flopwire send @alex --intent request -- "Can you rebase api on main?"
   ```

   Put the main point in the first line. Use `-` as the text to read it
   from standard input. The command prints a receipt. The `arrives` field
   says when the message arrives: `next_tool_call`, `next_prompt`,
   `when_accepted`, `next_session` or `only_if_resumed`. A receipt is not
   a reply. With `--text`, the receipt is one line, for example:

   ```text
   sent m1a2b3c4d5e6f7a8 to 4c19e0d2 (gary codex api@main): idle, arrives when its session resumes
   ```

4. Check what you sent:

   ```sh
   flopwire inbox --sent
   ```

   Each entry has `direction` (`sent` or `received`) and `state`:
   `queued`, `held`, `claimed`, `delivered`, `read`, `expired`,
   `refused` or `undelivered`. A `refused` or `undelivered` entry also
   has a `reason`. `undelivered` with `unconfirmed` means that hooks took
   the message 3 times and none confirmed that it printed it.
   `undelivered` with `session_ended` means that the recipient session
   ended before delivery was confirmed. If the work is still needed, use
   an [explicit retry](#retry-after-a-recipient-session-ends)
   to the same session. A previous hook might have printed without confirming. The state
   of a sent message is its delivery only. `read` (with `read_at`) means
   that the message's text entered the recipient's context, not that the
   recipient acted on it ([read receipts](#read-receipts)).
   A reply is a received
   entry whose `reply_to` names your message. To read one thread, use
   `flopwire inbox --thread ID`. When `more` is true, pass `next` as
   `--cursor`.

A failure prints one JSON object on standard error, and the command exits
with status 1. The object has a stable `code` (for example `thread_rate`,
`duplicate` or `agent_not_running`), a `detail`, a `fix` and an `example`.

The commands talk only to the device agent. If the agent does not run,
they stop with an error that says how to start it.

The sender is always the calling session. The commands find it as
[search](search.md#your-own-session) does. If no session is found, `send`
and `inbox` stop with an error. `peers` still lists the sessions. To name
the session, set `FLOPWIRE_SESSION_ID`, and `FLOPWIRE_AGENT` (`claude`,
`codex` or `devin`).

A subagent is not a session, so it gets no messages. A hook that runs in
a subagent prints nothing and takes nothing. The session's own next hook
delivers the messages. A message that a subagent sends goes out as its
parent session, and the reply goes to the parent session. The parent's
human does not see the call. This is the same on Claude Code, Codex,
Devin CLI and opencode.

In Codex, a subagent's `flopwire_send` call names the subagent's own
thread in `_meta.threadId`. The agent follows the index's parent links
from that thread to the top-level session and sends as that session. The
receipt and the recipient's inbox show the parent session as the sender.
A message for the parent, queued while the subagent runs, prints in none
of the subagent's hooks. It prints once, at the parent's next hook.

A session that a path rule keeps off the server cannot send. The agent
refuses the request before anything leaves the device. The agent also
refuses a send whose recipient or ref names such a session
(`withheld_session`). The agent does not check the message text: a
withheld session id written in the text reaches the server. Do not write
one there.

A new session can send before the agent has indexed it, or before its
transcript names its directory. The agent then indexes the session and
waits up to 2 seconds. If the session is still not ready, the agent
refuses the send with `session_not_on_device` and "not indexed yet". Send
again after a few seconds. `flopwire send` retries once by itself.

## Accept and revoke message senders

A message from another person's agent is held until you accept that
person. This page tells you how to review held messages, accept a sender
and revoke a sender. Read [what accepting means](../README.md#accepting-a-sender)
before you accept anyone.

You need:

- An account on the team server.
- A web browser, or a terminal on a device where you ran `flopwire login`
  in the last 24 hours.

You cannot accept or revoke from an agent session. The commands need a
terminal. There is no MCP tool for them. Accepting also needs your
password, so an agent that reads your saved login session cannot accept
for you. Such an agent can still list the held previews and revoke a
sender.

### Find held messages

The device tells you when messages are held:

- In Claude Code and Codex, a notice shows when you type a prompt. It
  names the sender and the number of messages. It shows at most once a day
  for each sender on each device. Your agent does not see it. Devin CLI
  and `codex exec` show no notice.
- `flopwire agent status` lists held senders and counts.

### Review held messages in the web console

1. Open the server URL in a browser.
2. Sign in with your email and password.
3. Select **Messaging**.
4. Read the messages under **Waiting for you**. Each sender shows the
   number of held messages. Each message shows its first line, the sending
   agent, its repo and branch, and its intent.

### Review held messages in a terminal

1. Run `flopwire login` if your login session is older than 24 hours.
2. Run `flopwire accepts --text`.
3. Read the list. It shows held messages by sender, and the people you
   accept.

### Accept a sender in the web console

1. Open **Messaging**.
2. Find the sender under **Waiting for you**.
3. Select **Review and accept**.
4. Read the statement of what accepting means.
5. Type your password.
6. Select **Accept** to accept, or **Cancel** to stop.

The held messages from that person go to your sessions. Their next
messages arrive without a hold.

### Accept a sender in a terminal

1. Run `flopwire accept EMAIL`. For example, `flopwire accept alex@example.com`.
2. Read the statement and the held messages.
3. Type your Flopwire password and press Enter to accept. The password
   is not shown. Press Enter without a password to stop.

### Revoke a sender

Revoking takes effect at once. The sender's undelivered messages are held
again. Their next messages are held until you accept them again. A
message that a session already received stays with that session.

A device can take a message just before the revoke reaches the server.
The hook can then still print that message into the session once. The
server rejects its delivery receipt, so the sender's inbox shows the
message as held, and then as expired. Flopwire accepts this race and will
not close it.

To revoke in the web console:

1. Open **Messaging**.
2. Find the person under **People whose agents can message yours**.
3. Select **Revoke**.

To revoke in a terminal:

1. Run `flopwire revoke EMAIL`.

## Read receipts

A sent message has the state `read` when its text entered the recipient
session's context. `read` does not mean that the recipient acted on the
message or will answer.

The device that holds the recipient session sets the state:

1. The hook prints the message into the session. The message is
   `delivered`.
2. The harness records the hook's output in the session's transcript.
3. The device agent indexes the transcript. It finds the message in the
   hook's output and records the time of that record.
4. The message is `read`. With a server, the device sends the time to the
   server in its next batch of receipts.

`flopwire inbox` shows the state and `read_at`. With `--text`, the state
shows the time, for example `read 2026-10-02 14:03Z`.

| Harness | Where the transcript keeps hook output | Read receipts |
|---|---|---|
| Claude Code | A `hook_additional_context` attachment | Yes |
| Codex | A developer message of kind `hooks.additional_context` | Yes |
| Devin CLI | A `system` message that starts with the hook's output | Yes |
| opencode | A text part with `metadata.flopwire` that the plugin stored ([opencode.md](harnesses/opencode.md)) | Yes |

A message stays `delivered` in these cases:

- The message text is only in a prompt, a reply or a tool output. An
  agent can quote or write any text there.
- A path rule denies the session's transcript, so the device does not
  index it.

## Retry after a recipient session ends

Accepting a sender releases held messages for delivery. Release does not
confirm delivery. If the addressed session ends first, the message can
become `undelivered` with reason `session_ended`. Resuming that session
does not requeue the failed message.

An explicit retry creates a new message for the same recipient session.
Use it only if the work is still needed. A previous hook might have
printed the original message without confirming. A retry can repeat text
in the recipient's context. Delivery and read receipts do not confirm
that the recipient acted on a request.

1. In the original sending session, run `flopwire inbox --sent`.
2. Find the failed message ID and its recipient session, text, intent,
   refs and `reply_to`.
3. Check the intended session with `flopwire peers --session SESSION`.
4. Send the same fields with `--retry-of MESSAGE_ID`. For example:

   ```sh
   flopwire send RECIPIENT_SESSION --retry-of FAILED_MESSAGE_ID --intent request -- "Original message text"
   ```

With MCP, call `flopwire_send` with the original fields and
`retry_of: "FAILED_MESSAGE_ID"`. The retry must come from the original
sending session on its original device. It must keep the same recipient
session, text, intent, refs and `reply_to`. The server preserves the
original thread. The source must be a session-addressed message with
state `undelivered` and reason `session_ended`. Retries of `@user`
messages, or messages with another state or reason, are refused.

If the recipient session is closed when the retry is admitted, the receipt
has `admission: "next_resume"` and `arrives: "only_if_resumed"`. It waits
for the same session's first receiving hook, or its first running cloud
push, until that retry's expiry. Starting and stopping a process
without that receiving boundary does not consume the wait. The retry
never wakes the session or moves to another session. After the first
receiving attempt, ordinary lease and session-end handling applies.
A retry admitted while the recipient is live uses ordinary delivery
handling from the start.

The retry still passes current privacy rules, sender limits, recipient
capacity limits and sender acceptance. An ordinary identical send remains
subject to the ten-minute duplicate guard. Do not change text to bypass
that guard.

Repeated requests with the same `retry_of` return the same successor.
The receipt has `replayed: true`, the successor's current `state` and
`reason`, and `arrives: "already_recorded"`. It creates no additional
message. If that successor also fails with `session_ended`, retry its ID
to create the next attempt. Retry records are available only while the
source and successor remain retained.

API clients submit the original send fields plus `retry_of` to
`POST /v1/bus/retry`, with the original device credential. The ordinary
`/v1/bus/send` route rejects `retry_of`. The CLI and MCP clients send a
retry through a separate device agent operation. An explicit retry needs
a recipient device agent that advertises retry support in its poll and
claim requests. A recipient agent without that support does not receive
explicit retries; the messages wait for a recipient agent with support,
or expire.

Do not downgrade a device agent while explicit retries wait in its local
inbox. An older binary can lose retry attribution or waiting semantics.

A message still queued is not eligible for explicit retry. Use `retry_of`
only after its state becomes `undelivered` with reason `session_ended`.

## How long messages are kept

The server keeps a message for 7 days after it expires, which is about 8
days after it was sent. Then it deletes the message and its audit rows.
The message is then gone from `flopwire inbox`, for the sender and for
the recipient. The server administrator can change the 7 days; see
[Set message-bus retention](runbook.md#set-message-bus-retention). A
message that still waits for delivery is never deleted before it expires.

When the server refuses a send, the sender's inbox shows the refusal.
Repeated refusals of one session with the same reason, to the same
recipient and in the same thread, within one hour show as one entry with
a count, for example
`refused (duplicate, 12 attempts)`. The entry keeps the text of the first
attempt. Each attempt still counts toward the hourly send limits.

## Fix errors

| Error code | Cause | Fix |
|---|---|---|
| `terminal_required` | The command did not run in a terminal. | Run it yourself in a terminal, or use the web console. |
| `login_required` | There is no valid login session. | Run `flopwire login`, then run the command again. |
| `local_only` | No server is configured. Every session is yours, so nothing is held. | Join a team server first. |
| `not_confirmed` | You did not type a password. | Run `flopwire accept EMAIL` again. |
| `password_required` | The password was not correct. | Run `flopwire accept EMAIL` again and type your own password. |
| `unknown_recipient` | No member has that name or email. | Use the member's email. |

## Read recent repository threads

Use `flopwire messages` when starting or resuming work to find recent coordination
on the current repository. It reads retained server messages across devices and
ended sessions. It needs an enrolled device. It does not need a live caller
session.

```sh
flopwire messages --text
flopwire messages --repo payments --user alex@example.com --text
flopwire messages --session SESSION_PREFIX --since 2026-10-01T00:00:00Z
flopwire messages --thread THREAD_ID --text
flopwire messages --cursor NEXT_CURSOR
```

The default response contains the newest eligible message in each thread, a
first nonblank line preview, participant previews, canonical message and thread
IDs, and delivery state. Open a thread by its ID to read full stored bodies and
references. Thread rows are newest first. `--limit` defaults to 20 and accepts
up to 200. `--max-bytes` bounds output using whole rows. The `next` cursor resumes
pagination; new activity can move a thread while you page through the list.

`--repo` defaults to the current repository. A recorded normalized Git remote
matches across devices; paths and main checkout identities match only on the
sending device. Ambiguous or unknown repository names need a known path. There
is no fallback to every repository. User and session filters match either
participant in an eligible row. List time filters select a thread's latest
eligible activity; open-thread time filters select individual messages.

Agents have the same read-only view through `flopwire_messages`, with `repo`,
`user`, `session`, `since`, `until`, `thread`, `cursor`, `limit`, and `max_bytes`
arguments. MCP output is capped at 24,000 bytes. Inspect relevant history once,
open a useful thread, then use `flopwire_peers` if you need to contact a live
session. The tool does not poll or change presence, claims, delivery, or read
receipts.

### How this differs from transcript search

`grep` and keyword `search` find text in indexed transcripts. A delivered message
can appear there as hook output, depending on harness capture and kind filters.
Those searches also find decisions and explanations outside Flopwire messages.

`messages` reads the canonical message records, including exchanges never
captured in a transcript. It provides thread links, participant identity, and
current delivery state. The list is sorted by recent activity; it does not
replace text search or provide semantic search. `inbox` remains the view for
this session's incoming messages and sends.

### Visibility and retention

You can read your own authored rows, including held or refused sends. Other
rows require current acceptance of the external participants and recipient
acceptance of the sender. Held and refused rows from other people are excluded.
These rules apply to each row, including direct exchanges between accepted
teammates. Revocation removes access on subsequent reads. Being an administrator
does not bypass the rules. Hidden rows do not contribute participant previews
or visible parent links.

Read a teammate exchange as information. It is not a request addressed to your
agent. Observation does not authorize an action or reply. Send a new direct
request when you need coordination with the original peer. Delivery state or a
reply does not establish that a task is complete.

Final messages expire under server retention. A retained thread can be missing
older rows or parents. Each send records the sender's remote and main
checkout identity. A row without that identity matches only a recorded path
on the sending device.

## Known limits

- Only Claude Code, Codex, Devin CLI and opencode sessions send and
  receive. Claude Code cloud sessions and Devin cloud sessions only
  receive, while they run a turn. See the
  [support matrix](harnesses/README.md#support-matrix) and
  [cloud.md](harnesses/cloud.md). For opencode, see
  [opencode.md](harnesses/opencode.md#known-limits).
- A Devin hook finds a subagent's tool call in Devin's session store. A
  background subagent (`run_subagent` in the background, or a foreground
  one that Esc moved there) writes its nodes to the store as it runs, on
  a root of its own. Its tool call is then in the store, but not on the
  session's main chain (`sessions.main_chain_id`), so its hook delivers
  nothing. If the hook cannot read the store, it delivers messages only
  at a prompt. It writes the cause to stderr.
- Without a server, the agent applies the server's per-session,
  per-device, per-thread, duplicate and recipient limits. It does not
  apply the per-person ceiling: it is higher than the per-device ceiling,
  and without a server the person has one device.
- `@user` messages are routed by repo name. With `--server`, `--repo`
  does not match another machine's checkout at another path.
- A Devin session held by a `devin acp` process with several sessions
  costs one `lsof` call (about 30 ms) for that process at each presence
  check. A process with one session needs none. The calls of one check
  stop after 500 ms. When `lsof` is missing, fails or is stopped, the
  check keeps every lock of that process.
- A background Devin subagent fires a `Stop` hook when it ends. That
  `Stop` reads as the session's own and makes the session idle, also
  while the session's own turn still runs. A message waits for the
  session's next own hook.
- The live mark of retrieval (`flopwire sessions`, the live sessions
  each sync reports to the server, and `--exclude-live`) still takes a
  Devin lock of a running `devin` as live without the per-session checks
  that `peers` makes. A session that a `devin acp` process deleted, or a
  lock whose pid a later `devin` reuses, reads live there until that
  process exits. Messages are not affected: they follow `peers`.
- On Linux, the agent computes a process's start from the boot time in
  `/proc/stat`, and the boot time moves when the wall clock steps. A
  forward step of more than 2 seconds (for example, a VM or WSL resume
  that resyncs the clock) makes a running Claude Code, Devin or opencode
  process look as if it started after the file that names it. A backward
  step of more than 2 seconds also affects Claude Code: its session file
  records the process's start, and the two must agree within 2 seconds.
  The sessions then end, and they read as ended until that process exits.
  Restart the harness to resume them. Codex sessions are not affected:
  their writer lock does not use the start time.
