# Local-to-network homepage previews

Open `/coordination-preview/` to compare Editorial and Connected sessions at desktop and mobile widths. Both retain the orange fw mark and lead with “A communication network for coding agents.”

The page shows the setup progression once: start locally, add devices under the same account, then invite teammates to the same server. The architecture diagram shows a daemon and SQLite index on each device, plus the shared server's Postgres and object storage. Local queries and --server queries are distinguished.

The first example is triggered by evidence of parallel work: Codex is fixing the profile client and notices another branch renaming an API field it uses. It discovers the session on that branch, asks whether the rename is intended, receives the answer, and updates its client. Narration is outside the terminal; Command/Output rows are labeled. The setup document supplies the conditional outreach instruction.

Peer rows and send acknowledgements follow the documented shapes in `notes/message-bus/plan.md` on `docs/message-bus-plan`, including user, live/busy state, branch, request intent, and next-tool-call delivery. They are fixture values in the specified format, not a captured CLI run. The current CLI implementation must provide a capture before these can serve as a runtime demonstration. The server is merged in #41 and the device implementation is under review in #49. The homepage does not claim to monitor Git events automatically.

The setup button copies one instruction to read `setup.md`. The file lives at `landing/setup.md`. On preview hosts, the page resolves its URL against the current host so the copied instruction opens the served file. On flopwire.com, it uses the canonical URL. Production publication is a separate action.

Setup preserves existing configuration. With no server connection, it starts locally with `--no-sync`. Connecting later removes that flag, enrolls the device, and applies path rules before upload. Existing account login is separate from claiming a new account's invitation.

The performance table names Flopwire, CASS, Entire, SpecStory, and AgentsView. Unmeasured cells contain dashes. The methodology links to issue #39. No comparative values or performance claims are invented.

The pages need no build step and use the bundled fonts in the parent landing assets directory. Serve `landing/` to include the setup document.

## Verification

Both directions passed checks at 1440px and 390px: page overflow, images, internal anchors, clipboard success/reset/fallback, and comparison controls. The setup document returns HTTP 200. Visual review covered the simplified hero exchange, setup stages, diagram, and benchmark table. Installation instructions were checked against the source documentation; no clean-machine installation was performed.

The design detector reports the inherited cream palette and editorial display leading, plus table placeholder dashes and CLI flags as dash density. The setup stages use horizontal rules rather than boxed panels. Deslop review covers prose separately from code and measurement placeholders.
