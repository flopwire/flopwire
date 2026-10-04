# Accept and revoke message senders

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

## Find held messages

The device tells you when messages are held:

- In Claude Code and Codex, a notice shows when you type a prompt. It
  names the sender and the number of messages. It shows at most once a day
  for each sender on each device. Your agent does not see it. Devin CLI
  and `codex exec` show no notice.
- `flopwire agent status` lists held senders and counts.

## Review held messages in the web console

1. Open the server URL in a browser.
2. Sign in with your email and password.
3. Select **Messaging**.
4. Read the messages under **Waiting for you**. Each sender shows the
   number of held messages. Each message shows its first line, the sending
   agent, its repo and branch, and its intent.

## Review held messages in a terminal

1. Run `flopwire login` if your login session is older than 24 hours.
2. Run `flopwire accepts --text`.
3. Read the list. It shows held messages by sender, and the people you
   accept.

## Accept a sender in the web console

1. Open **Messaging**.
2. Find the sender under **Waiting for you**.
3. Select **Review and accept**.
4. Read the statement of what accepting means.
5. Type your password.
6. Select **Accept** to accept, or **Cancel** to stop.

The held messages from that person go to your sessions. Their next
messages arrive without a hold.

## Accept a sender in a terminal

1. Run `flopwire accept EMAIL`. For example, `flopwire accept alex@example.com`.
2. Read the statement and the held messages.
3. Type your Flopwire password and press Enter to accept. The password
   is not shown. Press Enter without a password to stop.

## Revoke a sender

Revoking takes effect at once. The sender's undelivered messages are held
again. Their next messages are held until you accept them again. A
message that a session already received stays with that session.

A device can take a message just before the revoke reaches the server.
The hook can then still print that message into the session once. The
server rejects its delivery receipt, so the sender's inbox shows the
message as held, and then as expired. Flopwire accepts this race and will
not close it (#70).

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
| opencode | A text part with `metadata.flopwire` that the plugin stored ([opencode.md](opencode.md)) | Yes |

A message stays `delivered` in these cases:

- The message text is only in a prompt, a reply or a tool output. An
  agent can quote or write any text there.
- A path rule denies the session's transcript, so the device does not
  index it.

## Resend a message that was not delivered

A message waits for the session it was sent to, or for the session that
took an `@user` message. If that session ends before a hook delivers the
message, no other session gets it. The sender's inbox shows it as not
delivered.

1. In the sending session, run `flopwire inbox --sent --text`, or call
   `flopwire_inbox` with `sent: true`.
2. Find the entries with the state `undelivered`. The reason is in
   parentheses:
   - `session_ended`: the recipient session ended first.
   - `unconfirmed`: hooks took the message 3 times and none confirmed
     that it printed it.
3. Run `flopwire peers` to find a live session for the work.
4. Send the message again, to that session or to `@user`.

A message sent to a session that `peers` no longer lists is not marked
`undelivered`. Its receipt says `only_if_resumed`: it waits until that
session resumes, or until it expires after 24 hours.

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
