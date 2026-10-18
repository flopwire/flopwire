# Agent interface comparison

Record of 2026-10-13. It describes the interfaces of each product as of that
date, against Flopwire source `208f48b`.

Scope: recover prior work, search by meaning, identify the producing session,
and contact another agent. See the [feature comparison](../agent-workflow-comparison.md)
for the broader capability inventory.

The useful comparison is how much work an agent must do between a question
and usable evidence or a reply. Tool availability alone does not answer that.

| Product | Agent's common path | Helpful interface choices | Remaining work for the agent |
|---|---|---|---|
| Flopwire | Search → read; Git/PR → sessions → peers → send | Familiar lexical/regex tools, transcript outlines, source addresses, explicit fallback and scope notes; send receipts suggest the next action | Reconstruct squash lineage; corroborate attribution; choose read bounds and sometimes combine address parts |
| AgentsView | Check readiness → search → read cited range | Focused three-tool profile, hybrid retrieval, explicit exclusions, revision-bound reads and long-body continuation | Configure embeddings; account for hidden recent/automated work; manage revision and paging arguments |
| Agent Mail Rust | Start/register → search mail → open thread → contact named agent | Setup/thread macros, optional search bodies, recoverable errors, contact policies and reply threading | Connect a registered identity to native coding history; reasoning never sent as mail is outside the archive |
| Entire | Search → explain checkpoint; or commit trailer → checkpoint | Compact search results, Git-linked identity, separate full-transcript expansion | Authenticate hosted search; resolve repository/checkpoint identifiers; avoid treating experimental commands as ordinary stable interfaces |
| SpecStory | API search → select session → fetch Markdown | Ranked snippets, GraphQL field selection and cursor paging; conditional reads with ETags | Construct API requests and select fields; full-session Markdown retrieval lacks a documented message-range read contract |
| Langfuse | Discover fields/filters → list observations → expand an observation | Compact defaults, field projection, schema discovery and stable observation/trace IDs | Translate a coding question into trace/filter vocabulary; a live coding-agent contact path is not established |

These are source-based interface assessments. Only the narrow retrieval
spot checks and Flopwire probes below were exercised. They do not rank agent
task success.

## What matters for Flopwire

1. **Open the evidence the search found.** A tight read budget should preserve
   the cited line. The current probe below needed an explicit line offset.
   Complete address output already exists; test simpler defaults before
   introducing a new citation format.
