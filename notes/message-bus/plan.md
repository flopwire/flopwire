# Message bus: v1 design

The design of Flopwire's message bus: decisions, the agent interface,
hook delivery, per-harness packaging and vendor cloud. Code comments cite
its sections as "plan §N". Evidence:
[`probes-2026-10-01.md`](../log/probes-2026-10-01.md) (hooks, Devin,
opencode, cloud) and the socket and queue tests summarized in section 2.
User-facing behaviour is in [`docs/messaging.md`](../../docs/messaging.md).

## 1. Decisions

| # | Decision |
|---|---|
| B1 | **Direct messages only.** No channels and no repo broadcast in v1. |
| B2 | **Address a session or a person.** A session is addressed by its session id prefix, the same key `sessions`, `grep` and `read` print. `@user` addresses a person. |
| B3 | **Explicit wake only.** Ordinary sends arrive through hooks. A send with `wake=true` may start a potentially billable turn in your own live, supported session. Wake does not propagate to replies. See [messaging](../../docs/messaging.md#explicit-wake). |
| B4 | **Authority depends on the sender.** From the recipient's own user: a teammate request, acted on within the recipient session's permissions. From another user: information; the agent confirms with its human before consequential actions. A message never changes permissions or settings. |
| B5 | **v1 harnesses:** Claude Code, Codex, Devin CLI, opencode. |
| B6 | **Vendor cloud in v1:** Claude cloud sessions and Devin cloud, pushed only while the session is running. An occasional wake from a send that races the end of a turn is accepted. See section 6. |
| B7 | A message from another user is held until the recipient's human accepts that sender once. Acceptance is per sender and revocable. |
| B8 | Undelivered messages expire after 24 hours by default. Flopwire never resumes a session headless to deliver. |

Accepted risks and limits:

- **Authority (#77).** Accepting a sender is the trust decision. An
  accepted sender's agents can direct the recipient's agents within each
  session's permissions, including sessions that skip permission prompts.
  The information-only rule for teammate messages (B4) is guidance to the
  model, not a boundary; smaller models do not follow it.
- **Idle recipients (#66).** An ordinary send waits until the recipient
  resumes. Explicit wake may start a turn and consume usage; its receipt
  confirms the request, not native submission or turn start.
- **Poll before revoke (#70): won't fix.** A device that took a message
  just before a revoke can still print it once. Its receipt is then
  rejected, so the sender sees it held, then expired.
- **Subagent sends.** On every harness, a subagent's send goes out as its
  parent session, and the parent's human does not see the call.
- **opencode mid-turn.** A message delivered during an opencode turn earns
  one extra model reply before the session goes idle, because opencode
  reads the stored message only after the current stream ends.
- **Cloud sessions in `peers`.** A teammate whom the user has not
  accepted still sees the user's cloud sessions in `peers`, as for local
  sessions. Acceptance gates delivery, not presence.
- **Undocumented surfaces.** The Claude cloud session list and the Devin
  CLI token on REST can change without notice. Claude Code and Codex ship
  several releases a week; `flopwire probe`, run by hand after a harness
  release, re-checks delivery ([`probe-runs.md`](../log/probe-runs.md)).
- **Desktop apps and IDE extensions** probably load the same hooks. They
  are not tested.

## 2. Why hooks, not the socket or the queue

Claude Code's inbox socket and `codex queue` both wake an idle session,
which B3 rules out. Pushing only while the session is busy does not fix
that: a send that lands as the turn ends starts a new turn, and on Codex a
queued item always runs as its own turn after the current one. A queue to
a running `codex exec` loses the message when the process exits.

Hooks have neither problem. The session pulls its own messages at points
where a turn is already running or a human has just typed, so no delivery
can start a turn. The same three hook events work on Claude Code, Codex and
Devin, with the same output format, and all were confirmed live. The
tradeoff is setup: hooks must be installed ahead of time. The install is
needed anyway, because the agent needs the send tools.

### Socket and queue: verified, not used

Verified 2026-09-28 on Claude Code 2.1.284 and codex-cli 0.158.0, by live
tests on throwaway sessions and by reading the Codex source at tag
`rust-v0.158.0`. Both are local to the machine, same-user only, and
internals that can change in any release. They are the route for an
opt-in wake, which v1 does not have.

- **Claude Code inbox socket.** Documented under "The session's inbox
  socket" at code.claude.com/docs/en/cross-session-messaging. The registry
  `~/.claude/sessions/<pid>.json` names `messagingSocketPath`,
  `sessionId`, `cwd`, `status` (idle or busy), `procStart` and `name`;
  read the path from it and check that the pid is alive and matches
  `procStart`. The socket is 0600 in a 0700 directory and takes
  newline-delimited JSON `{"type":"user","message":{"role":"user",
  "content":"..."}}`, with no response. An idle session wakes; a busy one
  reads the message between tool-call batches. The auth line is optional
  on macOS and Linux, so Claude Code does not authenticate same-user
  peers: any same-user process can forge the peer framing and skip the
  bypass-mode dialog. The Flopwire device agent, not the harness, enforces
  who may reach a session.
- **Claude Code MCP channels** need the server named in `--channels` at
  launch, so they need launch flags.
- **`codex queue --thread <UUID>`** (`thread/queue/add`). Every Codex
  process, including an in-process TUI, polls `~/.codex/queue_1.sqlite`
  every 10 s and starts a turn when the thread is idle; a busy thread runs
  the item as its own turn after the current one. The daemon is not
  needed. A queued item to a closed thread runs the moment the user
  resumes, before they type. Address by UUID: `--thread <name>` took 79 s
  and failed.
- **Codex `turn/steer` and `thread/inject_items`** work only for threads
  hosted on the shared app-server daemon
  (`~/.codex/app-server-control/app-server-control.sock`, JSON-RPC over a
  WebSocket). Many launch flags (`-c`, `--search`, `--profile`,
  `--no-daemon` and others) make the TUI run its own in-process server.

`cc_send.py` posts one message to a Claude session's socket.
`codex_rpc.py` is a JSON-RPC client for the Codex daemon (`loaded`,
`turns`, `steer`, `inject`, `queue`; needs `websockets`). Run them only
against throwaway sessions.

## 3. Agent interface

CLI verbs and MCP tools mirror each other, as the retrieval verbs do.

```
flopwire peers [--session ID] [--repo R] [--user U] [--agent A] [--limit N]
flopwire send <to> [--intent request|inform|done] [--reply-to ID]
              [--ref ADDRESS]... [--repo R] -- <text | ->
flopwire inbox [--sent] [--thread ID] [--limit N] [--cursor C]
```

MCP: `flopwire_peers {session?, repo?, user?, agent?, limit?}`,
`flopwire_send {to, message, intent?, reply_to?, refs?, repo?}`,
`flopwire_inbox {sent?, thread?, cursor?, limit?}`. On opencode the same
three are plugin tools, because an MCP server there cannot tell which
session called it.

**Output.** All three print compact JSON
by default, on the CLI and over MCP: busproto's field names, full session
ids, a `kind` field (`peers`, `send_receipt`, `inbox`, `error`), and
named fields that say whether more follows (`total`, `more`, `next`,
`hint`). `--text` (MCP: `format: "text"`) prints the readable forms
below. A failure is `{"kind":"error","error":{"code","detail","fix",
"example",…}}`: on stderr with exit status 1 on the CLI, `isError` over
MCP. The send result is a receipt (`kind: send_receipt`, `state`,
`arrives`), never a reply; inbox entries carry `direction` (sent or
received) and `is_reply`, so "delivered" never reads as "answered".
Over MCP each answer is one text block, with no `structuredContent` or
`outputSchema`, because Claude Code shows the model only the structured
part.

**Finding the recipient.** History first, then presence: `sessions
--repo R --branch B` and the session digest's commits name the session
behind a change; `peers --session ID` says whether that exact session is
live; then send to that id. A peer's title is its original task and its
branch is where it is now, so neither alone identifies who made a change.

### peers

One entry per live session, the caller's own user first, the calling
session left out. With `--text`, one row each, in the `sessions` header
shape:

```
0b7e2c1a alex claude live busy api@main "refactor client pagination"
4c19e0d2 gary codex  live idle api@main "add cursor to list endpoint"
```

`busy` means a turn is running, so a message arrives at the next tool call.
`idle` means it waits for that session's human.

### send

- `<to>` is a session id prefix or `@user`. An unknown or ambiguous prefix
  is an error that lists the candidates.
- `@user` with `--repo` (default: the caller's repo) goes to that person's
  live session on the repo. `--repo` names a repository: its main checkout
  and normalized remote, across worktrees and clones on the device. With none live, it waits for their next session
  on the repo, then their next session anywhere, until it expires.
- `--intent`: `request` expects a reply, `inform` (default) does not, `done`
  closes a thread and must not be answered.
- `--ref ADDRESS` attaches an archive address. It renders on delivery as the
  address plus a short excerpt; the recipient widens it with `read`.
- The first line of the text is the preview a human sees. Body cap: 4,000
  bytes, well under each harness's hook context cap (Claude Code 10,000
  characters, Codex 10,000 bytes, Devin over 1 MiB;
  [`hook-caps-2026-10-03.md`](../log/hook-caps-2026-10-03.md)). Longer
  material goes by `--ref`.

The receipt states the outcome (`arrives`: `next_tool_call`,
`next_prompt`, `when_accepted`, `next_session`, `only_if_resumed`), so the
sender never polls. A receipt for `intent=request` also carries `next`,
what to do until the reply comes; there is no blocking wait. With
`--text` it is one line:

```
sent m7f3a to 0b7e2c1a (alex claude api@main): busy, arrives at its next tool call
sent m7f3b to 4c19e0d2 (gary codex api@main): idle, arrives with its human's next prompt
held m7f3c for @sam: sam has not accepted messages from you
queued m7f3d for @alex: no live session on api; expires 2026-10-02T14:02Z
```

### inbox

Lists this session's threads, newest first: messages received, messages
sent and their state: `queued`, `held`, `claimed`, `delivered`, `read`,
`expired`, `refused` or `undelivered` (with `reason` `unconfirmed`,
`session_ended` or `push_failed`). `read` means the text entered the
recipient's context, not that it acted on it. It exists to check a sent
message and to re-read a thread. Delivery does not depend on the agent
calling it.

### What the recipient sees

```
<flopwire-message id="m7f3a" from="0b7e2c1a" user="alex" agent="claude"
  repo="api@main" sender="teammate" intent="request" sent="2026-10-01T14:02:11Z">
Heads-up: pagination is changing. Use the cursor returned by the API.
</flopwire-message>
Reply: flopwire_send to="0b7e2c1a" reply_to="m7f3a"
```

- Every attribute is set by the server from the sender's device credential.
  The sending agent supplies only the body, intent, reply id and refs.
- `sender` is `own` or `teammate`, computed from the two sessions' users.
- The body is escaped so it cannot close the wrapper or open a second one.
- The reply line appears only for `intent="request"`.

A standing instruction, delivered at session start on every harness, tells
the model what the wrapper is and states B4. Without it, three of three
models tested refused requests. The tested text is in the probes note.

### Loop and volume limits

Enforced by the server, reported to the sender as a refusal:

- A reply to a `done` message is refused.
- No per-session or per-thread hourly send cap.
- At most 120 sends per device and 300 per person per hour.
- A refused send counts toward these two ceilings like a sent one, so an agent looping on a refusal reaches them. A refusal by one of these two ceilings does not count: it would keep a retrying device or person locked out.
- Repeated refusals of one session with the same code, recipient and thread within an hour of the first are one stored row and one audit row, with `attempts` and `last_at`. The ceilings sum `attempts`.
- The same body to the same recipient within 10 minutes is dropped.
- At most 50 undelivered messages per recipient session.

## 4. Architecture

```
sender session ── flopwire_send ──► device agent ──► server ──► long-poll ──► device agent
                                                                                   │ local inbox
recipient session ◄── additionalContext ◄── flopwire hook ◄── control socket ◄─────┘
```

**Server.**
- Tables: `bus_messages (id, thread_id, from_session, to_session, to_user,
  repo, intent, body, reply_to, refs, created_at, expires_at, claimed_by,
  delivered_at, read_at, state, reason)` and `bus_accepts (recipient_user,
  sender_user, created_at)`, plus `bus_presence` and `bus_messages_seq`.
- Routes: `POST /v1/bus/send`, `GET /v1/bus/peers`, `GET /v1/bus/inbox`,
  `POST /v1/bus/poll` (it carries and writes presence), `POST
  /v1/bus/claim`, `POST /v1/bus/ack`, `GET /v1/bus/held`, and the
  accept/revoke routes `GET`/`POST /v1/bus/accepts` and `DELETE
  /v1/bus/accepts/{user}`, which need a human login session.
- Retention: a message in a final state (`delivered`, `read`, `expired`,
  `refused`, `undelivered`) is deleted 7 days after its expiry, with its
  `bus.send` and `bus.claim` audit rows; `bus.poll`, `bus.deliver` and
  `bus.read` audit rows go a day later by age. `FLOPWIRE_BUS_RETENTION`
  sets the 7 days. The sweep deletes at most 10,000 messages a minute. A
  deleted message leaves both inboxes; a late receipt for it is rejected.
- The sender's session id must belong to the calling device. Every send and
  delivery is audited.

**Server-to-device leg.** Sync is device-initiated HTTP only, so the
device agent holds one long-poll (`/v1/bus/poll`) that returns messages
for sessions on that device and `@user` messages for its user. The same
request carries the presence heartbeat; a session is live when reported
within 75 s.

**Device agent.**
- Keeps a local inbox in `bus.db`, its own SQLite file in the config dir,
  so a hook never waits on the network or on the index writer.
- Reports presence: session id, harness, repo and branch, busy or idle. It
  already tails every transcript; busy or idle comes from the last event,
  with the harness registries as a cross-check (Claude session files, Codex
  writer locks, Devin `isLocked`). Devin is busy after `UserPromptSubmit`
  or `PostToolUse`, idle after `Stop`, and idle after 15 minutes with no
  hook event.
- Ends sessions from what the harness shows, never from
  idleness or the 15-minute busy cap: a `SessionEnd` hook; a registry
  entry naming a process that is gone (a Claude session file of a dead or
  reused pid, a Codex writer lock nobody holds, a Devin lock of a dead or
  non-devin pid); an entry it had, missing at two reads 1 s apart. An
  ended session leaves presence at once. Its queued and claimed messages
  become `undelivered` (reason `session_ended`) and are reported in the
  next ack batch; they are never given to another session. A message that
  arrives later is marked too only when its sender could have been told
  the session is live: sent within 1 s of the last live sighting without
  a server (presence is cached 1 s, so this matches the receipt), within
  10 s with one (the server learns the end at the next poll, on another
  clock). A later one keeps the `only_if_resumed` receipt and waits for a
  resume. A hook
  that started after the end, or a newer process in the registry, resumes
  the session. State in `bus.db` (`devbus_sessions`).
- Claims an `@user` message for one live session with one atomic server
  call, so two devices cannot both deliver it.
- With no server configured, routes messages between sessions on the same
  machine itself.

**Hook.** `flopwire hook` flushes the session's source, as `flopwire agent
flush` does, and delivers.
- `SessionStart`, `UserPromptSubmit`, `PostToolUse`: anything pending,
  after the standing instruction while the session is owed it.
- `SessionEnd`: nothing printed; the flush tells the agent the session
  ended. Claude Code, Codex and Devin all have the event. Adding it to the
  Codex plugin made Codex ask to trust the new hook once.
- The standing instruction is leased like a message: owed until a
  hook confirms printing it, printed before any message, and while its
  lease is out no hook of the session gets messages. A killed or late
  `SessionStart` hook leaves it to the next hook. A `SessionStart` with
  source `resume`, `compact` or `clear` after the confirmation makes it
  owed again: a compaction summarizes it away, and Flopwire cannot see
  what a resumed context kept. The confirmation is kept per session in
  `bus.db`, so two hook configs and an agent restart print it once.
- It asks the device agent over the control socket for messages for the
  `session_id` on stdin, prints them as `additionalContext`, and then
  confirms them. It exits 0 with no output if the agent is down or slow
  (budget 200 ms). The pending request takes about 0.5 ms; the hook's wall
  time is process start, which a loaded machine stretches past 200 ms.
- Delivery has two steps. `pending` leases the messages to the hook
  (local state `leased`, 10 s). The hook writes its output and then sends
  `confirm` with the printed ids; only then is a message `delivered` and
  its receipt owed. While a lease of the session runs, no other hook of
  that session gets messages, so order holds across an expired lease and
  concurrent hooks (two hook configs) get each message once.
- An unconfirmed lease ends and the message is queued again. The next
  hook prints it with `redelivery="true"`; the standing instruction tells
  the model not to act again on an id it has seen. After 3 leases without
  a confirmation the message is `undelivered` (reason `unconfirmed`) on
  the device, the device reports it in its next ack batch, and the
  sender's `inbox` shows it. When the pending answer cannot be written
  (the hook left inside its budget), the agent requeues at once and the
  lease does not count.
- Impossible now: a message marked delivered that no hook printed (a hook
  killed or timed out between `pending` and its print, a broken stdout).
  Bounded: a message printed whose confirmation was lost is shown again,
  marked, at most 3 times in all. A hook older than 3 s (from its process
  creation) neither takes nor confirms: a harness that timed it out (the
  plugins give 5 s) may still hold its pipe open while no model reads it,
  and killing the `sh` of `flopwire hook || true` leaves the hook running.
  Its messages come again, marked.
- When a held cross-user message exists, it prints a user-visible notice,
  not model context.

**Sender identity.** Extends `internal/retrieval/local/caller.go`: Claude by
ancestor session file; Codex by `_meta.threadId` on the MCP call or
`CODEX_THREAD_ID`; Devin by parent pid matched to the lock file; opencode
by the plugin's `ctx.sessionID`.

**Receipts.** `delivered_at` when the hook confirms that it printed the
message (not when it takes it). `undelivered` with reason `unconfirmed`
when no hook confirmed it after 3 leases; the device reports it in the
ack request (`undelivered` ids) and the sender sees it. `undelivered`
with reason `session_ended` when its session ended first; the device
reports it in `session_ended` ids, and the server takes the report also
for a session already gone from the device's presence when no other
device holds it.

`read_at` when the message's wrapper appears in hook context
in the recipient session's transcript: its text entered the session's
context. It does not say that the model acted on it. The device agent
finds it while indexing, not the server: the device sees every row first,
withheld sessions included, and its ack batch already carries the
device's authority.

| Harness | Hook context in the transcript | Row |
|---|---|---|
| Claude Code 2.1.287 | `attachment` of type `hook_additional_context`, `content` one string per hook | injected, `hook_context` = the hook event |
| Codex 0.159.3 | developer message, `content_item_kinds: ["hooks.additional_context"]` | system (role developer), `hook_context` |
| Devin CLI 3000.11.1 | `role: "system"` node, like Devin's own system parts | system; counts only when it starts with the hook's output |
| opencode | a user text part the plugin delivered with `promptAsync(noReply)`, `metadata.flopwire` on the part | injected, `hook_context` = `flopwire-plugin` |

- Only `<flopwire-message id="…"` at the start of a line in those rows
  counts. Prompts, replies and tool output never do: an agent that
  `read`s another session's transcript sees its wrappers in tool output.
- The first sighting wins: a redelivery printed again changes nothing.
  A sighting while the message is still leased counts from its
  confirmation, so `read_at` is never before `delivered_at`.
- The device sends `read` events (`AckRequest.Read`) after the delivery
  receipt. The server sets `read_at` once, cut to `[delivered_at, now]`,
  only on a message delivered to that session and harness of the person,
  on a session the calling device holds; others are rejected. Audited as
  `bus.read`. Without a server the device sets it in `bus.db`.
- A hook inside a subagent (Claude Code, Codex or Devin) delivers
  nothing, and a wrapper in a subagent's transcript is that
  subagent's sighting, never its parent's.
- Codex encrypts its own inter-agent message bodies in rollouts, but it
  stores hook context as plain text, so Codex recipients get `read_at`.

**Permission.** Accept and revoke are human actions: the web console, or
the CLI on a terminal. There is no MCP tool for them and the CLI refuses
them when stdin is not a terminal. The terminal check alone does not
stop an agent of the same OS user, which can read the saved login session
or run the CLI under a pseudo-terminal, so accept also needs the person's
password, checked and rate-limited by the server. Revoke is one step and
also re-holds claimed messages. With the saved session alone an agent can
still list held previews and revoke. Held messages show to the person as
first-line previews only. The held notice is a `systemMessage` on
`UserPromptSubmit`, once per sender per day per device, on Claude Code and
Codex; Devin has no such channel.

**Redaction.** A message body passes the device redactor before it leaves
the machine, like transcript text.

## 5. Per-harness packaging

`flopwire setup` wraps each harness's own plugin commands.

| Harness | Install | Delivery | Send tools |
|---|---|---|---|
| Claude Code | One plugin: MCP server, hooks, skill | `SessionStart`, `UserPromptSubmit`, `PostToolUse` hooks | MCP |
| Codex | One plugin (`.codex-plugin/plugin.json`): MCP server, hooks, skill. Hooks need one approval; the hook command stays stable so later releases do not re-prompt. Codex caps a hook's timeout at 3 s | Same three hooks | MCP |
| Devin CLI | `devin plugins install --local` of the Claude Code plugin; no Devin config is written | Same three hooks. Not `PreToolUse`: it does not reach the model | MCP |
| opencode | One plugin | After a tool call in a running turn (`tool.execute.after`), `promptAsync` with `noReply: true` stores the message in the session; with the next user prompt (`chat.message`), it joins that prompt as a text part. The standing instruction goes through `experimental.chat.system.transform`. Never `promptAsync` without `noReply`, which wakes | Plugin tools |

Devin also runs hooks found in `.claude/settings.json`. `flopwire hook`
therefore detects the harness from its input and delivers each message
once, whichever config invoked it.

## 6. Vendor cloud (B6)

Push only, sent by the device agent while the cloud session reports
running. Both routes arrive with the account owner's authority and no peer
marking, so the wrapper and the B4 rule travel inline in every message.
User-facing detail is in [`docs/harnesses/cloud.md`](../../docs/harnesses/cloud.md);
the tests are in [`cloud-2026-10-03.md`](../log/cloud-2026-10-03.md).

| Surface | Discover | Deliver | Limits |
|---|---|---|---|
| Claude cloud | `GET /v1/code/sessions?statuses=active` with the user's sign-in token (undocumented) | `claude -p <text> --cloud <id> --output-format json` | No outbound route from the VM at the default network level, so these sessions receive and cannot send. No transcript in the archive, so no search |
| Devin cloud | `devin acp --cloud` `session/list` | `session/prompt` over `devin acp --cloud` | A Devin cloud session reaches public HTTPS hosts but has no `flopwire` CLI or device credential, so it receives only. Messages are attributed to the account owner |

- Each vendor is behind `vendorcloud.Adapter`. Claude discovery reads the
  list the CLI's `--teleport` reads. `/v1/sessions` is not used: it takes
  no filter and lists every Remote Control session of the account (over
  3,000 for the test account). Only `environment_kind: "anthropic_cloud"`
  sessions are kept. Devin's `session/list` is the organization's; only
  the person's own sessions are kept.
- Cloud sessions are owned by a user, not a device. Devices report them
  in `PollRequest.Cloud`; the server keeps one `bus_presence` row per
  person and session with `cloud` set and no device. Peers marks them
  `cloud`. `@user` never routes to one.
- A message to a cloud session is offered to all of the owner's devices;
  one claims it only while the vendor reports a turn running and pushes
  it. Lease and confirm as for hooks; a failed push is retried at the next
  presence tick and after 3 failures is `undelivered` (`push_failed`). A
  push the vendor refuses as archived or exited ends the session
  (`session_ended`).
- The cloud instruction (`busrender.CloudInstruction`) and the wrappers
  travel in every push; requests carry no reply line, and the instruction
  says the session cannot reply.
- Cross-user: the accept rule applies unchanged (B7), so accepted
  teammates reach cloud sessions.
- Read receipts: Claude from the session's events (the first assistant
  event after the pushed text), Devin from the push (the first agent
  chunk after the echo, within 8 s).
