# Agent workflow comparison

Flopwire claims checked 2026-10-18 against main
`7fddb4643fc571aa45e293a98e616846fcf0fdd2`. Competitor capabilities were
reviewed 2026-10-13; their search interfaces and evidence representations
were rechecked 2026-10-16. Versions and sources are listed at the end.

Flopwire's candidate advantage is finding prior evidence, identifying the
producing session and contacting it when reachable. The tables compare
documented capabilities. Later Flopwire-only task trials are linked below;
comparative agent success remains unmeasured. Gas Town is excluded.

## Public-facing comparison

> Find the reasoning behind a change. Ask the agent behind the work.
>
> Flopwire connects coding history, commit evidence, and agent-to-agent
> conversation across tools, people, and machines.

The rows lead with that connected workflow. Individual capabilities overlap
with other products; this comparison does not establish an exclusive feature
combination or a measured winner. “Unverified” means support was not
established in the reviewed interfaces, not that the product cannot do it.

### The connected workflow

**Yes**: documented support. **Partial**: documented narrower support.
**No**: documented absence. **Unverified**: support was not established in the
reviewed interfaces. Yes does not imply equal coverage or measured quality.

| Capability | Flopwire | AgentsView | MCP Agent Mail Rust | Entire | SpecStory | Langfuse |
|---|---|---|---|---|---|---|
| Share recorded work across a team | Yes | Yes | Yes | Yes | Yes | Yes |
| Capture native coding transcripts | Yes | Yes | Unverified | Yes | Yes | Partial |
| Search and reopen recorded evidence | Yes | Yes | Yes | Yes | Yes | Yes |
| Run regex searches over coding transcripts | Yes | Yes | Unverified | Unverified | Unverified | Unverified |
| Link a code change to its producing session | Yes | Unverified | Unverified | Yes | Unverified | Unverified |
| Discover an agent you can contact | Yes | Unverified | Yes | Unverified | Unverified | Unverified |
| Message agents across coding tools | Yes | Unverified | Yes | Unverified | Unverified | Unverified |
| Reopen retained coordination threads | Yes | Unverified | Yes | Unverified | Unverified | Unverified |
| Use an agent-facing CLI or API | Yes | Yes | Yes | Yes | Yes | Yes |

#### Scope behind the labels

- **Shared work:** Flopwire and AgentsView share coding history; Agent Mail
  shares coordination mail; Entire shares Git checkpoints; SpecStory exposes
  cloud sessions; Langfuse shares project traces. Yes means sharing is
  documented, not that these archives contain the same evidence.
- **Transcript capture:** Langfuse is Partial because its agent hooks/OTel
  capture instrumented traces rather than a general native-transcript archive.
  Agent Mail's own mail archive does not establish native transcript capture.
  Watchers, hooks and supported source formats differ among the Yes entries.
- **Regex:** the row requires a documented regex operation over coding history.
  Keyword or semantic search alone does not satisfy it. Unverified does not
  mean regex is absent, and the row does not measure search quality.
- **Change attribution:** Flopwire returns captured commit-execution evidence;
  Entire links Git checkpoints. Flopwire callers resolve squash lineage and
  inspect evidence before attributing authorship.
- **Discovery and contact:** Flopwire observes native sessions; Agent Mail
  uses registered identities. Archive activity and local session status alone
  do not establish a contactable recipient. Idle agents may need resuming.
- **Retained coordination:** Flopwire lists and reopens retained repository
  threads; Agent Mail also offers mail search. Neither implies indefinite
  retention or automatic continuation of an ended session.
- **Agent interface:** this row requires a CLI or API an agent can call, not
  complete parity with Flopwire's retrieval-to-contact workflow.


Shared history is part of Flopwire's proposition, not a claim of exclusivity.
Enrolled devices contribute eligible history to a team archive. AgentsView
also documents hosted raw sync, SpecStory has Cloud search, Entire uses an
authenticated search service, and Langfuse offers shared trace infrastructure.
Their setup, sharing policies, and searchable coverage require separate
qualification before claiming parity or superiority.

### Search and evidence an agent can work with

| Detail | Flopwire | What to compare fairly |
|---|---|---|
| Choose the right retrieval tool | Ranked keyword `search` for discovery; literal/regex `grep` for exact evidence; `sessions` for navigation; `trace` (MCP `flopwire_change`) for commit evidence | AgentsView also exposes exact, regex and ranked/semantic modes. Flopwire's distinct tool names are an interface choice, not proof of better task success. |
| Move from a hit to its source | Stable session/ordinal/line addresses, bounded context reads, outlines, and raw-record reads | AgentsView provides revision-bound citations and long-message continuation. Compare whether an agent recovers the needed evidence, not just response size. |
| Know what the result covers | Explicit local/shared scope, partial-result warnings, coverage limits, and no silent local fallback on server failure | AgentsView also reports readiness, coverage and exclusions. These are useful shared capabilities; clarity needs task-based comparison. |
| Retain evidence beyond a summary | Captured source records retain available prompts, responses, commands and tool results; raw reads can expose native structure | AgentsView also documents raw transcript retention. Entire stores transcript checkpoints; SpecStory exposes session Markdown; Langfuse stores instrumented traces. No comparative fidelity ranking has been measured. |