2. **Reduce change-to-session investigation.** Current instructions teach
   squash lineage recovery, but the agent still performs it manually. The
   [owner experiment](https://github.com/flopwire/flopwire/pull/252) recovers
   candidates, not verified authors. Entire's commit checkpoint is the
   relevant comparison for this task.
3. **Keep retrieval behavior explicit.** Flopwire's automatic term relaxation
   helped the tested paraphrase, but it is lexical retrieval. AgentsView's
   readiness, exclusions and stale-read recovery are useful contracts to
   compare when testing questions with different vocabulary.
4. **Prove the contact step.** Flopwire carries historical session identity
   into presence lookup and addressing. Its request receipt explains arrival
   and the next action. Compare this with Agent Mail's registered identities
   and macros using a fresh-agent task that requires an actual reply.

The former retained-message onboarding gap is fixed in
[#251](https://github.com/flopwire/flopwire/pull/251). The current skill also
teaches squash original-commit lookup. Neither change adds an owner resolver.

<details>
<summary>Exercised Flopwire interface probes</summary>

A CLI built from `208f48b` served MCP with an isolated home, `--local`, and
the retained synthetic pilot index. No real history or live messaging was used.

`initialize` and `tools/list` exposed eight tools. Shared instructions were
returned separately, not repeated in native tool descriptions. Retrieval
schemas had 34, 24, 16 and 12 properties for grep, search, sessions and read.
These counts describe the catalog, not an agent-performance score. A host can
present tool descriptions differently from the native server.

Five read-only calls tested the same synthetic rationale used in the
[spot check](#agent-ux-spot-check):

| Call | Observation |
|---|---|
| `search(query="prevent clients synchronizing retries", limit=3)` | Two hits; explicitly relaxed matching to required term `retries` |
| Same search with `no_heading=true` | Each hit included a complete read address with a session prefix |
| `read(address=SESSION/70828032:2, max_chars=500)` | Returned line 1, omitted the line-2 rationale, and suggested `line_offset=2` |
| Same read with `line_offset=2` | Returned the complete rationale within the focus budget |
| `read(address=ADDRESS, before=1)` | Tool error named `messages_before` and `messages_after` as the accepted alternatives |

The synthetic session was `4f3f0956-4bd7-5308-83f1-698a0e2d8ee0`.
The recovered rationale was: "retry jitter: bounded randomness prevents
synchronized client retries." Calls succeeded except the intentional invalid
argument, which returned `isError=true`. Coverage reported discovery unknown
because no device agent supplied discovery telemetry in this isolated setup.

These are interface probes by the reviewer, not fresh-agent trials. Earlier
Flopwire/AgentsView agents were reused; AgentsView semantic indexing was off.
No semantic ranking, verified-owner or real-reply outcome is established.

</details>

<details>
<summary>Sources and version boundaries</summary>

- Flopwire: [MCP schemas](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/cmd/flopwire/mcp.go),
  [messaging schemas](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/cmd/flopwire/mcp_bus.go),
  [messaging skill](https://github.com/flopwire/flopwire/blob/208f48b33f22d73f5d971e8b6859b4379158ada7/plugins/codex/flopwire/skills/messaging/SKILL.md).
- AgentsView: [MCP](https://www.agentsview.io/docs/mcp/) and
  [semantic search](https://www.agentsview.io/docs/semantic-search/).
  Current docs describe `expected_revision`, readiness, effective modes and
  exclusions. The earlier exercised v0.45.0 CLI did not expose the same
  revision argument. MCP and CLI need separate trials.
- Agent Mail Rust: [identity tools](https://github.com/Dicklesworthstone/mcp_agent_mail_rust/blob/0435b64d27c5f0245c3a7b410706b33c805d45a2/crates/mcp-agent-mail-tools/src/identity.rs),
  [search tools](https://github.com/Dicklesworthstone/mcp_agent_mail_rust/blob/0435b64d27c5f0245c3a7b410706b33c805d45a2/crates/mcp-agent-mail-tools/src/search.rs) and
  [README](https://github.com/Dicklesworthstone/mcp_agent_mail_rust/blob/0435b64d27c5f0245c3a7b410706b33c805d45a2/README.md).
  Semantic mail search needs build/runtime opt-in. `whois` recent commits
  concern the mail archive, not source-code authorship. Macro setup avoids
  requiring an agent to call each registration primitive separately.
- Entire v0.11.4: [search implementation](https://github.com/entireio/cli/blob/v0.11.4/cmd/entire/cli/search_cmd.go) and
  [README](https://github.com/entireio/cli/blob/v0.11.4/README.md).
  Search is authenticated and service-backed; compact output is available.
  `why`, `blame`, `experts`, `import` and `review` remain experimental.
- SpecStory: [GraphQL examples](https://docs.specstory.com/api-reference/graphql-examples),
  [session retrieval](https://docs.specstory.com/api-reference/sessions/get-session) and
  [authentication](https://docs.specstory.com/api-reference/authentication).
  ETags support conditional retrieval; a search-hit-to-read revision guard
  was not established. Related-session similarity does not establish
  arbitrary semantic-query search.
- Langfuse: [MCP schemas](https://mcp.reference.langfuse.com/) and
  [CLI](https://langfuse.com/docs/api-and-data-platform/features/cli).
  Observation tools require v4; metadata expansion is selective. These
  current contracts were reviewed, not exercised against the pilot stack.

</details>

## Agent UX spot check

Two agents performed the same read-only task on the synthetic corpus
from the [competitive pilot](competitive-benchmark-pilot-2026-10-13.md):

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
| Observed friction | Agent assembled the full address from header and hit; read included about 2KB of filler before the addressed line (build `208f48b`) | Context option returned several long adjacent messages; agent checked message-read help |

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
