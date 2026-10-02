# Retrieval tools: agent UX review

Date: 2026-09-30. Scope: `flopwire grep/search/sessions/read/raw` and the MCP server (`flopwire mcp`), as used by an LLM agent. Branch `feat/agent-ux`.

Method:
- A local index built from the synthetic oracle fixtures and from an 809 MB slice of the real corpus (three Claude projects and seven days of Codex rollouts, copied to scratch; nothing committed).
- Every verb and flag driven by hand, over the CLI and over MCP stdio.
- Five agent tasks run with `claude -p` (strict MCP config, flopwire tools only) before and after the fixes, and with `codex exec --ephemeral -s read-only` after.

## 1. What good agent-facing tools do (research)

Each rule carries its source. "Derived" marks my own inference.

1. **Qualifiers and totals before bodies.** Codex prefixes truncated output with `Warning: truncated output (original token count: N)` ([codex output-truncation](https://github.com/openai/codex/blob/main/codex-rs/utils/output-truncation/src/lib.rs)). Claude Code Grep reports a total even when `head_limit` cuts the list ([tools reference](https://code.claude.com/docs/en/tools-reference)).
2. **Cheapest shape by default; bodies on request.** Claude Code Grep defaults to `files_with_matches` ([tools reference](https://code.claude.com/docs/en/tools-reference)).
3. **Ranked lists: cap each hit and the hit count; don't cut the middle.** Tavily keeps at most 3 chunks of 500 chars per source ([Tavily](https://docs.tavily.com/documentation/api-reference/endpoint/search)). Brave keeps at most 5 extra snippets. Head+tail truncation is for logs (derived).
4. **Cap long lines and say so.** ripgrep `-M`/`--max-columns-preview` prints `[... omitted end of long line]` ([ripgrep guide](https://github.com/BurntSushi/ripgrep/blob/master/GUIDE.md)).
5. **Hosts cut long output themselves, and lose the tail.**
   - Claude Code writes Bash output over about 30k characters, and MCP output over 25k tokens, to a file and returns a 2,000-character preview. It warns above 10k tokens ([MCP](https://code.claude.com/docs/en/mcp), [tools reference](https://code.claude.com/docs/en/tools-reference)).
   - Codex middle-truncates tool output to a per-model budget, 10,000 bytes by fallback ([truncate.rs](https://github.com/openai/codex/blob/main/codex-rs/utils/string/src/truncate.rs)).
   - A tool that wants its footer seen must stay under these limits itself.
6. **Every truncation notice says three things:** what was returned, how much exists, and the exact call for more. "If you choose to truncate responses, be sure to steer agents with helpful instructions" ([Anthropic, writing tools for agents](https://www.anthropic.com/engineering/writing-tools-for-agents)).
7. **An empty page past the end must not read as "no matches."** Claude Code Grep says `No entries at this offset` ([tools reference](https://code.claude.com/docs/en/tools-reference)).
8. **Pagination: an opaque `nextCursor` plus a "more" flag.** See the [MCP pagination spec](https://modelcontextprotocol.io/specification/2025-06-18/server/utilities/pagination) and Sourcegraph's `hasMoreResults` ([Sourcegraph MCP](https://sourcegraph.com/docs/api/mcp)). Offsets suit line-addressed reads.
9. **Stable, short, meaningful ids that go straight into the next call.** Anthropic suggests resolving UUIDs to "semantically meaningful" language "(or even a 0-indexed ID scheme)" ([Anthropic](https://www.anthropic.com/engineering/writing-tools-for-agents)). The API docs say to return "semantic, stable identifiers" ([define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools)).
10. **Per hit, only the fields the next decision needs:** address, label, time, score and snippet. Exa and Tavily follow this ([define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools)). A `concise`/`detailed` switch saves about two thirds of tokens: 72 vs 206 in Anthropic's example.
11. **Errors state cause, fix and a valid example.** MCP tool failures are results with `isError: true`, so the model can correct itself ([MCP tools](https://modelcontextprotocol.io/specification/2025-06-18/server/tools)).
12. **Text for the model, JSON on request.** "There is no one-size-fits-all" ([Anthropic](https://www.anthropic.com/engineering/writing-tools-for-agents)). Markdown-like text costs 34–38% fewer tokens than JSON for nested data ([improvingagents](https://www.improvingagents.com/blog/best-nested-data-format/)). With MCP, `structuredContent` plus `outputSchema` carries the JSON side.
13. **Annotations.** Read-only tools declare `readOnlyHint: true` and `openWorldHint: false`, because the defaults are false and true ([tool annotations](https://blog.modelcontextprotocol.io/posts/2026-03-16-tool-annotations/)).
14. **Descriptions of 3–4+ sentences** covering what the tool does, when to use it and when not, and what each parameter means. Use unambiguous parameter names and service prefixes ([define tools](https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools)).
15. **One `--help` call shows everything, with examples.** This is progressive disclosure over preloaded instructions ([Willison](https://simonwillison.net/2025/Oct/16/claude-skills/), [Arcjet](https://arcjet.com/blog/designing-a-cli-for-ai-agents)).
16. **Default page 2–5k tokens, hard cap about 10k** for a ~200k-token agent. This is derived from points 5 and 10.

## 2. Audit (before this branch)

| Tool | Where output is cut | Metadata per hit | Hints | Gaps found |
|---|---|---|---|---|
| grep | `limit` 20 (max 500) hits; 10 matching lines per hit (`+N more`); 300 bytes per line around the match (`…`); identical texts collapsed (`+N copies`); counting stops 200 hits / 250 ms past the page (totals become `N+`); 200k candidates; 10 s budget (max 60 s); unindexed patterns scan 20k rows; injected text hidden; own session hidden | address, agent, kind(tool), time, repo basename, user (server), copies, superseded/off-path | footer totals and next offset; unindexed / timeout reason; self note | **no total output cap**: `-C 3 --limit 100` is 144 KB, past Claude Code's MCP cap, and a Bash caller loses the footer; notes printed after the hits; **no is_error**; times not marked UTC; no way to grep inside one session |
| search | `limit` 20; snippet around first match; 64 KB of text per row ranked (server); local rank cap 20k newest matches (note); any-term retry | as grep, plus snippet and line; score only in JSON | next offset; any-term note; rank-cap note | any-term note came **after** 19 unrelated hits (hallucination risk); no total; no session scope |
| sessions | `limit` 20; title cut to 100 chars | address, agent, last activity, repo basename, message count, user (server), parent | next offset | titles are often the raw first prompt, so agents read each session to learn what it was about |
| read | focus `max_chars` 4000 (unbounded above); neighbours a quarter each (min 200); before/after max 200; a session address shows 20 messages; the index stores capped text (the `--raw` hint) | header: agent, session id, repo/cwd, device, user, parent, title; per message: address, time, kind(tool), error/superseded/off-path | `lines a-b of N; more: line_offset=`; `[earlier messages: -B N]` | hints lack the address to go on from; no session time span or size; `max_chars=100000 -A 50` returned 160 KB |
| raw | none (up to 16 MB) | none | usage error names `read --raw` | fine for plumbing |
| MCP list | — | — | — | no `readOnlyHint`/titles; 17 shared arguments had **no description** in the schema (only in server instructions); unknown argument error named no alternatives |

## 3. Fixed on this branch

- **Output budget.** Every MCP answer stops at 24,000 bytes, about 6–8k tokens. It sits under Claude Code's 10k-token MCP warning. CLI output is not budgeted unless `--max-bytes N` asks for it (ripgrep's convention): a human redirecting to a file expects everything, and agent hosts cap Bash output themselves.
  - grep, search and sessions print whole hits until the next would pass the budget. The footer then reads `…; output budget of 24000 bytes reached; next: offset=K`.
  - read keeps the focus and the nearest neighbours and clamps `max_chars` to the budget. Its hints name the address to go on from: `flopwire_read address=X after=10`.
  - JSON is not budgeted.
- **Qualifiers first.** A timeout or partial reason, the any-term retry, the unindexed scan and the rank cap now print before the hits. The any-term note adds "(they may not answer the question)". The footer and the self-exclusion line stay last.
- **Self-exclusion line shortened.** It now reads "left out your own session S and its subagents; include_self=true includes them". It is in a separate `excluded` field, not in `notes`.
- **Times marked UTC.** Every timestamp ends in `Z`, and the help, instructions and schema say times are UTC. A baseline agent retried a filter with `-04:00` because it could not tell.
- **`is_error` on hits** (local and server). Grep and search mark failed tool calls and results `error` in the bracket.
- **`session` filter** for grep and search (`--session`, `session=`): one session, by any unique prefix, and its subagents. An unknown prefix is an error, not "no matches". A baseline agent tried `glob=` on grep for exactly this.
- **read header** adds the session's time span (UTC) and message count.
- **MCP schema:**
  - Every tool has a `title` and annotations (`readOnlyHint`, `idempotentHint`, `destructiveHint: false`, `openWorldHint: false`).
  - Every argument carries a description, because a client may drop server instructions.
  - Descriptions now say when to use each tool, when not to, and what gets cut.
  - The instructions tell the agent to read before relying on a hit, and to say the transcripts don't hold the answer rather than guess.
  - An unknown argument's error lists the tool's arguments.

## 4. Proposals (need a decision)

1. **Session digests in `sessions`.** In both runtimes, "what was each session about" cost one `read` per session: Claude 15 calls and 54 KB, Codex 49 calls and 90 KB. The title is often the raw first prompt, cut at 100 chars. A `detail`/`digest` option could print the first user prompt and the last assistant message, each clipped to about 300 chars. It needs a per-session query or a stored digest.
2. **Conversation outline read.** Agents emulate "show me the user/assistant turns of this session" with `grep '.' session=X kind=user,assistant`, an unindexed scan. Two options: `read` could take `kind`, or add an `outline` mode (one line per turn: address, time, kind, first 150 chars).
3. **Pair tool results with their call.** A `tool_result` hit shows output but not the command or file that produced it. In t2, agents could not name the file from the hit alone. Print the paired call's short form (e.g. `← Bash: rg -n FLOPWIRE_SWEEP cmd/`) in the bracket, or in read's header for the focus.
4. **Codex tool output is stored as JSON-escaped text** (`\n`, `\"`, `exit_code` wrappers). Hits show `…\"output\":\"Available…\n…`, which costs tokens and breaks line-based grep context. This is a parser change (internal/transcript, another stream).
5. **Addresses.** Ordinals are byte offset × 4096 (e.g. `agent-a85/7029981184`): stable but 10–12 digits per hit, and they look like positions. Options:
   - print a per-session turn number and keep the ordinal as an alias;
   - shorten the ordinal (base-36);
   - keep it as is, since agents never did arithmetic on it in these runs.
6. **Cursor instead of offset** for grep and search (MCP `nextCursor`). Offsets re-run the scan from the newest row each page. A cursor carrying (ts, id) would make page 2 cheap and stable under appends.
7. **structuredContent + outputSchema** for MCP. `format=json` is currently text holding JSON and has no budget. Tried and dropped on 2026-10-01 (§6): clients show a model `structuredContent` instead of the text (Claude Code) or beside it (Codex).
8. **Search totals and zero-hit terms.** Search prints no total. The any-term retry could also name the terms that match nothing (`zzqxv: 0 messages`), which is the strongest "not in the corpus" signal.
9. **A session address reads from the first message,** which for Codex is often a large system or developer prompt. Focusing the first user message would save a `line_offset` or `after` round trip.
10. **CLI budget for humans.** Piped CLI output is now budgeted like an agent's. A human exporting with `> file` gets the footer hint, or uses `--json`. If that is unwelcome, add `--max-output 0`.

## 5. Agent loop grades

Tasks:
- t1: a past `--since` parse error;
- t2: where `FLOPWIRE_SWEEP`'s default lives;
- t3: reconstruct the 09-29 Codex disk cleanup;
- t4: pandora sessions last week;
- t5: Redis/Helm decision (not in the corpus).

| Task | Claude before | Claude after | Codex after |
|---|---|---|---|
| t1 | right tool first; correct and found the later parser fix; 12 calls, 53 KB; retried a time filter in `-04:00` (UTC unclear) | right tool first; correct cause and workaround but missed the later parser fix (in another session); 8 calls, 28 KB; used `session=` | correct (same miss); 6 calls, 47 KB |
| t2 | correct; 3 calls | correct; 3 calls | correct; 2 calls |
| t3 | correct; 10 calls, 19 KB; failed `glob=` on grep | correct; **4 calls, 14 KB** via `session=` | correct; 10 calls, 27 KB |
| t4 | correct; 7 calls, 40 KB | correct; 15 calls, 54 KB (read every session) | correct; **49 calls, 90 KB** (see proposal 1) |
| t5 | correct "not in transcripts", no hallucination; 9 calls | correct; 7 calls; the any-term note shown first | correct; 6 calls |

No run hallucinated. Truncation hid no answer. The remaining waste is reading whole sessions to learn what they were about (proposals 1 and 2) and unindexed `.` greps used as an outline (proposal 2).

## 6. Output format (2026-10-01, issues #64, #84)

The decision on #55: record-shaped answers are JSON by default, text-shaped answers stay text.

- **sessions answers concise JSON by default,** on the CLI and in `flopwire_sessions`, like `peers`, `send` and `inbox`: `{"kind":"sessions","sessions":[…],"has_more","next_cursor"}`, one brief row per session (full `session_id`, agent, user, repo, branches, live, last activity, messages, title, intent, parent, commit ids, counts of files and failed calls). `--detail` (`detail: true`) prints main's `--json` rows with the whole digest. `--text` (`format: "text"`) prints rows.
- **grep, search and read stay text.** CLI `--json` is byte-for-byte unchanged (golden files captured from main).
- **Header lines** (grep/search session header, `sessions --text`, `grep -l`, read's header): the session id, then `key=value` fields one space apart. An empty value, or one with a space, quote, backslash, `=` or control character, prints as a JSON string; `intent` and `title` always do, and come last. Header times are `2026-09-23T10:00Z`, which `--since` accepts.
- **Hit lines keep rg's shape** (`ORDINAL:LINE kind/tool: text`); `--no-heading` is unchanged.
- **One text block per MCP answer; no `structuredContent`, no `outputSchema`,** on all seven tools. Claude Code 2.1.287 shows a model only `structuredContent` when a result has it; Codex shows both, the text JSON-escaped (review on #84). `format: "json"` returns compact JSON within the 24,000-byte budget as the text.
- **Errors in JSON mode** are one JSON object on stderr (`{"kind":"error","error":{"code","detail","fix","example"}}`), exit 1; over MCP the `isError` text is that object.

Cost, oracle fixtures (bytes of the default output, main → now):

| Query | main | now |
|---|---|---|
| grep, search (seven queries) | 5010 | 5369 (+7.2%) |
| `grep -l retr` | 590 | 695 (+17.8%) |
| read (three queries) | 3795 | 3867 (+1.9%) |
| `sessions` (13 sessions) | 2645 text | 3489 JSON (about 270 bytes a row) |
| 20 busy sessions (20 commits, 25 files each) | — | 16950 JSON |

Header forms tried on the grep and search queries: `key: value` two spaces apart 5731, one space 5640, the session bare and `key=value` 5369 (shipped). Instructions plus tool descriptions: main 9,340 bytes, now 9,335.
