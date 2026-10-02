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

## 7. Read context: lines or messages? (2026-10-02)

Question: `grep -A/-B/-C` (MCP `before`/`after`/`context`) count lines inside the matching message; `read -A/-B` (MCP `before`/`after`) count whole messages. Do agents confuse the two?

Method:
- Binary built from main at 3fd8b94 (after #84). Local index built with `agent run --once --no-sync` and `--claude-projects`/`--codex-home` pointed at a copy of the real corpus in scratch: three Claude projects (368 MB) and three days of Codex rollouts (241 MB), 427 transcript files. Nothing from it is committed.
- MCP only: `claude -p --tools "" --mcp-config F --strict-mcp-config --setting-sources project` (Claude Code 2.1.287), and `codex exec --ephemeral -s read-only` (codex-cli 0.160.0) with an isolated `CODEX_HOME`, shell, browser, apps and sub-agent features off, and the four retrieval tools auto-approved. The messaging tools were listed but had no agent behind them.
- Agents: Claude Code on Haiku and on Sonnet, Codex on `gpt-6-luna` (its cheapest listed model). Eight tasks, two runs each: 48 runs. Then a rename variant (below): T6–T8, one run each, 9 runs.
- Tasks (paraphrased):
  - t1: why a container job hit git's "dubious ownership" error, and the workaround.
  - t2: where a sweep-interval env var's default is set, and its value.
  - t3: reconstruct a Codex disk cleanup on one day.
  - t4: top-level sessions in one repo over two days, and what each was about.
  - t5: a Redis/Helm decision (not in the corpus).
  - **T6, previous message:** given a `fatal:` line from a tool result, quote the exact command that produced it. The answer is in the message before the hit.
  - **T7, next messages:** after an agent first saw a worktree error, what were its next three actions? The answer is the next six messages: three calls and their results.
  - **T8, lines in a message:** show the five lines before and after a failing-test line in a 72-line, 6 KB test log. The answer is lines inside one message.
- Measures come from the transcripts: every tool call's arguments and output bytes. Claude `-p` stream-json carried no visible thinking, and `codex exec --json` carries no reasoning text, so re-calls were classified from the call sequence and the brief preambles only.

### Results

Calls, KB of tool output and grade per run (r1 / r2). ✓ correct, ½ partly correct.

| Task | Haiku | Sonnet | Codex |
|---|---|---|---|
| t1 | 2, 12.5 ½ / 5, 24.7 ✓ | 4, 23.3 ✓ / 7, 37.6 ✓ | 4, 10.6 ✓ / 4, 23.4 ✓ |
| t2 | 3, 12.7 ✓ / 3, 9.7 ✓ | 2, 7.2 ✓ / 2, 8.1 ✓ | 2, 8.7 ✓ / 2, 6.9 ✓ |
| t3 | 16, 169.7 ✓ / 6, 48.3 ✓ | 5, 27.3 ✓ / 3, 16.3 ✓ | 13, 33.9 ✓ / 13, 42.4 ✓ |
| t4 | 1, 2.5 ✓ / 1, 8.5 ✓ | 2, 2.8 ✓ / 5, 26.7 ✓ | 1, 8.5 ✓ / 7, 108.4 ✓ |
| t5 | 3, 28.6 ✓ / 6, 19.5 ✓ | 2, 9.7 ✓ / 2, 9.7 ✓ | 5, 26.8 ✓ / 5, 23.2 ✓ |
| T6 | 2, 6.9 ½ / 2, 5.3 ½ | 2, 6.9 ✓ / 2, 3.1 ✓ | 3, 13.2 ✓ / 2, 8.7 ✓ |
| T7 | 2, 7.6 ✓ / 2, 9.8 ✓ | 2, 6.2 ✓ / 2, 6.2 ✓ | 2, 13.4 ✓ / 2, 5.8 ✓ |
| T8 | 2, 11.9 ✓ / 2, 11.9 ✓ | 1, 1.4 ✓ / 1, 1.4 ✓ | 2, 10.5 ✓ / 2, 13.8 ✓ |
| total | 58 calls, 390 KB | 44 calls, 194 KB | 69 calls, 358 KB |

The ½ grades: t1 Haiku r1 named a later exclusion as the workaround; T6 Haiku quoted the command without its leading `cd … &&`. No run hallucinated, and none passed 40 calls.

`flopwire_read` arguments (48 runs):

| | Haiku | Sonnet | Codex | All |
|---|---|---|---|---|
| read calls | 34 | 18 | 35 | 87 |
| with before/after | 25 | 11 | 16 | 52 |
| values used (value×count) | 2×1 3×5 5×12 10×5 15×4 20×5 25×1 150×1 | 1×1 2×5 3×5 4×1 6×1 8×3 12×2 | 1×13 2×3 3×3 5×4 8×4 10×1 16×1 | |
| reads with a value ≥10 | 16 | 2 | 2 | 20 |
| max_chars | 24000×13 | 1200×1, 1500×3 | 5000–12000×18 | |
| line_offset | 0 | 0 | 0 | 0 |
| outline | 3 | 1 | 8 | 12 |
| read bytes | 327 KB | 110 KB | 225 KB | 662 KB of 942 KB |

- Values ≥10 are not line counts here. 12 of Haiku's 16 come from t3: it walked one session by following each answer's `[later messages: … after=10]` hint with strides of 10–25 messages (each next address was the last one shown). The other eight are message counts on t1 and T7 (6 messages needed).
- 30 of 87 reads had their focus text cut (`lines a-b of N`). No agent ever passed `line_offset`. 3 reads (Codex outlines in t4) stopped at the 24,000-byte budget and said so.

**Corrective re-calls.** The rule: a read followed within two calls by a read on the same address or session with a smaller before/after, or a changed `max_chars`/`line_offset`. It flagged 9 of the 52 reads that used before/after:
- 8 were steps to a different message of the same session: following a "later messages" hint, or reading another search hit. That is paging, not correction.
- 1 re-read the same address with `before=5` after a focus-only read showed the error without its command. Message units were understood correctly.
- **None is attributable to lines versus messages: 0 of 52.** No model stated a belief about units either way (derived: there was little visible reasoning to catch one).

**T6 and T7 (messages wanted).** Every run used `before` (T6) or `after` (T7) in message units. 5 of 6 T6 runs and 6 of 6 T7 runs did so on the first read. One Codex T6 run read the focus first and then re-read with `before=3`. Values overshoot: T6 needs one message and runs used 2–5. T7 needs six and runs used 8–16.

| Task | Needed (estimate) | Read output |
|---|---|---|
| T6 | 1.5 KB (focus + 1 earlier message) | 2.7–8.2 KB |
| T7 | 2.6–4.5 KB (6 later messages) | 5.2–12.8 KB |
| T8 | 1.4 KB (`grep -C 5`) or 3.0 KB (read at `:LINE` alone) | 9.9–12.8 KB when before/after was passed |

**T8 (lines wanted): this is where units slipped.**
- Sonnet, both runs: `grep before=5 after=5`, then answered. 1 call, 1.4 KB.
- Haiku, both runs: `grep context=5`, whose output already held the answer, then `read address=…:LINE before=5 after=5`.
- Codex: `grep` without context, then `read …:LINE before=5 after=5` (r1) or `before=8 after=8` (r2).
- So **4 of 6 runs passed the task's line count to read's message arguments.** All four answers were right, because a `:LINE` address already focuses a window that starts five lines above the hit. The 5–8 neighbour messages on each side, about 7–10 KB per run, went unused. No agent noticed: it made no re-call and wrote nothing about it. The mismatch costs bytes and stays invisible. It did not cost correctness.

### Rename variant (throwaway branch, not pushed)

MCP `flopwire_read` arguments renamed `messages_before`/`messages_after`. The descriptions say "whole messages … for lines inside the focus use line_offset", and the instructions and hints use the new names. The CLI flags were left unchanged. T6–T8, three agents, one run each:

| Task | Haiku | Sonnet | Codex |
|---|---|---|---|
| T6 | 2, 3.2 KB ✓ `messages_before=2` | 2, 7.1 KB ✓ `messages_before=2` | 2, 6.1 KB ✓ `messages_before=1 messages_after=1` |
| T7 | 2, 3.3 KB ½ `messages_after=3` | 2, 5.8 KB ✓ `2/8` | 2, 6.3 KB ½ `2/8` |
| T8 | 2, 2.0 KB ✓ grep `context=5`; read `context="5"` (error) | 1, 1.4 KB ✓ grep `5/5` | 3, 8.4 KB ✓ grep `context=6`; read with no neighbours |

- T8: no run passed a line count to read (baseline 4 of 6). T8 bytes fell for Haiku (11.9 → 2.0 KB) and Codex (10.5–13.8 → 8.4 KB).
- T7: a new slip. Haiku read `messages_after=3` for "the next three things", but three actions span six messages (a call and its result each). It got two actions and split the first command into two. The Codex ½ is the same split, though it had read enough. n=1 per cell, so this is a direction, not a measurement.

### Other friction (first agent run on the #84 output)

- **`context` on read.** Haiku passed `context: "5"` (grep's argument) to `flopwire_read` in 3 of 16 baseline runs and 1 of 3 rename runs. The schema has no `context` for read, but the server accepts it as the CLI's `-C` (messages). The error was `context: want an integer`, which implies the argument is valid. It should say `flopwire_read` has no `context` and name `before`/`after` and `line_offset`.
- **Address taken from a grep context line.** Codex built `SESSION/ORDINAL-6` from rg's `ORDINAL-LINE-` context prefix (1 run). It got a "bad ordinal" error and recovered on the next call.
- **Dropped id prefix.** In a rename run, Codex dropped the `agent-` prefix of a subagent's full session id (1 run). It got "no session starts with …" and recovered.
- **Headers.** No `key=value` header was mis-parsed. 60 of 87 read addresses used the full session id copied from a header, and 27 used a short prefix. All resolved.
- **`sessions` JSON.** Every t4 run listed the sessions in one call (with `detail`, `exclude_subagents`, `since`/`until`). Haiku copied the `user` value from the JSON into a `user` filter (2 runs), which was harmless. One Codex run still read each session's outline (7 calls, 108 KB), as in §5's t4.
- **read drops requested neighbours without a note.** A session address with `after=150 max_chars=24000` returned the first message (a 23 KB Codex system prompt) and 2 neighbours. It printed no budget line, only `[later messages: … after=10]`. Reproduced on the CLI. With the default `max_chars`, 37 of 150 came back, also without a note. §3 says hints name where to go on, but here the answer reads as complete.

### Answer and recommendation

- **Agents are not confused in any way they notice.** 0 of 52 reads with before/after drew a units correction. On T6/T7 every model used message units correctly.
- **The mismatch shows only when the task is about lines.** On T8, 4 of 6 runs passed "five lines" to read's message arguments, and each over-fetched about 7–10 KB, silently.
- Against the decision rule:
  - Re-calls stayed far under one in five, at 0.
  - Line-sized values were routine only on the line-shaped task: 2 of 3 models, in both runs.
  - The rename removed that in the follow-up (0 of 3) at no cost in calls. Its one new slip, "three things" read as three messages, is cheaper to fix in the description than the silent over-fetch.
- **Recommendation: rename read's arguments to `messages_before`/`messages_after`** (CLI `--messages-before`/`--messages-after`; keep `-A/-B/-C` as lines only on grep). Also:
  - say in the description that a tool call and its result are two messages;
  - make read reject `context` with an error that names the right arguments;
  - make read say when the budget dropped requested neighbours ("showed 2 of 150 later messages; output budget").
  - The last two are worth doing whether or not the rename lands.

Limits: three models, eight tasks, two runs (rename: three tasks, one run). One corpus slice. T8's answer sat in a 72-line message that a `:LINE` focus happened to cover. With a target deeper in a longer message, the over-fetch would have missed the answer, and agents might have noticed (derived). Re-call classification relied on call sequences, because neither harness exposed the models' reasoning.
