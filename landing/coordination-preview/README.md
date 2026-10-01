# Local-to-network homepage previews

Open `/coordination-preview/` to compare Editorial and Connected sessions at desktop and mobile widths. Both retain the orange fw mark and lead with “A communication network for coding agents.”

The page shows the setup progression once: start locally, add devices under the same account, then invite teammates to the same server. The architecture diagram shows a daemon and SQLite index on each device, plus the shared server's Postgres and object storage. Local queries and --server queries are distinguished.

All command examples share `examples.css`, including the hero, device progression, and search/read flow. Code changes use one diff treatment. Agent setup and configuration rules have explicit labels.

The first example starts from a concrete commit on another branch. Codex queries branch history as JSON, matches the commit in the session digest, checks that same session ID in live presence, and asks about the API contract. The presence check is expandable so the main story stays readable. Narration is outside the terminal; commands and output are labeled.

The branch-history result is captured from the built retrieval CLI against `fixtures/api-change.jsonl`, then projected with the exact `jq` command shown. The commit ID was extracted from a successful git commit tool result. `fixtures/branch-session.json` contains the projected output. The stored title comes from the transcript's first prompt.

All agent-facing commands return JSON by default in the chosen CLI contract. The project direction is recorded in the root README. The API and device protocol already use JSON; the messaging CLI still needs implementation and capture. The presence projection uses the actual API field names. The send receipt projects the existing structured SendResponse fields. Implementation and a real exchange capture are tracked in [#55](https://github.com/flopwire/flopwire/issues/55). The server and device bus are merged in #41 and #49. The page does not claim to monitor Git events automatically.

The grep comparison shows captured system grep and Flopwire output side by side, using shared `-n -F` flags. The retrieval side requests `--json` and projects named fields with `jq`. The file fixture is `fixtures/src/pagination.ts`; transcript search uses `fixtures/pagination.jsonl`. The read result appears below the comparison. The source-context section shows a compact excerpt attributed to the actual agent, branch, and message address. The setup document links to `agent-tools.md`, which explains title provenance, commit attribution, retrieval addresses, and JSON parsing.

The setup button copies one instruction to read `setup.md`. The file lives at `landing/setup.md`. On preview hosts, the page resolves its URL against the current host so the copied instruction opens the served file. On flopwire.com, it uses the canonical URL. Production publication is a separate action.

Setup preserves existing configuration. With no server connection, it starts locally with `--no-sync`. Connecting later removes that flag, enrolls the device, and applies path rules before upload. Existing account login is separate from claiming a new account's invitation.

The performance table names Flopwire, CASS, Entire, SpecStory, and AgentsView. Unmeasured cells contain dashes. The methodology links to issue #39. No comparative values or performance claims are invented.

The pages need no build step and use the bundled fonts in the parent landing assets directory. Serve `landing/` to include the setup document.

## Verification

Both directions passed checks at 1440px and 390px: page overflow, images, internal anchors, clipboard success/reset/fallback, and comparison controls. The setup document returns HTTP 200. Visual review covered the simplified hero exchange, setup stages, diagram, and benchmark table. Installation instructions were checked against the source documentation; no clean-machine installation was performed.

The design detector reports the inherited cream palette and editorial display leading, plus table placeholder dashes and CLI flags as dash density. The setup stages use horizontal rules rather than boxed panels. Deslop review covers prose separately from code and measurement placeholders.

The complete spacing audit is in [VISUAL-AUDIT.md](VISUAL-AUDIT.md). Shared page intervals and responsive column behavior are defined in `layout.css`. The audit includes both full pages and checks from 320px to 1440px.
