# Field observation: a Codex session using Flopwire (2026-10-04/05)

Review of `~/.codex/sessions/2026/10/04/rollout-2026-10-04T16-59-08-01a108b6-9f7d-7703-aec7-5bcc575f0920.jsonl`
(53 MB, Codex CLI 0.160.0, `gpt-6`, `~/Code/eichler`, 2026-10-04 21:00 →
2026-10-06 02:20 UTC) and its two counterparts: Codex session
`01a10785-0bf2-7db3-9461-1d6c3147e111` (same device) and Claude Code
session `37dec565-9b37-4f48-acdc-8f224065aacf`. Real usage, not staged;
the shared server was enrolled. The session's own work was a long
multi-worktree "close loose ends" run (retention paging, outbox,
staging migrations).

Corrected 2026-10-06 after checking each finding against the code, the
device `bus.db` and the counterpart transcripts. Finding 4 was wrong as
first written; findings 2 and 6 were overstated. The corrections are
inline, and the "Verified against the code" section at the end says
what each finding turned out to be and where the fix went.

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
   tell from `sessions` output that liveness is broken. *Verified:* by
   design. `peers`, `send` and `inbox` go through the device agent so the
   caller is identified and path rules apply before anything leaves the
   device; the server's `live` flag in `sessions` is 10-minute recency,
   or the device's last live report within an hour, and needs no agent. The gap is the error text, which does
   not say that `live=` in `sessions` is unverified while the agent is
   down, or that this session can neither send nor receive. Taken by the
   presence lane (below).
2. **Both Codex sessions guessed the wrong SKILL.md path.** Both first
   tried `~/.codex/plugins/cache/flopwire/local/skills/messaging/SKILL.md`
   (missing the doubled `flopwire/flopwire` segment), then `rg --files`
   for the real one. The main session re-read the skill three times
   across context compactions, and re-fetched the `flopwire_*` tool
   descriptions via `ALL_TOOLS` introspection about eight times because
   Codex defers MCP schemas out of context. Most of the 5.5 min from
   delivery to first reply went to this re-discovery, not to the answer.
   *Verified:* a model error, set up by Codex's layout. Codex installs
   plugins at `cache/<marketplace>/<plugin>/<version>`, and our
   marketplace and plugin are both named `flopwire`, so the path is
   `cache/flopwire/flopwire/local/…`. The session's own
   `<skills_instructions>` listing gave the correct relative path
   (`r3/flopwire/local/skills/messaging/SKILL.md`); the model collapsed
   the repeated segment. The `ALL_TOOLS` dumps were about 1.6 KB for
   `flopwire_send` and under 1 KB for the others, not 4 KB each. Every
   other marketplace on the machine names the publisher, not the
   product (`openai-bundled/browser`); the rename to `flopwire-plugins`
   is #162.
3. **`grep` with a repo filter timed out having checked 0 candidates.**
   `flopwire_grep(repo="geteichler/eichler", pattern="…|…",
   include_self=true)` returned `timed out after 10s: checked 0
   candidates`, even though `flopwire_sessions` with the same repo value
   had just matched. The retry with `repo="eichler" agent="claude"
   since="24h"` found the hits in 9.4 s, itself just under the budget.
   *Verified:* the repo filter is resolved the same way in both tools
   (`resolveFilterRepo`), so there is no mismatch. The cost is in the
   candidate query: the trigram prefilter is a bitmap scan over every
   message in every repo, the repo predicate is applied on conversations
   after the join, and `ORDER BY m.ts DESC` has no index by design, so
   the first `FETCH` must finish the whole scan, recheck and sort before
   row one returns. `checked 0` means that took longer than 10 s.
   `docs/perf/shared-stack-2026-10-03.md` had already measured a 35 s
   broad grep, without a repo filter, on this corpus. There is no
   `EXPLAIN` of this exact query, so the scan-before-predicate reading is
   the most likely cause, not a measured one. The retry probably won on
   a warm cache,
   not on the narrower filters, since `since` is applied to the same
   unindexed column. Fix: resolve the matching conversation ids first
   when a conversation-level filter is set, and add them as a predicate
   on messages so the planner can AND the two bitmaps (branch
   `fix/grep-conversation-prefilter`).
