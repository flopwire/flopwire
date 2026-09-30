# Message bus delivery: what reaches a running agent

Verified 2026-09-28 on Claude Code 2.1.284 and codex-cli 0.158.0, by live
tests on throwaway sessions and by reading the Codex source at tag
`rust-v0.158.0`. This note supersedes spec section 9.1 and the delivery
bullets of 9.2 in `notes/local-search/README.md` where they differ.

The question: can Flopwire deliver a message into a Claude Code or Codex
session that the user launched normally, with no wrapper and no launch
flags? Config installed ahead of time (settings, hooks, MCP servers) is
allowed.

**Answer: yes for both local CLIs.** Claude Code needs nothing at all.
Codex needs nothing for turn-boundary delivery; mid-turn delivery depends on
how the user launched it. Both mechanisms are local to the machine, are
same-user only, and are internals that can change in any release.

Categories used below: **A** works with no setup, **B** needs config set
before the session starts, **C** needs launch flags or a wrapper, **D** not
possible.

## Claude Code

| Mechanism | Cat. | Notes |
|---|---|---|
| Inbox socket | A | Documented: code.claude.com/docs/en/cross-session-messaging, "The session's inbox socket". Idle: wakes the session. Busy: arrives between tool-call batches in the same turn. Does not touch a half-typed prompt. |
| MCP channels | C | Server must be named in `--channels` at launch; own servers also need `--dangerously-load-development-channels` or the approved plugin list. Research preview, claude.ai/Console auth only, org opt-in `channelsEnabled`. |
| `UserPromptSubmit` / `SessionStart` `additionalContext`, `Stop` block | B | Fire only on user turns or at stop. Cannot wake an idle session. |
| `SessionStart` + `Stop` hooks with `asyncRewake: true` | B | A hook script blocks on an inbox, prints the message to stderr, exits 2. Wakes an idle session in about 1 s and re-arms after each turn. The message arrives framed as "Stop hook blocking error", and Haiku refused to act on it. Fallback only. |

### Socket protocol

- Registry: `~/.claude/sessions/<pid>.json` (0644) with `messagingSocketPath`,
  `sessionId`, `cwd`, `status` (idle/busy), `procStart`, `peerProtocol`,
  `name`. Read the path from here; do not compute it. The default is
  `$XDG_RUNTIME_DIR` or the temp dir plus `/cc-socks/<pid>.sock`, with
  fallbacks (`/tmp/cc-socks-<uid>/`, `<pid>-<8hex>.sock`). Check that the pid
  is alive and matches `procStart`; stale entries exist.
- Socket 0600 in a 0700 directory: same user only.
- Newline-delimited JSON:
  `{"type":"user","message":{"role":"user","content":"..."}}`. Optional
  `priority` (now/next/later), `msg_id`, `from`, `session_id` (a mismatched
  `session_id` is dropped). No response is written. A connection with no
  complete line in 30 s is closed.
- Auth line is optional on macOS and Linux (required on Windows). Token, if
  wanted: `~/.claude/sessions/<pid>.<sha256(socketPath)>.key`, field
  `peerToken`, sent first as `{"type":"auth","token":"..."}`.
- On by default. Gated by remote flag `tengu_harbor_kite` (default true) and
  env `CLAUDE_CODE_HARBOR_KITE`. `--bare` sessions have no socket.
- The transcript records `origin:{kind:"peer",from:...,verifiedPeerPid:...}`,
  so `read_at` can be detected from the archive as the spec intends.
- A `file_attachments` field exists but is gated; treat the payload as text.

### Receive policy

- `crossSessionInbound` unset, recipient prompting for permissions:
  delivered.
- `hold`: held with a notice. These do not expire; they wait for `accept`.
- `refuse`: dropped.
- Recipient in bypass-permissions mode: held behind an approval dialog that
  closes after `dialogExpiry` (5 min default).
- Limits: 100 held, 50 queued, per-sender rate limits, duplicate drop.

### Security gap

Any same-user process can wrap the body in
`<cross-session-message from-mode="bypass">…</cross-session-message>` and
skip the bypass-mode dialog, and can set `from-name` to any label. Claude
Code does not authenticate peers on macOS/Linux. The spec's rule "nothing
enters an agent's context that its human did not allow" must be enforced by
the Flopwire device agent, not by the harness.

## Codex CLI

