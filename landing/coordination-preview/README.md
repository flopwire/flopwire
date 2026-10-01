# Agent communication homepage preview

Two homepage directions share the same examples and output contract. Open `index.html` to compare them. The production homepage is unchanged.

## Agent workflow

A client agent notices commit `a81f3c2` changing the `/users` response on `api-users`. It searches branch history, matches the recorded commit, checks the full session ID in live presence, and contacts that session. Presence is expandable to keep the story readable. Narration stays outside terminal panels. Commands and output have separate labels.

The preview shows intended lean responses directly, without `jq` projections:

| Commands | Default output |
| --- | --- |
| `sessions`, `peers`, `send` receipt, `inbox` | JSON records |
| `grep`, `search`, `read` | Readable text with labeled metadata headers |

The same split applies to MCP. JSON lookups offer an explicit text view; transcript commands offer JSON. The project README and [decision on #55](https://github.com/flopwire/flopwire/issues/55#issuecomment-5940281818) document the contract.

## Examples and implementation

The branch-history example derives its title and commit from `fixtures/api-change.jsonl`. The commit is extracted from a successful git commit tool result. The stored title comes from the first task prompt. The displayed JSON preserves the `sessions` envelope and existing field names. Its lean default is an intended response; the `sessions` default change is pending.

Presence and send panels are labeled **Contract example**. The message bus server (#41, #51) and device agent (#49) are merged. The messaging CLI is in progress on `feat/bus-cli`. The recipient hook is not built. A queued receipt does not demonstrate delivery. The final reply and code change show the intended exchange after delivery; they are not a captured live exchange.

The grep comparison keeps shared `-n -F` flags and shows text on both sides. File output is captured from system grep. Transcript content and addresses come from the pagination fixture. Labeled headers are the intended format; installed output still uses positional headers. Re-capture after the header follow-up lands. `read` keeps transcript text and role labels visible without JSON escaping.

`fixtures/intended-*` files contain the displayed contracts. Earlier captures remain in `fixtures/` as source evidence. See its README for reproduction steps. `landing/agent-tools.md` explains title provenance, commit attribution, addresses, formats, and implementation status.

## Preview checks

Check both directions on desktop and mobile. Check expanded presence, terminal wrapping, internal links, setup clipboard success and fallback, and the comparison controls. Parse the JSON examples and match the same full session ID across history, presence, and the send recipient. Keep search and read output as text.
