# Agent interface comparison

Reviewed 2026-10-13. Flopwire source: `208f48b`.
Scope: recover prior work, search by meaning, identify the producing session,
and contact another agent. See the [feature comparison](agent-workflow-comparison.md)
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
[earlier spot check](agent-workflow-comparison.md#agent-ux-spot-check):

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
  [current messaging skill](../plugins/codex/flopwire/skills/messaging/SKILL.md).
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
