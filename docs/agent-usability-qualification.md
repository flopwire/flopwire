# Agent usability qualification

## Current status: October 16, 2026

Four bounded fresh-agent tasks recovered the requested evidence:

| Task | Retrieval calls | Observed result and limits |
| --- | ---: | --- |
| Recover comparison decisions and latest next step | 6 | Recovered the owner's preference and final proposal; distinguished proposal from approval. Publishing and the current preview were outside scope. |
| Attribute an original PR commit to its producing session | 8 | Read captured command/result and benchmark evidence, coordination, and exact-session presence. The full original commit was supplied; GitHub discovery calls were excluded. An absent peer result did not explain the session's absence. |
| Recover the historical #228 decision | 6 | Recovered the original deferral and lack of an established fix under a historical cutoff. Evidence was sufficient after three calls; the rest added background. This historical answer does not describe the owner's current diagnosis scope. |
| Distinguish receipts from acknowledgment | 1 | Read a thread with two sender-authored messages and no recipient reply. Later read timestamps did not establish acknowledgment or completion. |

These observations support informed evidence recovery. They do not establish a
success rate, a general call-count improvement, or qualification of every
client. Live message delivery and fresh installation were not repeated.
The installed native MCP producer identified revision `f9124cd`; the persistent
exported MCP process and shared-server build were not independently pinned
for this audit. Host/server load was uncontrolled.

