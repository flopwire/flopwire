# Agent communication homepage preview

Two homepage directions share the same examples and output contract. Open `index.html` to compare them. The production homepage is unchanged.

## Agent workflow

A client agent notices commit `a81f3c2` changing the `/users` response on `api-users`. It searches branch history, matches the recorded commit, checks the full session ID in live presence, and contacts that session. A static sequence shows the API diff, source-session discovery, agent question/reply, and client diff. Complete history, presence, and send records sit in one expandable exchange. The visual uses short message text; the full request remains in the send command. Agent messages have speaker labels. Commands and output have separate labels. Light code artifacts share a header, inset, and diff colors with setup and the grep outcome. Shared CSS tokens keep dark terminals and request messages consistent.

The preview shows complete captured CLI responses, without `jq` projections:

| Commands | Default output |
| --- | --- |
| `sessions`, `peers`, `send` receipt, `inbox` | JSON records |
| `grep`, `search`, `read` | Readable text with full-ID, `key=value` headers |

The same split applies to MCP. JSON lookups offer an explicit text view; transcript commands offer JSON. The project README and [decision on #55](https://github.com/flopwire/flopwire/issues/55#issuecomment-5940281818) document the contract.

## Examples and implementation

The branch-history example uses `fixtures/api-change.jsonl`, a synthetic transcript with a plain `git commit` tool call and a successful result. Its title comes from the first task prompt. Commit IDs are at `.sessions[].commits`; deeper digest metadata requires `--detail`.

The `sessions`, `peers`, `send`, `inbox`, grep, and read examples are captured from main `2d7b552`. The isolated capture harness supplies synthetic same-person sessions and presence, sends the request and reply through the local daemon, and captures both hooks. This proves the output and delivery path, not autonomous agent behavior or cross-person messaging. The separate recorded Claude/Codex exchange is linked from the hero; it is also same-person. Its commit list is empty because the API agent used `git commit -q` ([#80](https://github.com/flopwire/flopwire/issues/80)). Do not fill in that historical capture.

Request receipts include `next` guidance. Sender acceptance, the CLI adapters, setup, and delivery hooks are implemented. Cross-person messages wait for human acceptance through the console Messaging page or `flopwire accept USER` with a password. Messages arrive at a tool boundary or the human’s next prompt, never by waking an idle session.

The grep comparison retains `-n -F` and shows text on both sides. Headers lead with the full session ID and use `key=value`, JSON-quoting values containing spaces or quotes. Intent or title is last. Read uses `--messages-before` and `--messages-after`. MCP tools return one plain content block each, without structured content.

`fixtures/intended-*` filenames are retained, but now contain direct captured responses. See [the capture instructions](fixtures/README.md) and reproducible `capture.py`. The agent tool reference explains title provenance, commit attribution, addresses, formats, and reply correlation. Setup uses `flopwire setup` for harness plugins and starts the daemon separately.

## Preview checks

Check both directions on desktop and mobile. Check the expanded exchange, horizontally scrollable commands/JSON, wrapped transcript text, internal links, setup clipboard success and fallback, and the comparison controls. Parse the JSON examples and match the same full session ID across history, presence, and the send recipient. Keep search and read output as text. Check the message route separately from history sync, and verify `--no-sync` on local setup. The benchmark table is replaced by a compact comparison plan until results exist.
