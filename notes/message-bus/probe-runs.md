# `flopwire probe` runs

One section per hand run of `flopwire probe --notes`, oldest first. See
[`docs/probe.md`](../../docs/probe.md) for what each case proves.

A manual check of a GUI surface or an interactive TUI gets its own
section, headed `## YYYY-MM-DD manual: SURFACE`, in date order with the
probe runs. See [`docs/manual-checks.md`](../../docs/manual-checks.md).

## 2026-10-03 16:05Z

flopwire dev, local agent. claude: 2.1.288 (Claude Code), model haiku. codex: codex-cli 0.160.0, model gpt-5.6-luna. devin: devin 3000.11.1 (cc4e349ca55e), model swe-2-medium.

| Harness | Case | Result | Evidence |
|---|---|---|---|
| claude | idle | PASS | no turn in 1m0s; m76fcd79a8352efd7 still queued |
| claude | prompt-submit | PASS | UserPromptSubmit printed m00dbf5ad2c36636a; model quoted PROBE-PROMPTSUBMIT-be18c7 |
| claude | framing | PASS | model quoted id, from=3fa6ad16…, intent=request and the marker |
| claude | mid-turn | PASS | PostToolUse Bash printed m154170576820d6ac, 6.718s before the turn's Stop; model quoted PROBE-MIDTURN-59e5e6 |
| claude | subagent | PASS | 4 hooks ran inside the subagent; the session's PostToolUse Agent printed mecec620906f1a468 after SubagentStop agent_id=a3b5b2d3dcd9…; subagent transcript clean; model quoted PROBE-SUBAGENT-b4cdde |
| codex | idle | PASS | no turn in 1m0s; me56997d92bfd13f2 still queued |
| codex | prompt-submit | PASS | UserPromptSubmit printed m1258ad2e949e77d5; model quoted PROBE-PROMPTSUBMIT-f10b04 |
| codex | framing | PASS | model quoted id, from=01a10283…, intent=request and the marker |
| codex | mid-turn | PASS | PostToolUse Bash printed m7fddc9300ee004e6, 9.417s before the turn's Stop; model quoted PROBE-MIDTURN-92cb81 |
| codex | subagent | PASS | 5 hooks ran inside the subagent; the session's PostToolUse multi_agent_v1wait_agent printed m4d478f89ccff365d after SubagentStop agent_id=01a10285-870…; subagent transcript clean; model quoted PROBE-SUBAGENT-11c50d |
| codex | guardian | PASS | review 4.135s; hooks during it: none; the session's PostToolUse Bash printed mb30b96ef7cc34f87 after the review; model quoted PROBE-GUARDIAN-82da44 |
| devin | idle | PASS | no turn in 1m0s; m0cc40e0ae979b85b still queued |
| devin | prompt-submit | PASS | UserPromptSubmit printed m500177ba41e52030; model quoted PROBE-PROMPTSUBMIT-d6d64c |
| devin | framing | PASS | model quoted id, from=lunar-de…, intent=request and the marker |
| devin | mid-turn | PASS | PostToolUse exec printed m005cd055cb84a14a, 2.185s before the turn's Stop; model quoted PROBE-MIDTURN-1256b4 |
| devin | subagent | PASS | 3 hooks ran inside the subagent; the session's PostToolUse run_subagent printed m55c96b5d60d3d831 after PostToolUse run_subagent; subagent transcript clean; model quoted PROBE-SUBAGENT-87fc65 |

16 passed, 0 failed.

### Codex auto-review ("guardian"), #107

Setting: `approvals_reviewer = "auto_review"` in `config.toml` (the legacy
value `guardian_subagent` is accepted), with `approval_policy =
"on-request"`. `codex exec --approve-for-me` sets it for one run; the
app-server takes `approvalsReviewer` on `thread/start`. An escalated
command then goes to the reviewer subagent, and the app-server reports the
pass as `item/autoApprovalReview/started` and `.../completed`.

Hooks do not run in the reviewer. In the `guardian` row above the message
was sent when the review started; no hook fired during the 4.1 s review,
and the session's own `PostToolUse` of the reviewed command printed the
message after it. A one-off run with the same hooks asked for
`curl --data-binary @notes.txt https://httpbin.org/post`. The 11.4 s
review approved "the verified non-sensitive contents of notes.txt", and
still no hook event (not even `PreToolUse`) fired between the review's
start and end. The reviewer cannot take the
session's messages, so the Codex subagent detection (#109) needs no change
for it.

