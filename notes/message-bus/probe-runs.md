# `flopwire probe` runs

One section per hand run of `flopwire probe --notes`, oldest first. See
[`docs/probe.md`](../../docs/probe.md) for what each case proves.

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