4. **An `inform` to an idle session was never read.** This finding first
   said the recipient had ended 4 min before the send and the message
   was silently lost. Both parts were wrong. `peers session=37dec565`
   reported the Claude session live at 20:33:23 because it was live: its
   last turn finished at 20:29:46, its `~/.claude/sessions/<pid>.json`
   file was current, and its process exited at 23:45:58 (transcript
   mtime; `bus.db` shows `ended_by registry` at that time). The send at
   20:33:55 got `queued … arrives: next_prompt` with a 24 h `expires_at`,
   which was accurate: the message waited for the human's next prompt,
   and no prompt came. At 21:29:45 the session dropped out of presence
   (its `updatedAt` plus the 1 h `LiveCap`); when the process exited the
   message became `undelivered` with reason `session_ended`, which is
   what `flopwire_inbox --sent` shows the sender. Exchange-capture
   finding 6 ("recency live window") predates ended-session tracking
   (2026-10-02, #67 and #82) and no longer applies. What remains true:
   nothing told the sender. The `session_ended` outcome changes the
   message's state and writes an audit row, and nothing reaches the
   sender's context; the receipt's `outcome` line leaves out the
   `expires_at` the JSON carries; and
   neither `peers` nor the receipt says how long the recipient has been
   idle. A new side finding: a Claude Code session left idle at its
   prompt with a live process drops out of `peers` after `LiveCap`
   (1 h) although it can still receive mail. All three are the scope of
   the presence lane: the Codex session's 2026-10-06 reliability batch,
   run `20261006095552-0b98288d`, which keeps confirmed-open idle
   sessions in `peers`, adds `idle_since` to `peers` and receipts, and
   sends the sender one failure notice at its next hook, for every
   intent.
5. **Message style was telegraphic past the point a cold reader can
   parse.** The 20:33 inform reads "Retention prepares privately
   on46b538+frozen4f4 now. No cronpause/currentmaintenance running …
   heavy local test lane presentlyfree." The skill says write for a
   reader who knows nothing of your session; this needed the surrounding
   transcript to decode.
   *Verified:* model behaviour. The skill already states the cold-reader
   rule; it now carries a good/bad example as well.

6. **Instruction overhead is real in long sessions, but about half the
   size first reported.** The `<flopwire-instructions>` block was
   injected 18 times across the 29 h session. The block is 1,267 bytes,
   about 320 tokens, not ~700. The 18 copies are the startup delivery,
   one after each of the 15 compactions, and about two after resumes;
   there is no timer. The compaction snapshots do not keep the block, so
   re-injection after `compact` is what keeps the trust rules in
   context. Over 29 h the copies total roughly 6k tokens, and only one is
   live after each compaction. The one lever is to stop renewing on
   `resume` for Codex, which replays the rollout file. Deferred: not
   worth the risk of losing the trust rules for two copies a day.

No message was ignored, no send was retried, and nothing was asked of a
peer that the sender couldn't do itself. The one functional loss was
finding 4, which was benign here only because the inform was advisory.

## Verified against the code (2026-10-06)

| Finding | What it is | Where it went |
|---|---|---|
| 1 `peers` vs `sessions` | By design; error text gap | presence lane (see finding 4) |
| 2 SKILL.md path | Model error, set up by the `X/X` layout | #162 rename, later batch |
| 3 grep timeout | Server cost; a broad grep measured 35 s | `fix/grep-conversation-prefilter` |
| 4 idle inform | Wrong as first written; sender gets no notice | presence lane: notice, `idle_since`, `LiveCap` |
| 5 telegraphic message | Model behaviour | example in both SKILL.md |
| 6 instruction overhead | By design; figure corrected | resume skip deferred |
