# Agent usability qualification

Reviewed October 15, 2026, against `54712c7` and the separately identified
fixes below. This is a bounded task review, not a comparative benchmark or
a claim that every supported client is qualified.

## Acceptance criteria

An agent must recover supporting evidence, distinguish an owner's decision
from a suggestion, identify a producing session from captured execution,
and distinguish message delivery from a correlated reply. An empty search
must not become a claim that no history exists. An absent peer must not
become a claim that the person cannot be reached elsewhere.

For each task, record the model, source revision, interface, exact calls,
errors, corrections, evidence read, and human intervention. Count retrieval
calls separately from tool discovery and GitHub calls. Keep raw private
history and authentication files out of the repository.

## Historical decision recovery

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

## Producing-session attribution

A separate fresh Sol agent investigated merged PR #256. It obtained original
PR commits through GitHub, used `flopwire_change`, read raw captured commands
and their results, corroborated the initial commit against GitHub, and checked
presence for the exact session. It used five Flopwire calls and three GitHub
calls. It correctly retained the limits of abbreviated hashes and incomplete
history, and reported that no live peer was returned.

The first lookup used `repo="flopwire/flopwire"` and failed with
`repo is required`. An absolute checkout path succeeded. This is a product
scope-normalization defect, not a failed attribution task. The fix is tracked
separately from this qualification record.

## Broad historical retrieval

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
is a product limitation. Continuation does not guarantee progress before
candidate retrieval starts. Ranked search helped discover examples; it did
not establish complete chronological results. Session-scoped retrieval was
fast once the relevant sessions were known.

## Request, reply, and explicit resume

The isolated CLI trial in [#287](https://github.com/flopwire/flopwire/pull/287)
uses two real Sol sessions and synthetic history. The requester discovers and
reads the original decision, checks the exact owner's presence, sends one
request, and handles an unknown-recipient refusal. The controller explicitly
resumes the owner. The owner reads its inbox and sends a reply linked to the
request. The resumed requester verifies the sender, correlation IDs, and
rationale token before answering.

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

## Installed-plugin hook delivery

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

## Changes and remaining qualification

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
  a universal startup bound or validate the original private-history timeout.
  The PR retains the experiment report and sanitized reproduction evidence.

The remaining qualification gates are:

- Repeat cited-line reading and repository-scope attribution on the final
  integrated build. The focused regression tests qualify the individual fixes.
- Repeat the original broad historical query against an updated shared server.
  Record elapsed time, candidate checks, completeness, and timeout recovery.
- Exercise ambiguous decisions and long-running MCP processes across credential
  rotation. The successful decision-recovery cases do not cover these paths.
- Keep GUI-client delivery and hosted concurrency qualification separate from
  the passing local CLI request/reply and installed-hook trials.

The historical recovery and attribution traces are summarized here rather than
committed verbatim because they contain private history. Their installed MCP
server revision was not pinned independently for each call. Their results are
workflow observations, not reproducible final-build acceptance tests.

## Earlier CASS usage

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
