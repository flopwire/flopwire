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
retries did not explain or fix the original miss. Neither needed human help.

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

## Changes and remaining qualification

- [#284](https://github.com/flopwire/flopwire/pull/284) ports the cited-line
  read fix onto current main. Local and shared reads reserve the cited line
  before adding preceding context. Explicit offsets retain sequential paging.
  Local race tests, six local/server parity cases, and isolated E2E passed at
  `069537c`. An oversized cited line still requires a larger byte budget.
- Commit lookup needs the repository-scope normalization fix found above.
- MCP guidance needs the continuation option restrictions and a recovery path
  for timeouts before any candidates are checked.
- A real-agent request/reply trial is being qualified separately. Transport
  tests alone do not establish that an agent identifies the correct recipient,
  receives a correlated answer, and uses it to complete the task.

Before declaring the workflow qualified, repeat the affected tasks on the
final code. Include unavailable recipients, ambiguous evidence, partial
results, and old MCP processes. Keep GUI-client delivery limitations and
hosted concurrency claims separate from a passing local CLI trial.
