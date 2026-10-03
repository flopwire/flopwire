# Message bus: v1 design and build plan

Date: 2026-10-01. Supersedes section 9 of
[`../local-search/README.md`](../local-search/README.md) where they differ.
Evidence: [`README.md`](README.md) (socket and queue, 2026-09-28) and
[`probes-2026-10-01.md`](probes-2026-10-01.md) (hooks, Devin, opencode,
cloud). Built as of 2026-10-03 except opencode and vendor cloud; where the build diverged, see section 9 and the dated notes in
sections 3 to 8.

## 1. Decisions

Made by Gary on 2026-10-01 unless marked "carried".

| # | Decision |
|---|---|
| B1 | **Direct messages only.** No channels and no repo broadcast in v1. |
| B2 | **Address a session or a person.** A session is addressed by its session id prefix, the same key `sessions`, `grep` and `read` print. `@user` addresses a person. |
| B3 | **No wake.** A message never starts a turn. It arrives inside a running turn, or with the human's next prompt. |
| B4 | **Authority depends on the sender.** From the recipient's own user: a teammate request, acted on within the recipient session's permissions. From another user: information; the agent confirms with its human before consequential actions. A message never changes permissions or settings. |
| B5 | **v1 harnesses:** Claude Code, Codex, Devin CLI, opencode. (2026-10-03: the first three are built; opencode is not, #62.) |
| B6 | **Vendor cloud in v1:** Claude cloud sessions and Devin cloud, pushed only while the session is running. An occasional wake from a send that races the end of a turn is accepted. (2026-10-03: not built, #63.) |
| B7 | (carried) A message from another user is held until the recipient's human accepts that sender once. Acceptance is per sender and revocable. |
| B8 | (carried) Undelivered messages expire after 24 hours by default. Flopwire never resumes a session headless to deliver. |

## 2. Why hooks, not the socket or the queue

Section 9 planned to push through Claude Code's inbox socket and
`codex queue`. Both wake an idle session, which B3 rules out. Pushing only
while the session is busy does not fix that: a send that lands as the turn
ends starts a new turn, and on Codex a queued item always runs as its own
turn after the current one. A queue to a running `codex exec` loses the
message.

Hooks have neither problem. The session pulls its own messages at points
where a turn is already running or a human has just typed, so no delivery
can start a turn. The same three hook events work on Claude Code, Codex and
Devin, with the same output format, and all were confirmed live. The
tradeoff is setup: hooks must be installed ahead of time. The install is
needed anyway, because the agent needs the send tools.

The socket and the queue stay documented in `README.md` as the route for a
future opt-in wake.

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

**Output (issue #55, changed 2026-10-01).** All three print compact JSON
by default, on the CLI and over MCP: busproto's field names, full session
ids, a `kind` field (`peers`, `send_receipt`, `inbox`, `error`), and
named fields that say whether more follows (`total`, `more`, `next`,
`hint`). `--text` (MCP: `format: "text"`) prints the readable forms
below. A failure is `{"kind":"error","error":{"code","detail","fix",
"example",…}}`: on stderr with exit status 1 on the CLI, `isError` over
MCP. The send result is a receipt (`kind: send_receipt`, `state`,
`arrives`), never a reply; inbox entries carry `direction` (sent or
received) and `is_reply`, so "delivered" never reads as "answered".

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
  live session on the repo. With none live, it waits for their next session
  on the repo, then their next session anywhere, until it expires.
- `--intent`: `request` expects a reply, `inform` (default) does not, `done`
  closes a thread and must not be answered.
- `--ref ADDRESS` attaches an archive address. It renders on delivery as the
  address plus a short excerpt; the recipient widens it with `read`.
- The first line of the text is the preview a human sees. Body cap: 4,000
  bytes. Longer material goes by `--ref`.

The receipt states the outcome (`arrives`: `next_tool_call`,
`next_prompt`, `when_accepted`, `next_session`, `only_if_resumed`), so the
sender never polls. 2026-10-02 (#83, #95): a receipt for `intent=request`
also carries `next`, what to do until the reply comes; there is no
blocking wait. With `--text` it is one line:

```
sent m7f3a to 0b7e2c1a (alex claude api@main): busy, arrives at its next tool call
sent m7f3b to 4c19e0d2 (gary codex api@main): idle, arrives with its human's next prompt
held m7f3c for @sam: sam has not accepted messages from you
queued m7f3d for @alex: no live session on api; expires 2026-10-02T14:02Z
```

### inbox

Lists this session's threads, newest first: messages received, messages
sent and their state (`queued`, `held`, `delivered`, `read`, `expired`,
`refused`). It exists to check a sent message and to re-read a thread.
Delivery does not depend on the agent calling it.

2026-10-03: the states as built are `queued`, `held`, `claimed`,
`delivered`, `read`, `expired`, `refused` and `undelivered` (with
`reason` `unconfirmed` or `session_ended`). `read` means the text
entered the recipient's context, not that it acted on it (#65, #106).

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
- At most 8 messages per thread per hour between agent sessions.
- At most 30 sends per session per hour.
- At most 120 sends per device and 300 per person per hour. The session id is the device's own report, so these ceilings hold a device that invents session ids.
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
  sender_user, created_at)`.
- Routes: `POST /v1/bus/send`, `GET /v1/bus/peers`, `GET /v1/bus/inbox`,
  `GET /v1/bus/poll`, `POST /v1/bus/claim`, `POST /v1/bus/ack`, and
  accept/revoke routes that need a human login session. 2026-10-01 (#41):
  poll is `POST`, because it carries and writes presence. Also built:
  `bus_presence` and `bus_messages_seq`; `GET /v1/bus/held` (#97);
  `GET`/`POST /v1/bus/accepts` and `DELETE /v1/bus/accepts/{user}`.
  `refuse_reason` became `reason` (#99).
- The sender's session id must belong to the calling device. Every send and
  delivery is audited.

**Server-to-device leg.** New. Sync today is device-initiated HTTP only. The
device agent holds one long-poll (`/v1/bus/poll`) that returns messages for
sessions on that device and `@user` messages for its user. The same request
carries the presence heartbeat.

**Device agent.**
- Keeps a local inbox table, so a hook never waits on the network.
- Reports presence: session id, harness, repo and branch, busy or idle. It
  already tails every transcript; busy or idle comes from the last event,
  with the harness registries as a cross-check (Claude session files, Codex
  writer locks, Devin `isLocked`).
- Ends sessions (#67, #82) from what the harness shows, never from
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

**Hook.** `flopwire hook` replaces `flopwire agent flush` in the hook
config and does both jobs.
- `SessionStart`, `UserPromptSubmit`, `PostToolUse`: anything pending,
  after the standing instruction while the session is owed it.
- `SessionEnd`: nothing printed; the flush tells the agent the session
  ended. Claude Code, Codex and Devin all have the event. Adding it to the
  Codex plugin made Codex ask to trust the new hook once.
- The standing instruction is leased like a message (#101): owed until a
  hook confirms printing it, printed before any message, and while its
  lease is out no hook of the session gets messages. A killed or late
  `SessionStart` hook leaves it to the next hook. A `SessionStart` with
  source `resume`, `compact` or `clear` after the confirmation makes it
  owed again: a compaction summarizes it away, and Flopwire cannot see
  what a resumed context kept. The confirmation is kept per session in
  `bus.db`, so two hook configs and an agent restart print it once.
- It asks the device agent over the control socket for messages for the
  `session_id` on stdin, prints them as `additionalContext`, and then
  confirms them (#90). It exits 0 with no output if the agent is down or
  slow (budget 200 ms), as `agent flush` does today.
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

`read_at` (built, #65) when the message's wrapper appears in hook context
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
| opencode | transform hooks do not persist; no parser | none: `delivered_at` only |

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
  nothing (#107), and a wrapper in a subagent's transcript is that
  subagent's sighting, never its parent's.
- The spec's Codex-to-Codex caveat (§9 of the local-search README:
  inter-agent bodies are encrypted in rollouts) is about Codex's own
  agent messages. It does not apply here: Codex stores hook context as
  plain text, so Codex recipients get `read_at`.
- opencode has `delivered_at` only unless the `noReply` route below
  works; its part metadata (`metadata.flopwire`) would then carry the id.

**Permission.** Accept and revoke are human actions: the web console, or
the CLI on a terminal. There is no MCP tool for them and the CLI refuses
them when stdin is not a terminal, so an agent cannot accept on its
human's behalf. 2026-10-02 (#97 review): the terminal check alone did not
hold, because an agent of the same OS user can read the saved login
session or run the CLI under a pseudo-terminal. Accept therefore also
needs the person's password, checked and rate-limited by the server.
Revoke stays one step. With the saved session alone an agent can still
list held previews and revoke.

**Redaction.** A message body passes the device redactor before it leaves
the machine, like transcript text.

## 5. Per-harness packaging

| Harness | Install | Delivery | Send tools |
|---|---|---|---|
| Claude Code | One plugin: MCP server, hooks, skill | `SessionStart`, `UserPromptSubmit`, `PostToolUse` hooks | MCP |
| Codex | One plugin (`.codex-plugin/plugin.json`): MCP server, hooks, skill. Hooks need one approval; the hook command stays stable so later releases do not re-prompt | Same three hooks | MCP |
| Devin CLI | Devin-format hooks and MCP config at user level (2026-10-02, #85: built as `devin plugins install --local` of the Claude Code plugin; no Devin config is written) | Same three hooks. Not `PreToolUse`: it does not reach the model | MCP |
| opencode | One plugin | `experimental.chat.messages.transform` adds pending messages to the next model call; the standing instruction goes through `system.transform`. No `promptAsync`, which wakes | Plugin tools |

Devin also runs hooks found in `.claude/settings.json`. `flopwire hook`
therefore detects the harness from its input and delivers each message
once, whichever config invoked it.

On opencode, `promptAsync` with `noReply: true` stores a visible message
with metadata and started no extra turn in a busy session. Whether it also
leaves an idle session idle is untested. If it does, it replaces the
transform hook and gives opencode a read receipt.

opencode needs a transcript parser (SQLite: `session`, `message.data`,
`part.data`) before its sessions appear in `peers`, search or receipts.

## 6. Vendor cloud (B6)

Push only, sent by the device agent while the cloud session reports
running. Both routes arrive with the account owner's authority and no peer
marking, so the wrapper and the B4 rule travel inline in every message.

| Surface | Discover | Deliver | Limits |
|---|---|---|---|
| Claude cloud | `GET /v1/sessions` with the user's sign-in token (undocumented; isolate behind an adapter) | `claude -p <text> --cloud <id> --output-format json` | No outbound route from the VM at the default network level, so these sessions receive and cannot send. No transcript in the archive, so no search and no `read_at` |
| Devin cloud | `devin acp --cloud` `session/list` | `session/prompt` over `devin acp --cloud` | Sending from inside is untested. Messages are attributed to the account owner |

Cloud sessions are owned by a user, not a device. Any of that user's
devices may deliver; the claim call picks one.

## 7. Build sequence

Small PRs, each with tests. Items 1 to 5 are the first usable slice.
Status as of 2026-10-03.

1. Server: schema, send, poll, claim, ack, peers, inbox, limits, audit.
   Done: #41; send ceilings #51.
2. Device agent: long-poll, local inbox, presence, control-socket
   `pending`, same-machine routing with no server. Done: #49.
3. CLI and MCP: `peers`, `send`, `inbox`; sender identity for Claude and
   Codex. Done: #74 (Devin identity too); output reworked in #84.
4. `flopwire hook` and the Claude Code plugin. Acceptance: the marker test
   from the probes, run with `claude -p`, including a plugin-loaded hook.
   Done: #75, #76.
5. Codex plugin. Acceptance: the same test with `codex exec` and an
   in-process TUI. Done: #78 (`codex exec` only; the TUI and its "Hooks
   need review" screen were not exercised live).
6. Devin CLI: installer, sender identity, acceptance with `devin -p`.
   Done: #85.
7. Accept and revoke: console and CLI; held-message notice. Done: #97.
8. opencode: parser, then plugin. Open: #62.
9. Cloud: Claude cloud push and discovery; Devin cloud push. Open: #63.
10. Docs. Check the README and landing claims in PR #23 against what
    shipped. #72.

Added during the build: the `next` hint (#95), two-step delivery (#99),
ended sessions and the leased standing instruction (#105), `--repo`
across worktrees (#100), commits without a sha (#103), read's
`messages_before`/`messages_after` (#98), read receipts (#106).

## 8. Open items

- **Idle recipients are invisible to their human.** With no wake, a request
  to an idle session waits until someone types, and nothing tells that
  person. Options: an OS notification from the device agent, a status-line
  count, the web console. Not decided. (2026-10-01, #66: no notice in v1;
  the sender is told the recipient is idle.)
- **Hook context size caps** per harness are unmeasured. The 4,000-byte
  body cap is a guess that needs checking against each. (2026-10-03, #68:
  Claude Code 10,000 characters, Codex 10,000 bytes, Devin over 1 MiB;
  the caps stay. See [`hook-caps-2026-10-03.md`](hook-caps-2026-10-03.md).)
- **Hook cost.** `PostToolUse` runs on every tool call. The 200 ms budget
  and the local inbox keep it cheap; measure it. (2026-10-02, #99: about
  20 ms per hook on macOS; the first exec of a new binary can stall for
  minutes on `syspolicyd`. #68 stays open for the other harnesses.)
  (2026-10-03, #68: the pending request takes 0.5 ms median, 115 ms at
  most; the hook's wall time is process start, which a loaded machine
  stretches past 200 ms. The budget stays.)
- **Plugin-loaded hooks on Claude Code** and interactive sessions were not
  probed. First acceptance test of PR 4. (2026-10-01, #76: plugin-loaded
  hooks deliver in `claude -p`; interactive sessions still untested.)
- **Cross-user messages into cloud sessions** arrive with user authority.
  Consider own-user only for cloud in v1. (2026-10-01, #63: accepted
  teammates may message cloud sessions, as local ones.)
- **Undocumented surfaces:** the Claude cloud session list, and the Devin
  CLI token on REST. Both can change without notice.
- **Harness drift.** Claude Code and Codex ship several releases a week.
  Re-run the marker tests in CI against current versions.
- **Desktop apps and IDE extensions** probably load the same hooks. Not
  tested.

## 9. As built (2026-10-03)

Where the build diverged from sections 1 to 8, and the decisions taken
after the plan merged. Tracker #73; the merged code wins over this note.

| Area | Plan | As built |
|---|---|---|
| Poll | `GET /v1/bus/poll` | `POST`: the request carries the device's presence and replaces it on the server (#41). Live means reported within 75 s. |
| Send ceilings | 30 per session per hour | Also 120 per device and 300 per person per hour, because a device reports its own session ids (#51). Without a server the device does not apply these two (#71). |
| Output | `--text` forms by default | JSON by default for `peers`, `send`, `inbox` and `sessions`; text with `key=value` headers for `grep`, `search` and `read` (#55, #74, #84). |
| MCP results | not specified | One text block per answer; no `structuredContent` or `outputSchema` on any tool, because Claude Code shows the model only the structured part (#84). |
| Device inbox | "a local inbox table" | `bus.db`, its own SQLite file in the config dir, so a hook never waits on the index writer (#49). Recreated when its schema version changes. |
| Hook delivery | the hook prints and the receipt follows | Two steps: `pending` leases the messages to the hook for 10 s, the hook prints, then confirms. An unconfirmed lease comes again marked `redelivery="true"`; after 3 it is `undelivered` (`unconfirmed`). A hook older than 3 s takes and confirms nothing (#90, #99). |
| Standing instruction | at session start | Leased like a message, printed before any message, renewed after a `resume`, `compact` or `clear` start (#101, #105). |
| Hook events | three | `SessionStart`, `UserPromptSubmit`, `PostToolUse` print; `Stop` and `SessionEnd` only flush. `SessionEnd` ends the session (#105). Adding it made Codex ask to trust the new hook once; Codex caps its timeout at 3 s. |
| Ended sessions | not specified | Ended on harness evidence only, never on idleness. Their waiting messages become `undelivered` (`session_ended`) and are never given to another session (#67, #82, #105). |
| Receipt | `arrives` | Also `next` on requests: what to do until the reply comes (#83, #95). |
| Install | one plugin per harness | `flopwire setup` wraps each harness's own plugin commands (#58, #76, #78). Devin installs the Claude Code plugin with `devin plugins install --local` (#85). Codex needs a one-time hook approval. |
| Devin busy | from the transcript | From hook events: busy after `UserPromptSubmit` or `PostToolUse`, idle after `Stop`, idle after 15 min with no event (#85). |
| Accept | console and terminal | Also needs the person's password (#97 review). Held messages show to the person as first-line previews only. The notice is a `systemMessage` on `UserPromptSubmit`, once per sender per day per device, on Claude Code and Codex; Devin has no such channel. Revoke also re-holds claimed messages. Members get the console's Messaging page. |
| Repo | path prefix or basename | `--repo` names a repository: its main checkout and normalized remote, across worktrees and clones on the device (#100). The server stores no remote, so `--server --repo` does not match another machine's checkout at another path (#102). `@user` routing is still by repo name. |
| Commit evidence | `[branch sha]` lines | Also `commits_no_sha` for quiet commits, resolved by a later `rev-parse`, `log`, `show` or `push` (#103). |
| Read receipts | `read_at` at ingest on the server | Set by the device agent when the wrapper appears in a hook-context row of the recipient's transcript (Claude Code, Codex, Devin CLI; not opencode); sent in the ack batch; the server sets `read_at` once (#65, #106). It means the text entered the context, not that the model acted. |

Decisions recorded on #73 and its issues:

- **Busy recipients:** a message is injected into the running turn at the
  next tool call, for every intent.
- **Idle recipients:** no notice to the human in v1 (#66).
- **Session ended before reading:** no reassignment; the sender sees
  `undelivered` with `session_ended` (#67).
- **Install:** `flopwire setup` wraps each harness's plugin install (#58).
- **Cloud senders:** accepted teammates may message cloud sessions, once
  cloud delivery exists (#63).
- **Authority (#77):** risk accepted. Accepting a sender is the trust
  decision. An accepted sender's agents can direct the recipient's agents
  within each session's permissions, including sessions that skip
  permission prompts. The information-only rule for teammate messages is
  guidance to the model, not a boundary; smaller models do not follow it.
- **Waiting for a reply (#83):** no blocking `inbox --wait`; the receipt's
  `next` says what to do.
- **Lost hooks (#101):** the standing instruction is leased like a
  message.

Still open: opencode (#62), vendor cloud (#63), marker tests in CI (#69), server
hardening (#70), device-agent hardening (#71), the server repo key
(#102), the plugin follow-ups (#58, #59, #60), and a captured exchange
for the homepage (#55, #54).