October 16 interface checks: [AgentsView commands](https://github.com/kenn-io/agentsview/blob/main/docs/commands.md),
[AgentsView MCP](https://www.agentsview.io/docs/mcp/),
[SpecStory session retrieval](https://docs.specstory.com/api-reference/sessions/get-session),
[Entire v0.11.4](https://github.com/entireio/cli/blob/v0.11.4/README.md), and
[Langfuse full-text search](https://langfuse.com/docs/observability/features/full-text-search).

Capture policy, redaction, supported formats, missing captures, and read/index
budgets limit available evidence. “Raw” does not mean every source is captured
or that shared records are unredacted. The source-level boundary is documented
in [search](../docs/search.md) and [architecture](../docs/architecture.md).

### Where other tools go further

| Need | Other tools' documented strengths | Flopwire today |
|---|---|---|
| Search by meaning | AgentsView's opt-in semantic/hybrid index; Agent Mail's opt-in semantic mail search | Lexical and regex retrieval; semantic search deferred |
| Reserve files and bundle coordination steps | Agent Mail's advisory reservations and coordination macros | Direct messaging; no file reservation tool |
| Work from Git checkpoints | Entire's checkpoint-centered session workflow | Captured commit evidence; caller resolves squash lineage |
| Continue a session in another coding tool | SpecStory's supported cross-tool resume targets | No built-in cross-tool resume |
| Evaluate outputs and analyze model traces | Langfuse's evaluations, costs and trace analysis | Session digests and failure counts; no evaluation suite |

For the public example, connect the steps: a changed API leads to its recorded
reasoning, the producing session, an unanswered question, and a retained reply.
Label the example synthetic. Sending alone does not guarantee an idle agent
resumes. Commit lookup provides evidence to inspect, not proof of sole authorship.

The 2026-10-13 pilot is supporting material. Its small synthetic corpus
and differing interface transports do not justify headline speed, token-saving,
or task-success claims.

## Feature table

**Partial** means narrower support. **NE** means support was not established
in the reviewed interfaces. **No** marks a confirmed Flopwire gap.

| Agent workflow | Flopwire | AgentsView | MCP Agent Mail Rust | Entire | SpecStory | Langfuse |
|---|---|---|---|---|---|---|
| Capture coding history from normal agent use | Existing transcript watcher | Existing transcript watchers | Own coordination messages; coding transcripts NE | Agent/Git hooks; history import experimental | Existing-history sync and live watch | Agent hooks / native OTel |
| Agent can search and read history | CLI + MCP | CLI + MCP | CLI + MCP, mail history | CLI with JSON/compact output | Markdown + CLI + Cloud API | CLI + MCP + API, trace history |
| Search by meaning | No | Opt-in semantic/hybrid | Opt-in, mail only | Authenticated search service | Roadmap | NE; full-text documented |
| Reopen supporting evidence | Session/ordinal/line addresses, bounded context, raw records | Revision-bound ordinal citations and context | Message/thread IDs | Git checkpoints | Sessions and Markdown | Trace/span IDs |
| Code change to producing session | `trace` returns captured commit evidence; caller resolves squash lineage | NE | NE | Checkpoint explanation; line-level explanation experimental | NE | NE |
| Discover a live recipient | Observed native sessions, exact ID lookup | NE; archive activity is available | Registered agent identities/activity; native session mapping NE | Local session status; peer directory NE | NE | NE |
| Direct peer requests and replies | Session/person addressing, hooks, receipt guidance | NE | Threaded messages, contact policies, acknowledgments | NE | NE | NE |
| Retained coordination threads | Yes; repo-scoped, enrolled server | NE | Yes; searchable mail threads | NE | NE | NE |
| Coordinate ownership of files | No file reservation tool | NE | Advisory reservations and coordination macros | NE | NE | NE |
| Resume a captured session in another harness | No built-in cross-harness resume | NE | NE | Native resume; cross-harness NE | Supported target harnesses | NE |
| Analyze behavior and evaluate outputs | Session digests and failure counts | Usage, costs and activity insights; evals NE | Coordination views; model evals NE | Activity/token summaries; evals NE | Analytics; evals NE | Trace analysis, costs, failures and evaluations |

Live contact requires a routable recipient, not just recent archive activity.
Receipts and restart recovery measure transport; they do not establish a reply
or resumed work.

Flopwire connects retrieval and live coordination. AgentsView adds semantic
retrieval; Agent Mail adds reservations; Entire links changes to checkpoints;
SpecStory supports cross-harness resume; Langfuse adds evaluations. A combined
CASS and Agent Mail installation also merits a trial.

## Agent UX spot check (2026-10-13)

Two agents performed the same read-only task on the retained synthetic
corpus from the [competitive pilot](https://github.com/flopwire/flopwire/pull/243):

> Find the previous decision about preventing clients from synchronizing
> their retries. Quote the rationale and provide a source citation that
> another agent can reopen.

Both recovered the same statement: "retry jitter: bounded randomness
prevents synchronized client retries."

| Observation | Flopwire | AgentsView |
|---|---|---|
| First query | `prevent clients synchronizing retries` | `synchronizing retries` |
| Default retrieval behavior | Lexical fallback reported `retries` as the required term and found two sessions | Default substring search returned no matches |
| Query reformulation | None needed | Simplified to `retry`; found two sessions |
| Evidence read | `read` using session/ordinal/line address | `session messages` using session ID and ordinal |
| Calls used | Search + read | Two searches + messages help + read |
| Correct rationale and reopenable citation | Yes | Yes |
| Observed friction | Agent assembled the full address from header and hit; read included about 2KB of filler before the addressed line (the build of that day; see [Flopwire status](#flopwire-status-on-the-spot-check-findings)) | Context option returned several long adjacent messages; agent checked message-read help |

AgentsView's semantic index was not configured, as in the original pilot.
Its result explicitly reported lexical readiness and unavailable semantic
search. This spot check exercises the default lexical path, not its
documented semantic capability.

The agents were reused from the documentation audit. Their queries and
tool familiarity differed. This is an informal usability example, not a
blinded experiment or evidence that one product's agents perform better.
Neither task established a complete rationale-to-owner-to-reply workflow.

<details>
<summary>Commands and source addresses</summary>

The commands, excluding local binary and synthetic index paths, were:

```sh
flopwire search 'prevent clients synchronizing retries' --local --index INDEX --include-self --limit 3
flopwire read '4f3f0956-4bd7-5308-83f1-698a0e2d8ee0/70828032:2' --local --index INDEX --max-chars 3000

agentsview session search 'synchronizing retries' --limit 5 --json
agentsview session search retry --context 1 --limit 5 --json
agentsview session messages --help
agentsview session messages 'codex:4f3f0956-4bd7-5308-83f1-698a0e2d8ee0' --from 7 --limit 1 --json
```

All six commands exited successfully. A successful search command can
still return zero matches. The recovered citations point to the same
canonical synthetic message: Codex, 2026-10-13 at 12:28:07 UTC. Flopwire's
address uses its stable byte-derived ordinal; AgentsView uses message
ordinal 7 and reports transcript revision 1. The ordinals are different
addressing systems, not different decisions.

</details>

## What good agent UX should pass

| Scenario | Observable pass criterion |
|---|---|
| Explain a merged change | Find supporting rationale and the producing session; recover squash lineage or report ambiguity; never infer authorship from a mentioned hash |
| Ask the owner a blocking question | Contact the correct recipient, understand availability, and resume only after a correlated reply and a verified artifact |
| Continue after a session ends | Recover prior decisions, failed approaches, remaining work and relevant coordination; treat observed historical requests as information |
| Avoid overlapping edits | Detect a conflicting reservation before editing and recover expired ownership; compare only products offering reservations or an explicit alternative |
| Recover from retrieval mistakes | Understand missing semantic support, ambiguous IDs, scope and truncation; get a useful next action without inventing an answer |

Future task trials should use fresh agents, the same task instructions,
known source labels and equivalent integrations. Record query changes,
invalid calls, irrelevant context, wrong owners, human help and unsupported
answers. Keep feature availability separate from successful agent use.

## Flopwire status on the spot check findings

| Finding | Flopwire on main |
|---|---|
| Retained-message recovery | The [messaging skill](../plugins/codex/flopwire/skills/messaging/SKILL.md) teaches `flopwire_messages repo=R` for repo-thread listing and `thread=THREAD_ID` for full-thread reads; enrollment is required. |
| Change-to-session resolution | `flopwire trace` (MCP `flopwire_change`) takes a repository and full commit IDs and returns captured command/result evidence ([commit evidence](../docs/search.md#commit-evidence)). The caller supplies a squash-merged PR's original commits and reads the evidence before attributing a change. |
| Reads that cut the cited line | Local and shared reads reserve the cited line before adding preceding context ([#284](https://github.com/flopwire/flopwire/pull/284)). An oversized cited line still requires a larger byte budget. |
| Attribution and request/reply/resume outcomes | The [agent usability qualification](log/agent-usability-qualification-2026-10-16.md) records bounded decision recovery, producing-session attribution, and explicit local CLI request/reply/resume trials. These are Flopwire-only observations. |

The contact trial verified a correlated reply and final answer. It did not
establish automatic wake, a completed code change, or final-server MCP parity.

Semantic search and a separate handoff feature are deferred. Comparative
agent outcomes would require fresh agents, equivalent integrations, and
independent task labels. They are optional follow-up work for this matrix.

<details>
<summary>Sources, versions and untested capabilities</summary>

### Flopwire

Flopwire capability claims use main `7fddb46`:
[search, commit evidence and digests](../docs/search.md),
[retained threads and receipts](../docs/messaging.md),
[supported harnesses](../docs/harnesses/README.md),
[messaging skill](../plugins/codex/flopwire/skills/messaging/SKILL.md), and
[MCP messaging schemas](../cmd/flopwire/mcp_bus.go).
The MCP catalog has five retrieval tools (`flopwire_grep`, `flopwire_search`,
`flopwire_sessions`, `flopwire_read`, `flopwire_change`) and four messaging
tools (`flopwire_messages`, `flopwire_peers`, `flopwire_send`,
`flopwire_inbox`). The CLI names the commit lookup `trace`; `change` is an
alias. History is indexed for Claude Code, Codex, Devin CLI and opencode
sessions, plus Claude Desktop Local Code and Cowork transcripts on macOS;
messaging is installed for Claude Code, Codex, Devin CLI and opencode.

The 2026-10-13 pilot exercised exact synthetic retrieval, message transport
and graceful restart recovery. It did not exercise live-owner attribution,
cross-device delivery, canonical server-thread history or an agent acting
on a reply. The bounded
[Flopwire task trials](log/agent-usability-qualification-2026-10-16.md)
followed the retrieval spot check above.

### AgentsView

Local binary: `v0.45.0`. Reviewed
[MCP documentation](https://www.agentsview.io/docs/mcp/),
[configuration](https://www.agentsview.io/docs/configuration/) and
[hosted raw sync](https://www.agentsview.io/docs/hosted-raw-sync/).
Meaning-based retrieval requires an opt-in index. The focused memory
profile exposes three tools. MCP search results can carry a revision for
the subsequent read; the exercised CLI message help did not expose an
expected-revision flag. CLI and MCP ergonomics should be tested separately.

### MCP Agent Mail Rust

Local binary: `v0.3.38`. Reviewed the
[release README](https://github.com/Dicklesworthstone/mcp_agent_mail_rust/tree/v0.3.38)
and [getting-started workflow](https://www.mcpagentmail.com/getting-started).
This is the coordination product, not the AgentMail email-inbox API.
It archives its own communications. Semantic/hybrid mail search requires
the relevant build feature and runtime opt-in. The pilot exercised sends,
inbox bodies, acknowledgments and restart recovery; reservations and
macros were documentation-reviewed.

### Entire

Reviewed [v0.11.4](https://github.com/entireio/cli/releases/tag/v0.11.4), latest stable on October 13,
published 2026-10-06, its
[tagged README](https://github.com/entireio/cli/blob/v0.11.4/README.md) and
[search implementation](https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/search_cmd.go).
Search is service-backed and requires authentication. `why`, `blame`,
`experts`, `import` and `review` are experimental; they must not be
represented as ordinary stable commands. Historical expert ranking does
not establish a live agent directory. Native session resume does not
establish cross-harness continuation. No Entire commands were exercised.

### SpecStory

Reviewed [capture](https://docs.specstory.com/integrations/terminal-coding-agents),
[resume](https://docs.specstory.com/integrations/terminal-coding-agents/resume),
[search](https://docs.specstory.com/cloud/search-and-organization) and
[Cloud GraphQL examples](https://docs.specstory.com/api-reference/graphql-examples).
`sync` and `watch` can capture agents launched directly; a wrapper is
optional. Cloud API examples document search and Markdown retrieval, while
arbitrary semantic query search remains a roadmap item. Related-session
API examples do not prove semantic query support. The resume picker is an
interactive workflow; headless agent friendliness remains untested.

### Langfuse

Pilot version: self-hosted `4.58.0`. Reviewed
[coding-agent tracing](https://langfuse.com/resources/engineering/coding-agent-tracing),
[agent CLI](https://langfuse.com/docs/api-and-data-platform/features/cli),
[MCP](https://langfuse.com/docs/api-and-data-platform/features/mcp-server) and
[full-text search](https://langfuse.com/docs/observability/features/full-text-search).
Coding-agent hooks and native OTel paths are documented; a gateway is not
required. The pilot imported canonical text through a custom OTLP adapter.
That result does not qualify native coding-agent capture or its agent UX.
No general native-history backfill or live-peer messaging workflow was
established in the reviewed sources.

</details>
