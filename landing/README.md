# Flopwire landing page

Static launch preview. Serve this directory with any static HTTP server.

```sh
python3 -m http.server 8993 --bind 127.0.0.1 --directory landing
```

No build step or third-party runtime dependencies. Fonts are bundled with their OFL license.

## Demonstrations

The exchange and search results are illustrative client-side examples. They do not connect to a live agent service. The hero uses a static connector with no replay controls. Source conversations, commit reasoning, and setup are visible in the page. No accordion or reveal controls. Copy falls back to selecting the setup commands when clipboard access is unavailable.

## Before public release

- Make the repository publicly accessible or change the availability claim and GitHub destination. The repository is currently private.
- Replace development-branch setup instructions with a verified release installation path.
- Confirm the final Flopwire command name and tool schemas. The page uses existing `flopwire` setup commands and labels proposed tool calls as illustrative.
- Verify messaging, commit-to-session links, supported harnesses, and environment support against the release. Delivery probes are not a released messaging service.
- Confirm the hosted offering announcement.

## Verification

Story revision checked at 1440px desktop and 390px mobile: no document overflow, static hero geometry, reduced motion, source anchor navigation and visible source passages, and clipboard fallback. The compatibility table scrolls horizontally on narrow screens. No live backend integration or clean-machine installation test was performed.

## Story revision

The story uses one pagination change across live coordination, grep/read, and commit provenance. Examples are synthetic and use the existing `flopwire` command name. CLI and MCP retrieval shapes were checked against `docs/search.md` and `cmd/flopwire/mcp.go` on main.

Performance lives behind a link to the benchmark method until results exist. Before adding comparisons, measure the same corpus and tasks across transcript grep, full-session reading, and alternative retrieval. Record retrieval success, total context returned (including follow-up reads), latency percentiles, corpus size, hardware, and cache conditions. Do not present acceptance thresholds as measured results.

The private preview targets v0.1 messaging and commit provenance. Validate both end-to-end before public launch. The installed code and available adapters determine compatibility claims.


The linear revision removes the benchmark link from the marketing page until there are measured results worth presenting. Benchmark plans above remain the implementation follow-up.
