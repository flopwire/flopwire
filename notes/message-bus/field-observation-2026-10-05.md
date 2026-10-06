# Field observation: a Codex session using Flopwire (2026-10-04/05)

Review of `~/.codex/sessions/2026/10/04/rollout-2026-10-04T16-59-08-01a108b6-9f7d-7703-aec7-5bcc575f0920.jsonl`
(53 MB, Codex CLI 0.160.0, `gpt-6`, `~/Code/eichler`, 2026-10-04 21:00 →
2026-10-06 02:20 UTC) and its two counterparts: Codex session
`01a10785-0bf2-7db3-9461-1d6c3147e111` (same device) and Claude Code
session `37dec565-9b37-4f48-acdc-8f224065aacf`. Real usage, not staged;
the shared server was enrolled. The session's own work was a long
multi-worktree "close loose ends" run (retention paging, outbox,
staging migrations).

## What Flopwire was used for

1. **Presence check before Computer Use** (21:39). The user said to check
   Rippling in Chrome "unless some other agent is using cua right now".
   The session ran `flopwire_sessions` then `flopwire_peers` to see who
   else was live.
2. **Dev-stack ownership coordination** (23:38–23:52). Counterpart Codex
   `01a10785` needed the local dev-stack that this session's retention
   subagent owned. It found the owner through history (`sessions` on the
   branch glob, `grep` for the worktree name, `sessions --detail` on
   candidate subagents — about 10 CLI calls over 2.3 min), checked
   `peers`, and sent a `request`. This session answered with two
   `inform`s (still-owned, then released) and the counterpart finished
   with `done`. Full loop: 14 min.
3. **Staging-ownership deconfliction** (next day, 20:32–20:33). Before
   running its own maintenance, the session used `grep`/`read`/`peers` to
   find and verify that Claude session `37dec565` had applied the #2058
   additive migrations (cross-checked against `gh run view`), then sent
   it an `inform`.

## Effectiveness

- Both coordination goals were achieved. The dev-stack exchange is the
  textbook flow on both sides: history → `peers` → `send`, correct
  `reply_to` threading, correct intents, no `inbox`/`peers` polling after
  send, `done` not answered. The counterpart's request message was a
  model first-line preview: what it wants, why, what it will not do, and
  how it found the session.
- Discovery worked as designed both times: worktree name → owning
  subagent → parent session; GH run id → applying session.
- The session verified transcript evidence against `gh` before relying
  on it, and sent the release notice proactively instead of leaving the
  requester waiting.
- Delivery: the request (23:38:30) surfaced at the session's next tool
  call (23:40:36); both replies reached the busy counterpart within
  seconds.

## Findings

1. **`peers` fails when the device agent is down, while `sessions` still
   answers from the shared server.** At 21:39 `flopwire_peers` returned
   `agent_not_running` ("nothing answers on …/agent.sock") although
   `flopwire_sessions` one call earlier worked. The session recovered
   gracefully and told the user "its live-presence service is
   unavailable", but then overclaimed "No agent in this team is using
   Computer Use" — presence was never actually established; Chrome-idle
   was the real evidence. Split availability is confusing: an agent can't
   tell from `sessions` output that liveness is broken.
2. **Both Codex sessions guessed the wrong SKILL.md path.** Both first
   tried `~/.codex/plugins/cache/flopwire/local/skills/messaging/SKILL.md`
   (missing the doubled `flopwire/flopwire` segment), then `rg --files`
   for the real one. The main session re-read the skill three times
   across context compactions, and re-fetched the `flopwire_*` tool
   descriptions via `ALL_TOOLS` introspection about eight times (~4 KB
   each) because Codex defers MCP schemas out of context. Most of the
   5.5 min from delivery to first reply went to this re-discovery, not to
   the answer.
3. **`grep` with a repo filter timed out having checked 0 candidates.**
   `flopwire_grep(repo="geteichler/eichler", pattern="…|…",
   include_self=true)` returned `timed out after 10s: checked 0
   candidates`, even though `flopwire_sessions` with the same repo value
   had just matched. The retry with `repo="eichler" agent="claude"
   since="24h"` found the hits in ~9 s. Either the repo filter did not
   constrain the candidate scan, or candidate enumeration alone blew the
   10 s budget; both are worth a look. The timeout note's advice
   ("narrow with --agent … --since") did produce the fix.
4. **An `inform` was sent to a session that had ended 4 min earlier, and
   was never delivered.** `peers session=37dec565` reported it live at
   20:33:23 (its last transcript record is 20:29:46); the send at
   20:33:55 got `queued … arrives: next_prompt` and never entered the
   Claude transcript (the id and body are absent). Same mechanism as
   exchange-capture finding 6 — the recency live window — now seen in
   production. For `intent="inform"` the loss is silent; the sender's
   receipt gave no hint the session was already gone.
5. **Message style was telegraphic past the point a cold reader can
   parse.** The 20:33 inform reads "Retention prepares privately
   on46b538+frozen4f4 now. No cronpause/currentmaintenance running …
   heavy local test lane presentlyfree." The skill says write for a
   reader who knows nothing of your session; this needed the surrounding
   transcript to decode.
6. **Instruction overhead is real in long sessions.** The
   `<flopwire-instructions>` block was injected 18 times across the 29 h
   session (~700 tokens each), plus 15 compacted snapshots carrying
   copies of the thread. Harmless per-turn, but it adds up with context
   compaction.

No message was ignored, no send was retried, and nothing was asked of a
peer that the sender couldn't do itself. The one functional loss was
finding 4, which was benign here only because the inform was advisory.
