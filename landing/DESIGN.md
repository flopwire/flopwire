# Flopwire landing page

Mode: Persuade. The v0.1 launch story leads with teammates using different harnesses and machines. A single pagination change connects the examples.

## Reading order

1. Live exchange: Alex sends the API constraint; Sam changes the client code. A disclosure shows discovery through the repo and recent sessions.
2. Grep: compact results from both teammates, then an address-based read of the surrounding conversation. CLI is visible; equivalent MCP calls are optional.
3. Commit provenance: a diff opens the discussion explaining why the cursor must stay intact.
4. Environments: local indexing, team retrieval, supported transcript sources.
5. Setup: open source, collection controls, installation, hosted coming soon, benchmark method.

Use whitespace between sections. Borders identify terminal windows and code surfaces, not every paragraph or page section. Avoid repeated explanations and status labels.

## Typography and color

IBM Plex Sans for explanatory text and section headings. IBM Plex Mono for the wordmark, hero title, commands, tool responses, session metadata, and measurements. Both are self-hosted under the bundled OFL license.

Paper #faf9f6; ink #17191b; muted #60615f; vermilion #d52e19. Restrained warm highlights mark search matches. Diff additions use #246448; removals #785a53. Prose is 18–20px in primary explanations; desktop terminal text 12–14px. Smaller mobile metadata remains at least 11px.

## Behavior

The hero is static: two stepped screen frames connected by a vermilion wire and a rounded Live update label. No autoplay, packet animation, replay, or pause controls. Search highlights animate once on entry; clicking a result reveals a read call and the surrounding passage. Commit discussion is a keyboard-accessible disclosure; the hero source link reveals and focuses it. Reduced motion disables automatic animations. No scroll locking or scroll scrubbing.

At 760px, sections stack. Code wraps where useful; installation commands scroll inside their container. Native details/summary elements hold supporting material. Focus rings, skip link, status announcements, and clipboard selection fallback remain.

## Truth boundaries

Examples are synthetic. Retrieval commands follow the current `flopwire` CLI/MCP schema. Messaging and commit-to-discussion are v0.1 release requirements; the page is a launch preview. Compatibility does not claim every harness has the same messaging support. Do not show performance numbers until measured.

## Full story revision

All primary sections share the page container edges. Constrain prose line length inside the container rather than centering narrower section wrappers. Terminal examples and provenance share a consistent full section width. Session discovery now has a visible section rather than a hero disclosure. Local-first adoption and explicit allow/local/deny policy examples complete the setup story. Performance comparisons remain pending measurement; no numbers are invented.

## Linear story and layout references

No accordions, collapsed passages, or click-to-reveal explanations. Grep results and their read passage are visible together; commit discussion is visible alongside the diff. Links navigate, and the only button copies setup commands. The hero remains static.

Maintain common outer page edges while varying the evidence inside them:

- Discovery: narrow prose beside a session directory.
- Retrieval: shared query above matches and the selected source passage.
- Provenance: unframed source quotation alongside a compact diff.
- Environments: a compact laptop-to-server diagram.
- Sharing: visible policy rules and outcomes.
- Setup: concise invitation beside the actual source-build commands.

Reference study, 2026-09-30:

- https://ledge.sh/ — focused feature demos, asymmetrical columns, visible explanations. Adopt the chapter composition; omit dense feature lists and autonomous typing loops.
- https://linear.app/ — consistent introductions, different evidence formats per feature. Use directory, retrieval, diff, and topology instead of a repeated full-width terminal.
- https://zed.dev/ — varied scale and feature grouping. Do not borrow feature accordions.
- https://tailscale.com/ — labeled topology and concrete setup cues. Keep Flopwire’s own visual identity.
- https://www.cloudflare.com/developer-platform/ — architectural explanation alongside concise text. Omit tabbed content and enterprise marketing filler.

The study used three Sol subagents with live site inspection. Motion observations were limited to directly observed state changes, not inferred timings.

## Lower-page rhythm

Environment uses a centered heading and laptop-to-server band. Sharing uses a heading row above three comparable policy columns. Setup retains the split CTA/terminal composition. This removes the repeated run of left-heading/right-evidence sections without changing the shared outer container or hiding content.

Read-only Claude consultation (`20260930-172149-claude-bda5771a`, default model/effort) confirmed the repeated grid diagnosis and recommended topology band → policy ledger → setup split. Adopted that composition; used a centered environment heading and a soft policy surface rather than the suggested extra separator rules, respecting the user's earlier request. Mobile follows source order and stacks policies.
