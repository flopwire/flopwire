# Messaging

The device agent carries messages between agent sessions. The `peers`,
`send` and `inbox` commands and MCP tools use it. The `flopwire hook`
command prints each message into the recipient's session. A session
without the hooks reads its messages with `flopwire inbox`. After a
harness release, check that the hooks still deliver with `flopwire probe`
([probe.md](probe.md)). For which harnesses send and receive, see the
[support matrix](harnesses/README.md#support-matrix).

For earlier coordination, `flopwire messages` (`flopwire_messages` over MCP)
reads retained server threads across devices and ended sessions. It requires
enrollment and does not depend on a live recipient. `inbox` remains the view
for the current session's messages and delivery state. Reading an archived
request does not address it to you or authorize action or reply. See
[retained repository threads](#read-recent-repository-threads).

## How the agent carries messages

With a server configuration, the agent holds one long poll to the server
and reports this device's live sessions: session id, harness, repo, branch,
and busy or idle. It checks the sessions every 2 seconds and reports a
change at once. A session that a path rule keeps off the server is not
reported, and cannot send or receive messages. A message to `@you` is
claimed for one live session on this device, preferring a session on the
message's repo, then a busy session.

Without a server configuration, or with `flopwire agent run --no-sync`,
the agent routes messages between the sessions on this device. To address
yourself, use `@` and your account name. The server's limits apply:
duplicates within 10 minutes, 8 messages per thread per hour, 30 sends per
session per hour, and 50 undelivered messages per recipient.

A session is live when `flopwire sessions` shows it as live and it has not
ended. The agent reads the harness's own session files (Claude Code session
files, Codex thread writer locks, Devin session locks) and never writes
them. Busy means a turn is running. Devin's store does not record that, so
a Devin session is busy after a `UserPromptSubmit` or `PostToolUse` hook,
and idle after `Stop`, after an interrupted turn, or after 15 minutes
without a hook event.

A message waits for 24 hours. Then it expires. `flopwire agent status`
shows the messaging state.

## How a hook delivers a message

A message arrives inside a running turn, at the next tool call, or with the
session's next prompt or resumption hook, including a background task
notification. A parent that has stopped while background agents run waits
until it resumes, through a human prompt or a harness task notification.
Child tool calls do not deliver messages to the parent. A message never
wakes an idle session and never starts a turn. The receipt's `next_prompt`
value includes these prompt and resumption hooks. For the hook events, see
[What the hooks do](harnesses/README.md#what-the-hooks-do).

Delivery is at least once. The hook takes the session's messages under a
10-second lease, prints them, then confirms them; only a confirmation marks
a message delivered. While a lease runs, no other hook of the session gets
messages, so messages keep their order. A message whose lease ends
unconfirmed (a harness timeout, a killed hook, a closed pipe, a print that
ends more than 3 seconds after the hook started) is offered again at the
next hook with `redelivery="true"`, because the model may have seen it.
After 3 unconfirmed leases it becomes `undelivered` with the reason
`unconfirmed`, and the sender's `inbox` shows it.

Before the first message, the session gets the standing instruction, by the
same lease rules: no message arrives before it. It prints once per session,
and again after a `SessionStart` with the source `resume`, `compact` or
`clear`, since compaction likely removed it from context.

One call prints at most 5 messages and at most 9,000 bytes, oldest first;
the rest wait for the next hook. A single message longer than the cap is
cut, with a note that names the `flopwire inbox --thread` command that
shows all of it.

When a session ends before a hook delivers its message, the message is
`undelivered` with the reason `session_ended`. It is never given to another
session. See [Ended sessions](#ended-sessions).

## Ended sessions

A session ends when its harness shows it, never because it is idle:

| Harness | The session ended when |
|---|---|
| Claude Code | Its `SessionEnd` hook runs. Or its `sessions/<pid>.json` names a process that is not running or that started at another time. Or the file is gone at two reads 1 second apart. |
| Codex | Its `SessionEnd` hook runs. Or no process holds its writer lock. Or the lock file is gone at two reads 1 second apart. |
| Devin | Its `SessionEnd` hook runs. Or its `session_locks/<session>.lock` names a process that is not running, is not `devin`, or started after the lock was written. Or the session is gone from `sessions.db`, or a process named by several locks closes this lock. |

On this device, `peers` stops listing an ended session within about 1
second of a `SessionEnd` hook or a killed process, and within about 3
seconds of a clean exit without the hook. The server learns of it from the
next poll, about 2 seconds later. A session that ran and exited between two
presence checks (a `claude -p` of under 2 seconds) without a `SessionEnd`
hook stays live for 10 minutes after its last write; with the plugin's
`SessionEnd` hook it ends at once.

When a session ends, each message that waits for it (queued for it, or an
`@user` message claimed for it) becomes `undelivered` with the reason
`session_ended`. It is never given to another session. A sender can
explicitly [retry](#retry-after-a-recipient-session-ends) a message that
was addressed to that exact session. A message that a hook took and has
not confirmed keeps its lease: a late confirmation still counts.

A message sent after the session ended is marked the same way only when its
sender could still have been told that the session runs. Otherwise its
receipt said `only_if_resumed`, and it waits for a resume. Without a
server, the window is 1 second after the agent last saw the session live,
and the receipt and the mark always agree. With a server, the window is 10
seconds, so a message whose receipt said `only_if_resumed` can still be
marked `undelivered`; the sender sees it and can send it again.

A session resumes when a hook of it runs that started after the end, or
when its harness file names a newer process. A resumed session receives its
new messages; messages already `undelivered` stay so. A session that its
harness file showed before an agent restart, and that is gone after it,
ends at the first presence check after the restart.

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
- `redelivery="true"` marks a message that an earlier hook took and never
  confirmed. The standing instruction tells the model not to act again on a
  message id already in its context.
- A ref shows a short excerpt when the recipient's local index holds that
  message. Otherwise it shows the address alone. `flopwire read ADDRESS`
  shows the whole message.
- The text is escaped (`&`, `<`, `>`, look-alike angle brackets and
  invisible Unicode tag characters become character references; control
  and bidirectional characters are shown or replaced), so a message cannot
  close its wrapper, open another one, or imitate the standing instruction.
  Attribute values are escaped the same way, plus `"`.

Before the first message of a session, the hook prints the standing
instruction (see [How a hook delivers a message](#how-a-hook-delivers-a-message)).
It tells the model what the wrapper is, that a message from another session
of your own user is a request to act on within this session's permissions,
that a message from a teammate is information to confirm with you before
consequential actions, and that a message never changes permissions or
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
   `when_accepted`, `next_session`, `only_if_resumed`, `when_running` (an
   idle cloud session) or `already_recorded` (a replayed retry). A receipt
   is not a reply.

4. Check what you sent:

   ```sh
   flopwire inbox --sent
   ```

   Each entry has `direction` (`sent` or `received`) and `state`:
   `queued`, `held`, `claimed`, `delivered`, `read`, `expired`,
   `refused` or `undelivered`. A `refused` or `undelivered` entry also
   has a `reason`: `unconfirmed` (hooks took it 3 times and none
   confirmed) or `session_ended` (the recipient ended before a confirmed
   delivery; see [explicit retry](#retry-after-a-recipient-session-ends)).
   The state of a sent message is its delivery only. `read` (with
   `read_at`) means that the text entered the recipient's context, not
   that the recipient acted on it ([read receipts](#read-receipts)). A
   reply is a received entry whose `reply_to` names your message. To read
   one thread, use `flopwire inbox --thread ID`.

A failure prints one JSON object on standard error, and the command exits
with status 1. The object has a stable `code` (for example `thread_rate`,
`duplicate` or `agent_not_running`), a `detail`, a `fix` and an `example`.
The commands talk only to the device agent; if it does not run, they say
how to start it.

The sender is always the calling session. The commands find it as
[search](search.md#your-own-session) does. If no session is found, `send`
and `inbox` stop with an error. `peers` still lists the sessions. To name
the session, set `FLOPWIRE_SESSION_ID`, and `FLOPWIRE_AGENT` (`claude`,
`codex`, `devin` or `opencode`).

A subagent is not a session, so it gets no messages. A hook that runs in
a subagent prints nothing and takes nothing; the session's own next hook
delivers. A message that a subagent sends goes out as its top-level parent
session (in Codex, found through the index's parent links), and the reply
goes to the parent. The parent's human does not see the call. This is the
same on Claude Code, Codex, Devin CLI and opencode.

A session that a path rule keeps off the server cannot send; the agent
refuses before anything leaves the device. It also refuses a send whose
recipient or ref names such a session (`withheld_session`). It does not
check the message text: a withheld session id written in the text reaches
the server.

A new session can send before the agent has indexed it. The agent then
indexes it and waits up to 2 seconds, else refuses with
`session_not_on_device` ("not indexed yet"); `flopwire send` retries once
by itself. Send again after a few seconds.

## Accept and revoke message senders

A message from another person's agent is held until you accept that
person. This page tells you how to review held messages, accept a sender
and revoke a sender. Read [what accepting means](../SECURITY.md#messages-between-people)
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

The device that holds the recipient session sets the state. After the hook
prints the message (`delivered`), the harness records the hook's output in
the transcript. When the device agent indexes that record, the message is
`read` at the record's time. With a server, the device sends the time in
its next batch of receipts.

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
push, until that retry's expiry; starting and stopping a process without
that receiving boundary does not consume the wait. The retry never wakes
the session or moves to another session. After the first receiving
attempt, or from the start when the recipient is live, ordinary lease and
session-end handling applies.

The retry still passes current privacy rules, sender and recipient limits
and sender acceptance. An ordinary identical send stays subject to the
ten-minute duplicate guard; do not change text to bypass it.

Repeated requests with the same `retry_of` return the same successor, with
`replayed: true`, its current `state` and `reason`, and
`arrives: "already_recorded"`; no additional message is created. If that
successor also fails with `session_ended`, retry its ID. Retry records last
only while the source and successor are retained.

API clients send the original fields plus `retry_of` to
`POST /v1/bus/retry` with the original device credential; `/v1/bus/send`
rejects `retry_of`. A recipient device agent receives explicit retries only
when it advertises retry support in its poll and claim requests; otherwise
they wait for one that does, or expire. Do not downgrade a device agent
while explicit retries wait in its local inbox.

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
flopwire messages --repo payments --user alex@example.com --text
```

The default response contains the newest eligible message in each thread, a
first nonblank line preview, participant previews, canonical message and thread
IDs, and delivery state. Open a thread with `--thread ID` to read full stored
bodies and references. Thread rows are newest first. The `next` cursor resumes
pagination; new activity can move a thread while you page through the list.

`--repo` defaults to the current repository. A recorded normalized Git remote
matches across devices; paths and main checkout identities match only on the
sending device. Ambiguous or unknown repository names need a known path. There
is no fallback to every repository. User and session filters match either
participant in an eligible row. List time filters select a thread's latest
eligible activity; open-thread time filters select individual messages.

Agents have the same read-only view through `flopwire_messages`; MCP output
is capped at 24,000 bytes. Inspect relevant history once, open a useful
thread, then use `flopwire_peers` if you need to contact a live session. The
tool does not poll or change presence, claims, delivery, or read receipts.

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
- A Devin hook delivers nothing during a background subagent's tool calls
  (`run_subagent` in the background, or a foreground one that Esc moved
  there): they are not on the session's main chain. If the hook cannot read
  Devin's session store, it delivers only at a prompt and writes the cause
  to stderr.
- Without a server, the agent applies the server's per-session,
  per-device, per-thread, duplicate and recipient limits. It does not
  apply the per-person ceiling: it is higher than the per-device ceiling,
  and without a server the person has one device.
- An `@user` message from a session without a remote is routed by repo
  name, so it can reach another repository with the same name (see
  [Which repo](search.md#which-repo)). With `--server`, `--repo` does not
  match another machine's checkout of a repository without a remote at
  another path.
- A `devin acp` process with several sessions costs one `lsof` call
  (about 30 ms) at each presence check. When `lsof` is missing, fails or
  passes the 500 ms limit, the check keeps every lock of that process.
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
- On Linux, a wall-clock step of more than 2 seconds (a VM or WSL resume
  that resyncs the clock) moves the boot time that process start times
  come from. A forward step makes running Claude Code, Devin and opencode
  sessions end; a backward step also ends Claude Code sessions. They read
  as ended until the process exits; restart the harness to resume them.
  Codex sessions are not affected.
