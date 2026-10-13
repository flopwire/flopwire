# Agent workflow comparison

Reviewed 2026-10-13. Flopwire source: `f83db684e912ea77c08ac02264996f55cf1f4993`.
Gas Town is excluded at the user's request.

Flopwire's strongest candidate advantage is the integrated route from
historical evidence to a live coding session and a new request. Several
alternatives already capture history and expose it to agents. This table
compares individual product capabilities; it does not establish a unique
feature or an overall UX winner.

## Feature table

**Yes** means documented native support, including configuration where
stated. **Partial** means a narrower or different workflow. **NE** means
not established in the reviewed interfaces; it does not prove absence.
**No** is used for explicit Flopwire gaps. These are capability labels,
not agent success scores.

| Agent workflow | Flopwire | AgentsView | MCP Agent Mail Rust | Entire | SpecStory | Langfuse |
|---|---|---|---|---|---|---|
| Capture coding history from normal agent use | Existing transcript watcher | Existing transcript watchers | Own coordination messages; coding transcripts NE | Agent/Git hooks; history import experimental | Existing-history sync and live watch | Agent hooks / native OTel |
| Agent can search and read history | CLI + MCP | CLI + MCP | CLI + MCP, mail history | CLI with JSON/compact output | Markdown + CLI + Cloud API | CLI + MCP + API, trace history |
| Search by meaning | No | Opt-in semantic/hybrid | Opt-in, mail only | Authenticated search service | Roadmap | NE; full-text documented |
| Reopen supporting evidence | Native byte addresses and context | Revision-bound ordinal citations and context | Message/thread IDs | Git checkpoints | Sessions and Markdown | Trace/span IDs |
| Code change to producing session | Partial: observed commits/PRs; manual resolution | NE | NE | Checkpoint explanation; line-level explanation experimental | NE | NE |
| Discover a live recipient | Observed native sessions, exact ID lookup | NE; archive activity is available | Registered agent identities/activity; native session mapping NE | Local session status; peer directory NE | NE | NE |
| Direct peer requests and replies | Session/person addressing, hooks, receipt guidance | NE | Threaded messages, contact policies, acknowledgments | NE | NE | NE |
| Retained coordination threads | Yes; repo-scoped, enrolled server | NE | Yes; searchable mail threads | NE | NE | NE |
| Coordinate ownership of files | No file reservation tool | NE | Advisory reservations and coordination macros | NE | NE | NE |
| Resume a captured session in another harness | No built-in cross-harness resume | NE | NE | Native resume; cross-harness NE | Supported target harnesses | NE |
| Analyze behavior and evaluate outputs | Session digests and failure counts | Usage, costs and activity insights; evals NE | Coordination views; model evals NE | Activity/token summaries; evals NE | Analytics; evals NE | Trace analysis, costs, failures and evaluations |

Sources and important qualifications follow below. A stored session's
recent activity does not establish a routable live agent. Acknowledgment
does not establish a substantive reply. Successful message recovery after
a restart does not establish task resumption.

## What the table says about choosing a product

| Product | Most relevant reason to compare it |
|---|---|
| Flopwire | Recover coding evidence, identify the responsible session, then contact that session or person across harnesses and machines |
| AgentsView | Richer historical retrieval, meaning-based search and revision-aware evidence reads |
| MCP Agent Mail | Explicit coordination: contacts, threads, acknowledgments, file reservations and macros |
| Entire | Git-linked checkpoints and explanations of the context behind changes |
| SpecStory | Captured session search and continuation into another supported agent |
| Langfuse | Inspection and evaluation of agent behavior, failures and cost |

These are workflow interpretations of documented capabilities. They are
not measured rankings. A combined CASS and Agent Mail installation is
also a relevant alternative to test; separate products can supply parts
of Flopwire's combined workflow.

## Agent UX spot check

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

## Flopwire priorities exposed by the comparison

1. **Teach the existing coordination history.** `flopwire_messages` already
   lists retained repo threads and opens full messages. It appears in the
   MCP catalog, but the main messaging skill does not teach it and the
   README still describes three messaging tools. Update the normal agent
   workflow before adding another history feature.
2. **Simplify change-to-owner resolution.** Commit digests record what the
   transcript proves. A squash-merged main SHA can require recovering the
   PR and original branch. Teach the fallback and investigate a resolver
   that returns evidence and ambiguity explicitly.
3. **Reduce context and address bookkeeping.** Full-address output and
   bounded reads already exist. Improve defaults or instructions using
   task trials before inventing another retrieval surface.
4. **Evaluate a concrete handoff view.** Recovering a transcript is only
   part of resuming work. A useful handoff names goals, decisions, rejected
   attempts, tests, blockers and source links. Compare this against native
   resume workflows and Agent Mail's thread macros.

## Evidence and version boundaries

### Flopwire

Reviewed source and documentation at main `f83db68`:
[search and digests](../docs/search.md),
[retained threads and receipts](../docs/messaging.md),
[messaging skill](../plugins/codex/flopwire/skills/messaging/SKILL.md), and
[MCP messaging schemas](../cmd/flopwire/mcp_bus.go).
The catalog has four retrieval and four messaging tools, including
`flopwire_messages`. The older README tool count is not the catalog.

The existing pilot exercised exact synthetic retrieval, message transport
and graceful restart recovery. It did not exercise live-owner attribution,
cross-device delivery, canonical server-thread history or an agent acting
on a reply. This review adds the informal retrieval spot check above.

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

Reviewed latest stable [v0.11.4](https://github.com/entireio/cli/releases/tag/v0.11.4),
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
