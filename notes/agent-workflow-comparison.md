# Agent workflow comparison

Flopwire refreshed 2026-10-16 against `2cd14559e0b0339ebbd6378078bb2693a5603aa1`.
Competitor capability review: 2026-10-13; versions and sources below.
The October 13 spot check is retained as historical evidence.

Flopwire's candidate advantage is finding prior evidence, identifying the
producing session and contacting it when reachable. The tables compare
documented capabilities. Later Flopwire-only task trials are linked below;
comparative agent success remains unmeasured. Gas Town is excluded.

## Public-facing matrix drafts

Both versions use the same proposed positioning:

> Find the reasoning behind a change. Ask the agent behind the work.
>
> Flopwire connects searchable coding history, commit evidence, and direct
> messages between coding agents.

These are copy drafts for comparison, not changes to the published homepage.
“Not established” means the reviewed interfaces did not establish support;
it does not mean the product cannot do it. Features are documentation-backed
unless the evidence section records a trial.

### Version A: closest alternatives

| What an agent needs | Flopwire | AgentsView | MCP Agent Mail Rust | Entire |
|---|---|---|---|---|
| Recover earlier work | Search and read captured coding sessions | Search and read captured coding sessions | Search coordination messages | Search sessions; Git-linked checkpoints |
| Search by meaning | Not supported | Opt-in semantic/hybrid index | Opt-in semantic mail search | Authenticated search service |
| Connect a change to its source | Commit lookup with captured execution evidence | Not established | Not established | Git-linked checkpoints |
| Ask another agent a follow-up | Session/person addressing and threaded replies | Not established | Registered agent identities and threaded replies | Not established |
| Reserve files before editing | Not supported | Not established | Advisory file reservations | Not established |

Flopwire's proposed reason to choose it is the connected path from archived
reasoning to a reachable agent. AgentsView offers semantic session retrieval;
Agent Mail adds file reservations and coordination macros; Entire organizes
session evidence around Git checkpoints. The table does not establish which
product completes a task more reliably.

### Version B: broader shortlist

| What an agent needs | Flopwire | AgentsView | MCP Agent Mail Rust | Entire | SpecStory | Langfuse |
|---|---|---|---|---|---|---|
| Recover earlier work | Coding sessions | Coding sessions | Coordination messages | Sessions/checkpoints | Sessions/Markdown | Traces/spans |
| Search by meaning | Not supported | Opt-in | Opt-in, mail only | Authenticated service | Roadmap in reviewed docs | Not established |
| Connect a change to its source | Captured commit evidence | Not established | Not established | Git checkpoints | Not established | Not established |
| Ask another agent a follow-up | Session/person messages | Not established | Registered-agent messages | Not established | Not established | Not established |
| Reserve files before editing | Not supported | Not established | Advisory reservations | Not established | Not established | Not established |
| Resume in another coding tool | No built-in workflow | Not established | Not established | Cross-tool support not established | Supported target tools | Not established |
| Evaluate model outputs | No evaluation suite | Not established | Not established | Not established | Not established | Evaluations |

Version A keeps the decision focused on session retrieval, coordination, and
Git provenance. Version B also shows why someone might choose SpecStory for
cross-tool continuation or Langfuse for evaluation. It is wider and includes
products serving different primary needs.

For either version, explain the contact limit alongside the example: sending
a message alone does not guarantee an idle agent resumes. Commit lookup returns evidence
to inspect, not a guarantee of sole authorship; squash merges require the
original PR commit IDs. See the source-backed detail below.

No speed or token-saving claims belong in these drafts. The retained pilot
compares a small synthetic corpus and different interface transports. Its
numbers do not establish comparative task success or production-scale speed.

## Feature table

**Partial** means narrower support. **NE** means support was not established
in the reviewed interfaces. **No** marks a confirmed Flopwire gap.

| Agent workflow | Flopwire | AgentsView | MCP Agent Mail Rust | Entire | SpecStory | Langfuse |
|---|---|---|---|---|---|---|
| Capture coding history from normal agent use | Existing transcript watcher | Existing transcript watchers | Own coordination messages; coding transcripts NE | Agent/Git hooks; history import experimental | Existing-history sync and live watch | Agent hooks / native OTel |
| Agent can search and read history | CLI + MCP | CLI + MCP | CLI + MCP, mail history | CLI with JSON/compact output | Markdown + CLI + Cloud API | CLI + MCP + API, trace history |
| Search by meaning | No | Opt-in semantic/hybrid | Opt-in, mail only | Authenticated search service | Roadmap | NE; full-text documented |
| Reopen supporting evidence | Native byte addresses and context | Revision-bound ordinal citations and context | Message/thread IDs | Git checkpoints | Sessions and Markdown | Trace/span IDs |
| Code change to producing session | Captured commit evidence; caller resolves squash lineage | NE | NE | Checkpoint explanation; line-level explanation experimental | NE | NE |
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

## Historical agent UX spot check (October 13)

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
| Observed friction | Agent assembled the full address from header and hit; read included about 2KB of filler before the addressed line | Context option returned several long adjacent messages; agent checked message-read help |

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

## What changed after the comparison

| Earlier finding | Current status |
|---|---|
| Retained-message recovery was poorly taught | [#251](https://github.com/flopwire/flopwire/pull/251) updated onboarding; the messaging skill teaches repo-thread listing and full-thread reads. |
| Change-to-session resolution required transcript discovery | [#256](https://github.com/flopwire/flopwire/pull/256) added `flopwire_change`; [#286](https://github.com/flopwire/flopwire/pull/286) corrected expanded repository scopes; [#297](https://github.com/flopwire/flopwire/pull/297) added end-to-end coverage. Read captured execution evidence before attributing a change. |
| Tight reads could omit the cited line | [#284](https://github.com/flopwire/flopwire/pull/284) fixed local/shared reads, superseding #254. Oversized lines still require a larger budget. |
| Attribution and request/reply/resume outcomes were untested | [Agent usability qualification](../docs/agent-usability-qualification.md) now records bounded historical recovery, producing-session attribution, and explicit local CLI request/reply/resume trials. These are Flopwire-only observations. |

The contact trial verified a correlated reply and final answer. It did not
establish automatic wake, a completed code change, or final-server MCP parity.
The October 13 spot check above preserves the behavior actually observed on
that build; its cited-line friction is no longer a current product gap.

Semantic search and a separate handoff feature remain deferred. Comparative
agent outcomes would require fresh agents, equivalent integrations, and
independent task labels. They are optional follow-up work for this matrix.

<details>
<summary>Sources, versions and untested capabilities</summary>

### Flopwire

Current Flopwire capability claims use main `2cd1455`:
[search and digests](../docs/search.md),
[retained threads and receipts](../docs/messaging.md),
[messaging skill](../plugins/codex/flopwire/skills/messaging/SKILL.md), and
[MCP messaging schemas](../cmd/flopwire/mcp_bus.go).
The catalog has five retrieval and four messaging tools, including
`flopwire_change` and `flopwire_messages`. Commit evidence and its limits are
documented in [search](../docs/search.md#commit-evidence).

The existing pilot exercised exact synthetic retrieval, message transport
and graceful restart recovery. It did not exercise live-owner attribution,
cross-device delivery, canonical server-thread history or an agent acting
on a reply. The informal retrieval spot check above was followed by the bounded
[Flopwire task trials](../docs/agent-usability-qualification.md).

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
