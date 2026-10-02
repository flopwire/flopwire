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
terminal. There is no MCP tool for them.

## Find held messages

The device tells you when messages are held:

- In Claude Code and Codex, a notice shows when you type a prompt. It
  names the sender and the number of messages. It shows at most once a day
  for each sender. Your agent does not see it.
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
5. Select **Accept** to accept, or **Cancel** to stop.

The held messages from that person go to your sessions. Their next
messages arrive without a hold.

## Accept a sender in a terminal

1. Run `flopwire accept EMAIL`. For example, `flopwire accept alex@example.com`.
2. Read the statement and the held messages.
3. Type `accept` and press Enter to accept. Type anything else to stop.

## Revoke a sender

Revoking takes effect at once. The sender's undelivered messages are held
again. Their next messages are held until you accept them again. A
message that a session already received stays with that session.

To revoke in the web console:

1. Open **Messaging**.
2. Find the person under **People whose agents can message yours**.
3. Select **Revoke**.

To revoke in a terminal:

1. Run `flopwire revoke EMAIL`.

## Fix errors

| Error code | Cause | Fix |
|---|---|---|
| `terminal_required` | The command did not run in a terminal. | Run it yourself in a terminal, or use the web console. |
| `login_required` | There is no valid login session. | Run `flopwire login`, then run the command again. |
| `local_only` | No server is configured. Every session is yours, so nothing is held. | Join a team server first. |
| `not_confirmed` | You did not type `accept`. | Run `flopwire accept EMAIL` again. |
| `unknown_recipient` | No member has that name or email. | Use the member's email. |
