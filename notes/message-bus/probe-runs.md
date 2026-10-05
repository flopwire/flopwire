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