## 2026-10-03 19:35Z

flopwire dev, local agent. opencode: 1.18.30, model opencode/big-pickle.

| Harness | Case | Result | Evidence |
|---|---|---|---|
| opencode | idle | PASS | no turn in 1m0s; m6071bbf66dcf0d86 still queued |
| opencode | prompt-submit | PASS | UserPromptSubmit printed mb39e838c1770c0d3; model quoted PROBE-PROMPTSUBMIT-b03bd9 |
| opencode | framing | PASS | model quoted id, from=ses_efcb…, intent=request and the marker |
| opencode | mid-turn | PASS | PostToolUse bash printed mf8090986a4bfe29c, 1m34.147s before the turn's Stop; model quoted PROBE-MIDTURN-6169fe |
| opencode | subagent | PASS | 4 hooks ran inside the subagent; the session's PostToolUse task printed mcbd57c7bfc190d49 after PostToolUse task; subagent transcript clean; model quoted PROBE-SUBAGENT-2e6880 |

5 passed, 0 failed.

## 2026-10-04 20:12Z

flopwire dev, local agent. claude: 2.1.289 (Claude Code), model haiku. codex: codex-cli 0.160.0, model gpt-5.6-luna. devin: devin 3000.11.1 (cc4e349ca55e), model swe-2-medium. opencode: 1.18.30, model opencode/big-pickle.

| Harness | Case | Result | Evidence |
|---|---|---|---|
| claude | framing | PASS | transcript hook context holds the wrapper: id, from=47ba615d…, agent=claude, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| claude | prompt-submit | PASS | UserPromptSubmit printed m1759ad45df0296c0; transcript hook context holds m1759ad45df0296c0 with PROBE-PROMPTSUBMIT-40f16f; model quoted PROBE-PROMPTSUBMIT-40f16f |
| claude | mid-turn | PASS | PostToolUse Bash printed ma1fd4619c4053096, 4.333s before the turn's Stop; transcript hook context holds ma1fd4619c4053096 with PROBE-MIDTURN-01f3b5; model quoted PROBE-MIDTURN-01f3b5 |
| codex | framing | PASS | transcript hook context holds the wrapper: id, from=01a1088b…, agent=codex, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| codex | prompt-submit | PASS | UserPromptSubmit printed ma36ac645238153fe; transcript hook context holds ma36ac645238153fe with PROBE-PROMPTSUBMIT-39df3e; model quoted PROBE-PROMPTSUBMIT-39df3e |
| codex | mid-turn | PASS | PostToolUse Bash printed m60cacc61faadefd3, 7.376s before the turn's Stop; transcript hook context holds m60cacc61faadefd3 with PROBE-MIDTURN-3c035a; model quoted PROBE-MIDTURN-3c035a |
| devin | framing | PASS | transcript hook context holds the wrapper: id, from=perfect-…, agent=devin, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| devin | prompt-submit | PASS | UserPromptSubmit printed m5e1ba103f15db481; transcript hook context holds m5e1ba103f15db481 with PROBE-PROMPTSUBMIT-dccd52; model quoted PROBE-PROMPTSUBMIT-dccd52 |
| devin | mid-turn | PASS | PostToolUse exec printed m7005467a31605d62, 4.623s before the turn's Stop; transcript hook context holds m7005467a31605d62 with PROBE-MIDTURN-7594d4; model quoted PROBE-MIDTURN-7594d4 |
| opencode | framing | PASS | transcript hook context holds the wrapper: id, from=ses_ef77…, agent=opencode, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| opencode | prompt-submit | PASS | UserPromptSubmit printed mac1dc75717a16ce6; transcript hook context holds mac1dc75717a16ce6 with PROBE-PROMPTSUBMIT-38c011; model quoted PROBE-PROMPTSUBMIT-38c011 |
| opencode | mid-turn | PASS | PostToolUse bash printed md6f181af568c138a, 5.196s before the turn's Stop; transcript hook context holds md6f181af568c138a with PROBE-MIDTURN-135b1f; model quoted PROBE-MIDTURN-135b1f |