The original broad historical grep completed in 2.311 seconds with an exact
total of 231 hits. The response returned 10 of 35 requested hits because of
the response byte budget, with next offset 10. This was response pagination,
not scan truncation. It is one warm/live observation, not a cold-start bound.
[#291](https://github.com/flopwire/flopwire/issues/291) records the earlier
hosted replay after [#298](https://github.com/flopwire/flopwire/pull/298).
The original timeout case has passed replay; general grep qualification
remains open for cold starts, broader patterns, concurrency, startup tails,
and write/index impact.

The expanded repository-scope defect described below was fixed by
[#286](https://github.com/flopwire/flopwire/pull/286) and qualified across
CLI, MCP, authenticated HTTP, and PostgreSQL paths in
[#297](https://github.com/flopwire/flopwire/pull/297). Valid empty live results
establish scope acceptance; they do not establish captured commit evidence.
Current live probes also retained a cited focus line under a clipped read
budget, returned a complete exact empty result for a known absent literal,
and distinguished invalid regex, incompatible continuation options, changed
cursor filters, and expired snapshots from completed empty searches.

The owner has now reopened diagnosis of the unexplained Codex IDE delivery
miss in [#228](https://github.com/flopwire/flopwire/issues/228). No fix is
established. The observer correction in
[#310](https://github.com/flopwire/flopwire/pull/310) and opt-in diagnostics in
[#312](https://github.com/flopwire/flopwire/pull/312) are merged. Live IDE
qualification has not been run and remains pending. The original cause is
unresolved; later successful retries do not explain the original failure.

Native MCP initialization emitted shared instructions once (6,459 characters);
nine native tool descriptions totaled 8,481 characters. Exported discovery
metadata totaled 79,311 characters, including a 6,374-character common prefix
repeated for each tool. This confirms amplification after the native/export
boundary; the totals measure different objects. The reported 21K-token
bulk-discovery truncation was not independently reconstructed from the
retained raw retrieval results. Exported continuation guidance lagged native
guidance; an old process, host caching, or both could explain it. Existing
runtime receipt and version diagnostics can identify producer state but do
not prove which metadata the host cached or refresh it.

## Historical qualification record

The October 15 review used `54712c7` and the separately identified fixes below.
Its trial versions, failures, skips, and limitations remain historical evidence.

### Acceptance criteria

An agent must recover supporting evidence, distinguish an owner's decision
from a suggestion, identify a producing session from captured execution,
and distinguish message delivery from a correlated reply. An empty search
must not become a claim that no history exists. An absent peer must not
become a claim that the person cannot be reached elsewhere.

For each task, record the model, source revision, interface, exact calls,
errors, corrections, evidence read, and human intervention. Count retrieval
calls separately from tool discovery and GitHub calls. Keep raw private
history and authentication files out of the repository.

### Historical decision recovery

Two fresh `gpt-6.1-sol` agents investigated why the Codex IDE missed-message
investigation was deferred and whether a fix had been established. Both
recovered the owner's decision and correctly reported that successful
retries did not explain or fix the original miss. Neither needed mid-task human intervention. The repeat received researcher-provided strategy guidance before starting.

| Trial | Retrieval calls | Observed friction |
| --- | ---: | --- |
| Initial | 11: four searches, two greps, five reads | Broad search for deferral, an unrelated hit opened, issue-number noise, and further searches after sufficient evidence |
| Guided repeat | 6: two searches, one grep, three reads | One weak scoped search and excessive neighboring context |

The repeat used a historical cutoff to exclude the current evaluation and
received general strategy guidance: use topic terms, narrow follow-ups to
a relevant session, read nearby decision context, and stop when claims have
direct support. The task and guidance were selected after inspecting the
initial trace. These two observations do not establish a general percentage
improvement or a success rate.

Review of the original trace found a sufficient five-call path. Four of its
reads corroborated the incident, diagnostic result, owner decision, and
tracker update; some corroboration overlapped. The biggest avoidable costs
were exploration outside the relevant session and continuing after the
question had been answered.

Both agents printed excessive tool metadata during discovery. This belongs
to the orchestration setup; the trials do not establish that changing
Flopwire's native tool descriptions would remove it.

### Conflicting evidence and a result boundary

A fresh `gpt-6.1-sol` turn on Codex 0.162.1 used the combined `d253746`
binary against three synthetic sessions: an investigation with successful
workaround runs but no established cause, an unsupported assistant claim of
a fix, and the owner's explicit deferral. The neutral task asked whether the
issue was fixed and what its owner decided. It supplied no expected answer
and required an initial search with `--limit 1` to expose pagination.

The model used two searches, three reads, and one help call, all successful.
After reading the first session, it broadened discovery and inspected the
original investigation and owner decision. Its final answer rejected the
unsupported fix claim, cited the owner, retained the unknown cause, and
qualified the local index's unknown discovery coverage. Root reviewed the
prompt, returned evidence, and final answer. This single synthetic trial adds
confidence; it does not establish general relevance or exhaustive discovery.
The guarded authentication copy and daemon were removed afterward.

### Producing-session attribution

A separate fresh Sol agent investigated merged PR #256. It obtained original
PR commits through GitHub, used `flopwire_change`, read raw captured commands
and their results, corroborated the initial commit against GitHub, and checked
presence for the exact session. It used five Flopwire calls and three GitHub
calls. It correctly retained the limits of abbreviated hashes and incomplete
history, and reported that no live peer was returned.

The first lookup used `repo="flopwire/flopwire"` and failed with
`repo is required`. An absolute checkout path succeeded. This exposed the
repository-scope defect subsequently fixed and qualified by #286 and #297,
as recorded in the current status above.

### Broad historical retrieval

An exact-text grep for historical CASS invocations timed out before checking
candidates. A repository-scoped retry with a 60-second timeout eventually
returned results. A bounded follow-up compared recovery strategies:

| Call | Observed result |
| --- | --- |
| Quoted ranked search with the original date/kind filters | Ten hits in 4.35 seconds |
| Repository session listing with the date filter | Two sessions in 1.42 seconds |
| Literal continuation retaining a per-session hit cap | Rejected: that option is unsupported |
| Literal continuation without the cap, five-second budget | No candidates checked; snapshot restart required after 5.17 seconds |

The exact-text grep was a reasonable documented choice. Its startup latency
was the observed limitation in this historical trial. Continuation does not
guarantee progress before candidate retrieval starts. Ranked search helped discover examples; it did
not establish complete chronological results. Session-scoped retrieval was
fast once the relevant sessions were known.

### Request, reply, and explicit resume

The isolated CLI trial in [#287](https://github.com/flopwire/flopwire/pull/287)
uses two real Sol sessions and synthetic history. The requester discovers and
reads the original decision, checks the exact owner's presence, sends one
request, and handles an unknown-recipient refusal. The controller explicitly
resumes the owner. The owner reads its inbox and sends a reply linked to the
request. The resumed requester verifies the sender, correlation IDs, and
rationale label before answering.

The final guarded trial passed those assertions at runner `371a5a8`, using
a binary built from main `3fc0415`. The requester received
`only_if_resumed`; sending did not start a turn. With no delivery hooks in
this CLI fixture, an inbox read does not mark hook delivery. After the owner
resumes and ends again, the original request can show
`undelivered/session_ended` alongside an actual correlated reply. Those are
separate observations, not contradictory task outcomes.

The first attempt used an inconsistent sandbox on resume and failed truthfully.
A later run exposed an assertion that incorrectly required nine characters
of a session ID although the agent used a valid eight-character prefix.
The runner now checks returned identity. These failures do not establish
product failures. Earlier successful runs preceded the copied credential
guard; they remain separate from the final guarded pass.

The three task turns used 19 shell commands: nine help calls, one binary
existence check, and nine operational calls. These include the intentional
unknown-recipient refusal and one local-only `messages` error, recovered by
using `inbox`. Top-level help returned exit 1 despite printing usage;
[#290](https://github.com/flopwire/flopwire/pull/290) fixes that convention.
The [synthetic final report](https://github.com/flopwire/flopwire/pull/287#issuecomment-6193869208)
records the actual request and reply correlation.

A combined-build repeat passed at temporary integration `d253746`, containing
main `237b144` plus #286 (`f84ec01`), #289 (`020718d`), #290 (`9004471`),
#292 (`690a4fb`), and #287 (`5ecd8b7`). It passed the same guarded source,
refusal, ended-owner receipt, actual reply correlation, and final-answer
assertions. Its 20 shell commands included nine successful help calls and
expected unknown-recipient and server-only command errors. The latter was
recovered through `inbox`. Authentication copies and the daemon were removed.

An earlier combined run failed the exact-answer assertion because the model
formatted the synthetic rationale as `token: decision-...`, triggering the
expected assignment redactor. The fixture now calls it a label. Redaction
rules and exact-answer assertions were not relaxed. The failed report is
retained alongside the passing report. A subsequent wording-only correction
at #287 `ed4b4da` removes one remaining prompt reference to “token”; four
synthetic tests passed, without another paid workflow run.

A final product-build trial used `1ff3a82051a2bfadf082e5b888cf8238d840954b`
(binary SHA256 `36360505cf78132987a969ac68f499563554a7f187f94fe1ce303f5e8354b5a5`)
with the merged runner `e23125a`. The requester successfully read ordinary text
output, but the runner accepted only JSON and stopped before the reply turns.
[#294](https://github.com/flopwire/flopwire/pull/294), reviewed at `d0c75bb`,
recognizes native text while retaining exact owner identity and nonempty source
evidence checks. Five Python tests cover both formats and rejection cases.

A guarded continuation reused the same two sessions, original source, request,
and expected answer. It did not repeat the initial model turns. All source,
refusal, ended-owner receipt, exact owner/reply correlation, and final-answer
assertions passed with the fixed verifier. The four total model turns used
19 shell commands, including eight successful help calls. The unknown-recipient
refusal and server-only `messages` lookup were the expected nonzero results;
the requester recovered the latter through `inbox`. The original failure report
is preserved separately from the continuation report. Authentication was removed
and the daemon stopped. This qualifies explicit local CLI resume and reply;
it does not demonstrate automatic wake or final-server MCP behavior.

### Installed-plugin hook delivery

The local installed-plugin probe passed `prompt-submit`, `framing`, and
`hook-binary` on Codex 0.162.1 with `gpt-6.1-sol`, using candidate `18c126e`.
The markers appeared in native `hooks.additional_context` developer records.
Tool output or an agent quoting a marker did not count as delivery evidence.
Copied credentials were removed and scratch processes stopped afterward.

This required [#289](https://github.com/flopwire/flopwire/pull/289). The prior
probe rejected credentials by age even when their readable token expiry was
safe. Verified Codex 0.160.0 and 0.162.1 code gives readable expiry precedence
and uses age only as a fallback. The corrected guard preserves its 30-minute
safety horizon and conservative refusal when expiry is unreadable.

This local CLI probe does not close the unexplained Codex IDE missed prompt
in #228 or qualify every supported client and device combination.

### Changes and remaining qualification

- [#284](https://github.com/flopwire/flopwire/pull/284) ports the cited-line
  read fix onto current main. Local and shared reads reserve the cited line
  before adding preceding context. Explicit offsets retain sequential paging.
  Local race tests, six local/server parity cases, and isolated E2E passed at
  `069537c`. An oversized cited line still requires a larger byte budget.
- [#286](https://github.com/flopwire/flopwire/pull/286) accepts validated
  expanded repository scopes in commit lookup. Missing scopes and resolver
  errors remain errors; authenticated owner/device restrictions remain intact.
- [#285](https://github.com/flopwire/flopwire/pull/285) documents continuation
  option restrictions and recovery from timeouts before candidate checks.
  It does not fix broad-grep startup latency.
- [#291](https://github.com/flopwire/flopwire/issues/291) tracks first-candidate
  latency. Isolated measurements found repeated text rechecks across Unicode
  fold alternatives. [#292](https://github.com/flopwire/flopwire/pull/292)
  removes that amplification for eligible folded literals. On a synthetic
  production `Store.Grep` fixture, first-page means for `cass search` fell
  from 2283/2321 ms to 326/218 ms for ordinary/continuation requests. All
  ordered first-page IDs matched. These warm three-call means do not establish
  a universal startup bound. Later hosted replay of the original case is
  recorded in the current status above.
  The PR retains the experiment report and sanitized reproduction evidence.

Focused race regressions also passed against exact final product source
`1ff3a82`: cited-line cuts and read budgets, expanded repository scopes,
change-query validation/cursor binding, MCP scope expansion and rejection,
HTTP authentication/scope contracts, and local completion attribution.
`TestFocusedReadLocalServerParity` was explicitly skipped because this final
run had no isolated PostgreSQL configured; earlier local/server parity remains
separate evidence. These checks qualify final-source behavior, not a live
updated installed-MCP or shared-server replay.

The remaining qualification gates are:

- Extend general grep qualification beyond the completed original hosted
  replay: cold starts, broader patterns, concurrency, startup tails, and
  write/index impact remain separate measurements.
- Keep GUI-client delivery and hosted concurrency qualification separate from
  the passing local CLI request/reply and installed-hook trials.

Long-running MCP credential rotation already has recorded subprocess E2E
qualification in [the v0.1.0 readiness record](releases/v0.1.0-readiness.md),
with maintained coverage in `internal/e2e/mcp_rotation_test.go`. Targeted client
race tests passed again at `237b144`. Incomplete-result behavior has CLI and MCP
regression coverage: complete empty results succeed, zero-hit timeouts return
an error, and useful partial hits retain explicit limits. The CLI warns that
no absence conclusion is justified. These targeted race tests also passed at
`237b144`; they establish output behavior, not universal model interpretation.
The conflicting-evidence trial above adds model-behavior confidence; no
observed ambiguity-handling defect currently makes it a release blocker.

The historical recovery and attribution traces are summarized here rather than
committed verbatim because they contain private history. Their installed MCP
server revision was not pinned independently for each call. Their results are
workflow observations, not reproducible final-build acceptance tests.

### Earlier CASS usage

A retained September 29 audit counted 1,532 CASS command segments across
165 sessions, including 889 searches, 114 expansions, 80 exports, and 18
views. Its common workflow was ranked keyword search followed by evidence
inspection. Agents sometimes piped exports into `rg`; CASS itself was not
being used primarily through a grep-shaped API.

That audit also reported help lookups in 123 sessions, 44 empty results among
870 searches with captured output, and approximately 95 searches with error
text. An independently reopened historical invocation confirmed an output
envelope parsing mistake. These are historical observations, not recomputed
rates or a controlled comparison with Flopwire. They do not establish that
the old installation used semantic retrieval.

## Portable guidance experiment — October 16, 2026

The guidance candidate at `8e7dfa5` preserves the tool API and moves procedural
detail from shared instructions into relevant tool descriptions. Native
initialize instructions shrink from 6,459 to 3,057 characters. Combined shared
instructions and nine tool descriptions shrink from 14,940 to 12,309 characters
(17.6%). These are native payload measurements; plugin-host exported metadata
was not measured.

Four held-out synthetic scenarios each ran once with baseline and candidate
metadata in fresh Codex 0.163.0 `gpt-6.1-sol` sessions at low reasoning effort.
Both arms passed all four core checks: owner reversal over a contrary assistant
summary, a decisive line clipped from search, failed command evidence behind a
commit, and delivery receipts without a correlated reply. The baseline commit
answer added unsupported branch and retry-absence claims; the candidate kept
those claims bounded in this sample. Calls totaled 18 for baseline and 19 for
candidate. This trial establishes no speed or call-count improvement.

The experiment replayed native metadata against deterministic fixtures with
simplified search ranking and filtering. Read fixtures modeled selected native
controls. It did not qualify plugin routing or live retrieval dispatch. Provider
prompt caching occurred despite fresh sessions and native initialization.
There was one sample per scenario and arm; the results establish no statistical
quality improvement. The earlier v1 smoke trial is excluded from this stronger
acceptance record. Private evidence is retained in
`agent-usability-guidance-20261016/acceptance-v2-run-1/manual-review.json`.

The separate observer correction merged in
[#310](https://github.com/flopwire/flopwire/pull/310). Diagnostics in
[#312](https://github.com/flopwire/flopwire/pull/312) remain pending. The scratch
IDE check was not run because the UI controller selected the existing window
instead of the isolated process. No diagnostic configuration was activated and
no markers were sent. None of these records resolves the unexplained
original IDE failure in #228.
