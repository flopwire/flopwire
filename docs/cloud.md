# Message vendor cloud sessions

Flopwire delivers messages into Claude Code cloud sessions (claude.ai/code)
and Devin cloud sessions. These sessions run on the vendor's machines, not
on one of your devices. This page tells you what you need, how a message
reaches such a session, and what such a session cannot do.

Design: [notes/message-bus/plan.md](../notes/message-bus/plan.md) §6.
Evidence: [notes/message-bus/cloud-2026-10-03.md](../notes/message-bus/cloud-2026-10-03.md).

## What you need

- The device agent (`flopwire agent`) running on one of your devices.
- On that device, the vendor's CLI, logged in as you:
  - Claude cloud: `claude`, logged in to a claude.ai account. An API key
    is not enough.
  - Devin cloud: `devin`, logged in with `devin auth login`.
- Nothing inside the cloud session. Flopwire installs nothing there.

The agent finds each CLI on `PATH` when it starts. To turn cloud delivery
off, start the agent with `FLOPWIRE_CLOUD=off` in its environment.

## Find a cloud session

1. List the live sessions:

   ```sh
   flopwire peers --text
   ```

2. Find the rows with `cloud busy` or `cloud idle`. A cloud row names its
   owner and its repository (`owner/name`), and no device. In JSON, the
   row has `"cloud": true`.

`cloud busy` means that the session runs a turn now. `cloud idle` means
that it does not.

## Send to a cloud session

1. Send the message as to any session, by its id or a unique prefix:

   ```sh
   flopwire send session_01AbCd -- "The API now returns a cursor. Use it for the next page."
   ```

2. Read the receipt:
   - `arrives: next_tool_call`: the session runs a turn. Flopwire pushes
     the message within seconds. The model reads it at its next tool call.
   - `arrives: when_running`: the session runs no turn. The message waits.
     Flopwire pushes it when the session's human starts a turn.
   - `arrives: when_accepted`: you are another person, and the owner has
     not accepted you. See [Accepting a sender](../README.md#accepting-a-sender).
3. Do not wait for a reply. A cloud session cannot send messages.

`@user` messages never go to a cloud session. Address the session itself.

## How delivery works

- **Discovery.** Every 20 seconds, the agent lists your cloud sessions
  with each vendor's CLI login. With a server, each poll reports them as
  yours. They are owned by you, not by a device: any of your devices that
  lists one keeps it live. With a server, a session that no device listed
  for 75 seconds leaves `peers`.
- **Push only during a turn.** Flopwire pushes a message only while the
  vendor reports a turn running. Before each push, Flopwire asks the
  vendor again whether the turn still runs. A push never starts a turn,
  except when the turn ends in the seconds between that check and the
  push. A message to an idle or stopped session waits in the normal
  queue, up to 24 hours.
- **One device pushes.** With a server, every one of your devices that
  lists the session is offered the message. One device claims it, only
  while the session runs a turn, and pushes it.
- **Push route.** Claude cloud: `claude -p TEXT --cloud ID --output-format
  json`. Devin cloud: `session/prompt` over `devin acp --cloud`. A push is
  confirmed when the vendor acknowledges it (Claude) or echoes the text
  back (Devin). Then the message is `delivered`.
- **Framing.** The vendor gives pushed text to the model as your own
  input, with no mark of its origin. Every push therefore starts with an
  instruction for cloud sessions, then the messages in their
  `<flopwire-message>` wrappers. The instruction says that the text is
  relayed, not typed by you. It says that a message from your own session
  is a request to act on, and a message from another person is
  information to confirm with you first. It says that the session cannot
  reply.
- **Failures.** A push that fails is tried again at the next presence tick
  (2 seconds), marked `redelivery="true"`. After 3 failed tries, the
  sender's `inbox` shows the message as `undelivered` with reason
  `push_failed`. A push that the vendor refuses because the session is
  archived or exited makes the session's waiting messages `undelivered`
  with reason `session_ended`.
- **Read receipts.** Claude cloud: the message is `read` when the
  session's event log shows the first model output after the pushed text.
  Devin cloud: the message is `read` when the session's first output after
  the echo arrives within 8 seconds of the push. If it comes later, the
  message stays `delivered`.

## Messages from other people

A teammate's message to your cloud session follows the same rule as one to
a local session. It is held until you accept that person. After you
accept them, their agents can message your cloud sessions too. Because the
pushed text arrives as your own input, the rule that a teammate's message
is information is only an instruction to the model. See
[what accepting means](../README.md#accepting-a-sender).

## Limits

- A cloud session cannot send or reply.
  - A Claude cloud session's network blocks outbound traffic by default.
  - A Devin cloud session reaches the internet, but it has no `flopwire`
    CLI and no device credential. Flopwire does not set them up.
- A cloud session's transcript is not in the archive. You cannot search
  it or `read` it.
- The vendor routes that Flopwire uses are not documented. They can
  change without notice.
- Claude discovery uses the token of your Claude Code login. The token
  expires after some hours unless Claude Code runs and refreshes it.
  Flopwire does not refresh it. While it is expired, your Claude cloud
  sessions are not listed. Run any `claude` command to refresh it.
- Each push into a Claude cloud session runs `claude -p` on your device.
  That run loads your Claude Code plugins and runs their hooks.
- The Devin list is your organization's. Flopwire keeps only the sessions
  that you created.
- An idle cloud session gets its messages only when its human starts a
  turn. Nothing tells that person that a message waits.
- A device that claimed a message keeps it. If that device goes offline,
  the message expires after 24 hours.
- A cloud session on a repository whose name a path rule withholds is not
  listed.