12 passed, 0 failed.

First run with the transcript verdicts (#131): each case reads the
message's wrapper from the recipient's transcript as the harness stored
it, and the model's quote is corroboration only. Cases framing,
prompt-submit and mid-turn, cheapest models.

## 2026-10-04 20:32Z

flopwire dev, local agent. claude: 2.1.289 (Claude Code), model haiku. codex: codex-cli 0.160.0, model gpt-5.6-luna. devin: devin 3000.11.1 (cc4e349ca55e), model swe-2-medium. opencode: 1.18.30, model opencode/big-pickle.

| Harness | Case | Result | Evidence |
|---|---|---|---|
| claude | subagent | PASS | 8 hooks ran inside the subagent; the session's PostToolUse Agent printed m2048d6212229a55a after SubagentStop agent_id=a06ce78e9fb0…; subagent transcript clean; transcript hook context holds m2048d6212229a55a with PROBE-SUBAGENT-88e2c0; model quoted PROBE-SUBAGENT-88e2c0 |
| codex | subagent | PASS | 5 hooks ran inside the subagent; the session's PostToolUse multi_agent_v1wait_agent printed m47a7eb8cd5a47d9a after SubagentStop agent_id=01a1089e-cfd…; subagent transcript clean; transcript hook context holds m47a7eb8cd5a47d9a with PROBE-SUBAGENT-be3068; model quoted PROBE-SUBAGENT-be3068 |
| codex | guardian | PASS | review 3.347s; hooks during it: none; the session's PostToolUse Bash printed me6f6aea9a6a758ae after the review; transcript hook context holds me6f6aea9a6a758ae with PROBE-GUARDIAN-807a6b; model quoted PROBE-GUARDIAN-807a6b |
| devin | subagent | PASS | 3 hooks ran inside the subagent; the session's PostToolUse run_subagent printed m5284fe353b5a8caa after PostToolUse run_subagent; subagent transcript clean; transcript hook context holds m5284fe353b5a8caa with PROBE-SUBAGENT-0155eb; model quoted PROBE-SUBAGENT-0155eb |
| opencode | subagent | PASS | 8 hooks ran inside the subagent; the session's PostToolUse task printed me162938a3f11d930 after PostToolUse task; subagent transcript clean; transcript hook context holds me162938a3f11d930 with PROBE-SUBAGENT-ee8dc4; model quoted PROBE-SUBAGENT-ee8dc4 |

5 passed, 0 failed.

Reviewer run for PR #140, after the wrapper-completeness fix: subagent and
guardian (Codex only), cheapest models. Each wrapper was closed, uncut,
followed by a model reply, and held once by the session's transcript. The
same build also passed framing on all four harnesses and mid-turn on
Devin and opencode (not recorded above).

## 2026-10-05 15:40Z

flopwire dev, local agent. Plugins as installed (`--as-installed`): the hooks ran the plugins' own commands through the shim. claude: 2.1.289 (Claude Code), model haiku. codex: codex-cli 0.160.0, model gpt-5.6-luna. devin: devin 3000.11.1 (cc4e349ca55e), model swe-2-medium. opencode: 1.18.30, model opencode/big-pickle.

| Harness | Case | Result | Evidence |
|---|---|---|---|
| claude | idle | PASS | no turn in 1m0s; m68c21adb98e055eb still queued |
| claude | prompt-submit | PASS | UserPromptSubmit printed m1957152c90f0c638; transcript hook context holds m1957152c90f0c638 with PROBE-PROMPTSUBMIT-4b5dae; model quoted PROBE-PROMPTSUBMIT-4b5dae |
| claude | framing | PASS | UserPromptSubmit printed mbbfe0b0d40b27554; transcript hook context holds the whole wrapper: id, from=78b56ff7…, agent=claude, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| claude | mid-turn | PASS | PostToolUse Bash printed m6f8374e0934a3493, 5.886s before the turn's Stop; transcript hook context holds m6f8374e0934a3493 with PROBE-MIDTURN-361684; model quoted PROBE-MIDTURN-361684 |
| claude | subagent | PASS | 8 hooks ran inside the subagent; the session's PostToolUse Agent printed m60ad6d089dc071b4 after SubagentStop agent_id=a27fc764bb08…; subagent transcript clean; transcript hook context holds m60ad6d089dc071b4 with PROBE-SUBAGENT-1773a2; model quoted PROBE-SUBAGENT-1773a2 |
| claude | hook-binary | PASS | the plugin's hook command ran this probe's flopwire: 21 hooks via recorded path |
| codex | idle | PASS | no turn in 1m0s; m1bf49e0a49949e4a still queued |
| codex | prompt-submit | PASS | UserPromptSubmit printed m30373441818dd6c2; transcript hook context holds m30373441818dd6c2 with PROBE-PROMPTSUBMIT-46596d; model quoted PROBE-PROMPTSUBMIT-46596d |
| codex | framing | PASS | UserPromptSubmit printed m4fe5ae1892d60905; transcript hook context holds the whole wrapper: id, from=01a10cb9…, agent=codex, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| codex | mid-turn | PASS | PostToolUse Bash printed md3531113bdc88532, 9.575s before the turn's Stop; transcript hook context holds md3531113bdc88532 with PROBE-MIDTURN-87759b; model quoted PROBE-MIDTURN-87759b |
| codex | subagent | PASS | 9 hooks ran inside the subagent; the session's PostToolUse multi_agent_v1wait_agent printed m65d9876398eeffdb after SubagentStop agent_id=01a10cba-e61…; subagent transcript clean; transcript hook context holds m65d9876398eeffdb with PROBE-SUBAGENT-ebbb7a; model quoted PROBE-SUBAGENT-ebbb7a |
| codex | guardian | PASS | review 10.027s; hooks during it: none; the session's PostToolUse Bash printed m2b7c152d66b14479 after the review; transcript hook context holds m2b7c152d66b14479 with PROBE-GUARDIAN-d11696; model quoted PROBE-GUARDIAN-d11696 |
| codex | hook-binary | PASS | the plugin's hook command ran this probe's flopwire: 30 hooks via recorded path |
| devin | idle | PASS | no turn in 1m0s; ma098d21e2ac0ae10 still queued |
| devin | prompt-submit | PASS | UserPromptSubmit printed m0d3b6203dca8dcd1; transcript hook context holds m0d3b6203dca8dcd1 with PROBE-PROMPTSUBMIT-87b207; model quoted PROBE-PROMPTSUBMIT-87b207 |
| devin | framing | PASS | UserPromptSubmit printed m07f58093fe4dc4f1; transcript hook context holds the whole wrapper: id, from=politica…, agent=devin, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| devin | mid-turn | PASS | PostToolUse exec printed m3aaafcab6f5c293c, 7.101s before the turn's Stop; transcript hook context holds m3aaafcab6f5c293c with PROBE-MIDTURN-9f36ca; model quoted PROBE-MIDTURN-9f36ca |
| devin | subagent | PASS | 3 hooks ran inside the subagent; the session's PostToolUse run_subagent printed meee5ff7340fc0c6c after PostToolUse run_subagent; subagent transcript clean; transcript hook context holds meee5ff7340fc0c6c with PROBE-SUBAGENT-d2e0b6; model quoted PROBE-SUBAGENT-d2e0b6 |
| devin | hook-binary | PASS | the plugin's hook command ran this probe's flopwire: 20 hooks via recorded path |
| opencode | idle | PASS | no turn in 1m0s; mc0738069ade0282d still queued |
| opencode | prompt-submit | PASS | UserPromptSubmit printed mc733d1cecc54ec85; transcript hook context holds mc733d1cecc54ec85 with PROBE-PROMPTSUBMIT-761d6b; model quoted PROBE-PROMPTSUBMIT-761d6b |
| opencode | framing | PASS | UserPromptSubmit printed m3d96a6994dd2d2de; transcript hook context holds the whole wrapper: id, from=ses_ef34…, agent=opencode, sender=own, intent=request and the marker; model quoted id, from, intent and the marker |
| opencode | mid-turn | PASS | PostToolUse bash printed m1615f2555bb82a68, 9.816s before the turn's Stop; transcript hook context holds m1615f2555bb82a68 with PROBE-MIDTURN-ac5afd; model did not quote PROBE-MIDTURN-ac5afd (reply: "No new `<flopwire-message>` tags arrived during or after either command. ID mc07…") |
| opencode | subagent | PASS | 4 hooks ran inside the subagent; the session's PostToolUse task printed m15686aea040069c3 after PostToolUse task; subagent transcript clean; transcript hook context holds m15686aea040069c3 with PROBE-SUBAGENT-3ef673; model quoted PROBE-SUBAGENT-3ef673 |
| opencode | hook-binary | PASS | the plugin's hook command ran this probe's flopwire: 20 hooks via recorded path |

25 passed, 0 failed.

## 2026-10-10 manual: interactive TUIs

Flopwire runtime `7cc9fd9e9148`, local production agent and shared server.
Repository main `f432f28` adds a test after that runtime; no new binary was deployed.
Fresh sessions ran in an empty scratch Git repository on the Mac.
Existing user plugins and hook trust were retained.
These checks used interactive PTYs, not headless probe drivers.

| Surface | Case | Result | Evidence |
|---|---|---|---|
| Claude Code 2.1.296 | tool roster / presence | PASS | New session `3e434760-3408-4aa1-86e2-51aaff5ad820`; peers/send/inbox in its deferred MCP roster; live idle on the scratch branch |
| Claude Code 2.1.296 | prompt-submit | PASS | `mf93cad8e05bf86c7` entered hook context; model quoted `MANUAL-CLAUDE-IDLE-20261010-A`; sender state read |
| Claude Code 2.1.296 | one-minute idle interval | PASS | Recheck `m637db1d4e651f586` stayed queued for over a minute before the next human prompt; initial A interval was about 53 seconds |
| Claude Code 2.1.296 | delivery at a tool boundary | PASS | `m689387e7d5fdea59` sent while busy; PostToolUse:Bash attached it after `echo first`; model quoted it; sender state read |
| Claude Code 2.1.296 | threaded reply | PASS | MCP reply `m3cfd5073859cd10c` contains `MANUAL-CLAUDE-REPLY-20261010-A`, reply_to `m7bfb114b256607b3` |
| Codex 0.162.0 | tool discovery / presence | PASS | New session `01a125db-7239-7e90-9550-00ff49d972a4`; discovery exposed peers/send/inbox; live idle on scratch repo |
| Codex 0.162.0 | idle / prompt-submit | PASS | `md3037c69cbdebcd5` remained queued for a minute; additional hook context contains it; model quoted `MANUAL-CODEX-IDLE-20261010-A`; sender state read |
| Codex 0.162.0 | mid-turn | PASS | `m11273a98640f420b` sent while busy during `sleep 30`; hook context arrived during completion wait before `echo second`; model quoted it; sender state read |
| Codex 0.162.0 | threaded MCP reply | BLOCKED | Send required approval; this test session's approval policy was never. No reply was sent for `m5960167bec8cb749` |
| Devin 3000.11.1 | tool roster / presence | PASS | New session `spiny-seagull`; MCP discovery confirmed peers/send/inbox; live idle on scratch repo |
| Devin 3000.11.1 | idle / prompt-submit | PASS | `me6564080d001bdf8` remained queued for a minute; transcript system step contains it; model quoted `MANUAL-DEVIN-IDLE-20261010-A`; sender state read |
| Devin 3000.11.1 | mid-turn | PASS | `m1a17e536b700c375` sent while busy during approved `sleep 30`; transcript system context appeared before `echo second`; model quoted it; sender state read |
| Devin 3000.11.1 | threaded reply | PASS | One-time send approval; reply `m715470b18bd68582` contains `MANUAL-DEVIN-REPLY-20261010-A`, reply_to `mda4b9f7e43a691bd` |

The first Codex prompt prohibited discovery, so its report of no visible
Flopwire tools was inconclusive. A discovery-only prompt exposed them.
No installation change was needed.

Claude's standalone `sleep 30` was blocked by its existing command policy.
The model used the permitted background form and resumed on completion.
The recorded Claude mid-turn case instead used separate `echo first` and
`echo second` calls. This proves delivery at an actual tool boundary,
but does not qualify the checklist's foreground sleep sequence.

All idle and mid-turn messages had intent inform and sender own.
Replies were separate request threads. Read receipts mean context delivery;
they do not mean a request was answered. Reply ids above prove replies were
queued for the exact requesting session. Fresh no-tool prompts subsequently
received those replies through hook context; their sender states are read.

Private evidence includes synthetic transcripts and inbox JSON in
`/tmp/flopwire-manual-evidence-20261010.a5myl31t` on the test Mac.
No private work transcript was added to this log.

Fresh hook-review UI, global install/remove and a genuinely different-user
Devin sender remain UNVERIFIED. Existing five Codex hooks were trusted. The TUI `/mcp` inventory
reported Flopwire connected with seven tools.
No global plugin removal, trust reset, app restart or app upgrade was done.
Desktop and IDE results are recorded separately below.

## 2026-10-10 manual: desktop availability

| Surface | Result | Evidence / remaining work |
|---|---|---|
| Claude Desktop 2.31226.0, Local Code | BLOCKED | Empty scratch repo selected; synthetic tool-list prompt stopped by "Your session timed out. Sign in again to verify your identity." No acceptance session started |
| Codex desktop app | UNVERIFIED | No standalone app found in the installed application directories; no app was installed |
| Devin Desktop | UNVERIFIED | No standalone app found in the installed application directories; no app was installed |
| Claude Code for JetBrains | UNVERIFIED | No JetBrains app found in the installed application directories |

The user subsequently signed in. The Local Code messaging checks are recorded
below. A collection check from an earlier session does not substitute for
these messaging checks.

## 2026-10-10 manual: Claude Code for VS Code

VS Code `1.139.1`, extension `anthropic.claude-code@2.1.207`, fresh session
`25ca6094-b620-4180-9c7c-b394e37f2439`. Flopwire runtime `7cc9fd9e9148`.
The extension loaded the existing user plugin in the empty scratch repo.

| Case | Result | Evidence |
|---|---|---|
| Tools / presence | PASS | UI named peers/send/inbox; exact session was live idle with the scratch branch |
| Idle / prompt-submit | PASS | `mb1ef278b680fca74` remained queued for over a minute; UserPromptSubmit hook context contains it; UI quoted marker `MANUAL-CLAUDE-IDE-IDLE-20261010-A`; sender state read |
| Delivery at a tool boundary | PASS | `mdc54b64f39fff7f2` attached at PostToolUse:Bash after `echo first`; `ma736c4a92ea0d232` attached at PostToolUse of the approved synthetic reply; UI quoted both |
| Busy receipt | PASS | `m383fdaca02645352` sent while busy, receipt next_tool_call; no tool call remained in that turn, so it stayed pending until the next prompt |
| Threaded reply | PASS | One-time approval; `m40ff846c72e7d5e7` contains `MANUAL-CLAUDE-IDE-REPLY-20261010-A`, reply_to `m934e0f2600189de4`; later requester prompt received it and sender state read |
| Foreground-sleep checklist sequence | UNVERIFIED | Existing command policy blocked standalone sleep; background completion created a new prompt boundary |

Marker names are test labels, not presence evidence. Messages labelled
BUSY-A and BUSY-D were actually sent with idle/unknown receipts. A arrived
at UserPromptSubmit after background completion. D arrived at PostToolUse
of the reply tool. BUSY-B had an idle/unknown receipt at turn startup but
entered the echo tool's PostToolUse context. BUSY-C had a busy receipt
but was sent after the last tool call, and entered context at the next
prompt. These outcomes must not be combined into a foreground-sleep pass.

No global plugin or permission setting was changed. Install/remove and
fresh plugin trust review remain UNVERIFIED. Evidence is in the private
synthetic evidence directory noted above.

## 2026-10-10 manual: Codex IDE extension

VS Code `1.139.1`, active extension `openai.chatgpt@26.51007.21434`,
Codex backend `0.162.0-alpha.17.2`
(`source=vscode`, `originator=codex_vscode`), fresh session
`01a125e5-3eb3-7522-8c08-e96dd3981eab`, empty scratch repository.
Flopwire runtime `7cc9fd9e9148`.

| Case | Result | Evidence |
|---|---|---|
| Tools / presence | PASS | Discovery exposed peers/send/inbox; exact synthetic transcript matches the IDE prompt; session live idle on scratch repo |
| Idle interval | PASS | `mdb224a12dc68e291` stayed queued for over a minute without an automatic turn |
| Prompt-submit | FAIL | Generic prompt at 13:01:51Z produced "No <flopwire-message> tags have appeared in my context so far." Native transcript had no tag and sender state was still queued after that prompt |
| Idle recheck | PASS | `m75c89105ac6b2ec9` stayed queued for over a minute and entered next-prompt hook context at 13:07:08Z; model quoted marker B |
| Later prompt catch-up | PASS | Previously missed idle marker entered UserPromptSubmit context at 13:04:30Z, before any command |
| Mid-turn | PASS | `m49e7a72af88d3004` sent busy during sleep; hook context at 13:05:04Z precedes separate echo at 13:05:09Z; model quoted both markers |
| Threaded reply | PASS | MCP reply `m196ac9ed4cd3cd19` contains `MANUAL-CODEX-IDE-REPLY-20261010-A`, reply_to `m031127983117c9ab`; requester received hook context and sender state read |

This failure was observed on the installed IDE surface; interactive Codex
0.162.0 TUI hook delivery passed above. At the missed prompt, IDE Hook stats reported one completed UserPromptSubmit run with
zero blocked or failed runs. The later prompt recovered the marker without a setting change.
The missed first boundary needs diagnosis before changing settings.

The missed prompt boundary is tracked in [#228](https://github.com/flopwire/flopwire/issues/228).

## 2026-10-10 manual: Claude Desktop Local Code

Claude Desktop `2.31226.0`, bundled Claude Code `2.1.295`
(`entrypoint=claude-desktop`), Flopwire runtime `7cc9fd9e9148`.
After the user signed in, the retained synthetic prompt started fresh session
`c905bffe-e366-4e0a-b573-8e53e34b0622` in the empty scratch repository.
The real interactive Claude Code sender was
`abe8b2cd-9795-4c7e-a2bf-67c3ed1ddda7`, CLI `2.1.296`.

| Case | Result | Evidence |
|---|---|---|
| Existing plugin / tools | PASS | Setup check reported enabled; Desktop named all seven tools, including peers/send/inbox; SessionStart attached the standing messaging instructions |
| Presence | PASS | Exact native session was live and idle on `manual/messaging-20261010` in the scratch repository |
| Idle / prompt-submit | PASS | `m9cd12d3d7a1ed1d2` stayed queued with no automatic turn for over a minute; generic prompt at 14:05:02.656Z contained no marker; UserPromptSubmit attached it at 14:05:02.824Z; UI quoted `MANUAL-CLAUDE-DESKTOP-IDLE-20261010-A`; sender state read |
| Delivery at a tool boundary | PASS | `m46931cc7f1e2d459` entered PostToolUse:Bash context at 14:05:27.231Z after separate `echo first`, before `echo second` at 14:05:28.542Z; UI quoted `MANUAL-CLAUDE-DESKTOP-BOUNDARY-20261010-A`; sender state read |
| Threaded reply | PASS | Desktop sent one MCP reply `m048b4b6cb84565fd` containing `MANUAL-CLAUDE-DESKTOP-REPLY-20261010-A` with reply_to `m3ed3c6592a728095`; a fresh generic requester prompt received it through hook context; state read |
| Foreground-sleep / busy receipt | UNVERIFIED | This run used separate echo calls. The boundary marker's send receipt reported idle at turn startup; actual native hook context proves the tool boundary, not a busy receipt during foreground sleep |
| Global install/remove / fresh trust review | UNVERIFIED | Existing plugins and approval settings were retained |

The Desktop UI did not display the message wrapper as a separate chat item.
The native transcript records the hook attachment, and the UI response quotes
its id and marker. Both delivery markers had intent inform; the reply used a
separate request thread. No reply was sent for either inform message.
The existing Auto mode allowed the synthetic commands and MCP reply without
a new approval dialog. No app restart, upgrade or global setting change was
performed.

Private filtered synthetic transcripts and inbox receipts are in
`/tmp/flopwire-desktop-evidence-20261010` on the test Mac.