| Mechanism | Cat. | Notes |
|---|---|---|
| `codex queue --thread <UUID> --message …` (`thread/queue/add`) | A | Every Codex process, including an in-process TUI, polls `~/.codex/queue_1.sqlite` every 10 s (`ext/queue/src/service.rs`) and starts a turn when the thread is idle. Idle: arrived in about 6 s. Busy: waits for the turn to end, then runs as its own turn. |
| `turn/steer` (mid-turn), `thread/inject_items` | B/C | Only for threads hosted on the shared app-server daemon. `inject_items` adds model-visible items without starting a turn and without showing in the TUI. |
| `PostToolUse` `additionalContext` | B | Mid-turn injection between tool calls in any session, daemon or not. Read from source, not live-tested. |
| Other hooks | B | SessionStart, SessionEnd, UserPromptSubmit, PreToolUse, PermissionRequest, Pre/PostCompact, SubagentStart/Stop, Stop, Interrupt. `Stop` with `decision: block` continues the turn. None wakes an idle session. Hooks need a one-time trust approval (`trusted_hash`); managed hooks are trusted automatically. |
| `notify` | B | Outbound only: tells us a turn ended. |
| MCP server push | D | Codex logs server notifications; nothing reaches the model. |

### Daemon attachment

`daemon_auto_start` is on by default, so a plain `codex` joins the shared
daemon. Any of `-c`, `--enable`, `--disable`, `--search`, `--profile`,
`--oss`, `--strict-config`, `--dangerously-bypass-hook-trust` or
`--no-daemon` makes the TUI run its own in-process server
(`tui/src/daemon_startup.rs`). `--search` counts because it expands to
`-c web_search="live"`; `--yolo` does not. A user who wants steer to work
moves those overrides into `config.toml`.

### Protocol

- Daemon socket: `~/.codex/app-server-control/app-server-control.sock`
  (symlink into `/private/tmp/codex-daemon-<uid>/<hash>`). JSON-RPC over a
  WebSocket on that Unix socket. Same user only, no token.
  `~/.codex/ipc/ipc.sock` is the TUI's `/ide` context socket, not the
  app-server.
- Handshake: `initialize` with `capabilities.experimentalApi: true`
  (`thread/queue/add` is experimental), then `initialized`.
- Methods used: `thread/loaded/list`, `thread/turns/list`,
  `turn/steer {threadId, expectedTurnId, input}`,
  `thread/inject_items {threadId, items}`,
  `thread/queue/add {threadId, clientUserMessageId, input}`.
- `clientUserMessageId` is kept in the rollout's UserMessage item, which
  gives a reliable `read_at` match.
- Prefer the `codex queue` CLI over writing the SQLite table.

### Behaviour to design around

- **Queued messages to a closed thread run on resume.** Codex accepts the
  item, stores it, and runs it the moment the user resumes, before they type.
  That spends the human's tokens unprompted, against spec 9.3. Flopwire should
  queue only to live sessions and keep everything else in its own inbox.
- **Esc stalls the queue.** After the user interrupts a turn, queued items
  wait until the user's next turn finishes.
- **Address by UUID.** `--thread <name>` took 79 s and then failed ("Cannot
  verify a unique session label across server pages").
- `codex queue` refuses under `--no-daemon`, and refuses config overrides
  while a daemon is up.
- The daemon installs itself under `~/.codex/packages/app-server-daemon/`
  and outlives the TUI.

## Across machines

Both mechanisms are local, so a device agent on each machine delivers. The
server already knows which device owns a session id, because uploads carry
the device credential. The device agent reports liveness:

- Claude: registry `status`, with pid and `procStart` checked.
- Codex on the daemon: `thread/loaded/list`.
- Codex in-process TUI: no registry. Needs process inspection (which rollout
  file a live `codex` has open). Not built or tested.

The server routes to the owning device's agent over its existing
connection; the agent writes to the socket or runs `codex queue`. Offline
device or no live session: hold in the Flopwire inbox (spec 9.3).

## Not covered

- **Claude cloud sessions (claude.ai/code):** only reachable through
  Anthropic's relay via `SendMessage` from a same-account Claude session,
  one-way. No public API. Treat as not possible.
- **Codex cloud tasks:** not checked whether follow-ups into a running task
  are possible.
- **Cursor:** Cloud Agents API accepts follow-ups (from the earlier survey,
  not re-verified).
- **Claude desktop app, Codex desktop app:** not tested. Likely the same
  engines (Claude registry and socket; Codex queue poller in every process),
  but app-hosted sessions may differ. Test with throwaway sessions before
  relying on either.

## Spec 9 corrections

- Drop "prefer an MCP channel": channels need launch flags.
- Claude delivery needs no `crossSessionInbound` setting for prompting
  sessions. Explicit `hold` never expires.
- Codex daemon socket and transport are as above; `ipc/ipc.sock` is wrong.
- `thread/injectItems` is `thread/inject_items`.
- The Codex queue does not need the daemon; steer and inject do.
- Mid-turn injection into a non-daemon Codex session is possible through a
  `PostToolUse` hook.

## Probes

- `cc_send.py <pid> <text> [auth] [extra-json]`: post one message to a
  Claude session's socket.
- `codex_rpc.py <loaded|turns|steer|inject|queue> <thread-id> [text]`:
  JSON-RPC client for the Codex daemon (needs `websockets`).

Run them only against throwaway sessions.
