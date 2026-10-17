# Four-section homepage preview

Serve `landing` and open `/coordination-preview/four-sections/`.

Uses the existing homepage stylesheet, fonts, logo, controls and example components. Production homepage files are unchanged. The four sections introduce talk, search, trace, and cross-device/team use. Section 04 combines device/team reach with architecture. Supporting sections cover privacy and the qualitative comparison from PR #308.

Examples are synthetic, not live agents. CLI syntax and data boundaries were checked against README.md, docs/search.md, docs/messaging.md and SECURITY.md at 8a725a5. Competitor docs and code were rechecked October 17: AgentsView 1440d77, Agent Mail Rust e069358, Entire 9c3b86e, and SpecStory f5e1c72. Source links and scope differences remain in the [pre-cut revision](https://github.com/flopwire/flopwire/blob/a51bc83/landing/coordination-preview/four-sections/index.html) and PR review record; the owner requested their removal from the page. Ratings cover supported public interfaces; no comparative runtime measurements were performed. Earlier research remains in PR #308.

Agent marks are bundled from Lobe Icons with their MIT license. Inline code uses a tinted background. All examples and notes are visible; there are no disclosure controls.

The hero introduces cross-harness reach; section 01 shows the only conversation as chat bubbles joined by a static Flopwire connector. The entire setup instruction is a native copy button with visible success feedback and a manual-copy fallback. All seven sections are numbered. Terminal examples share prompt, command and output styles; architecture uses the page background and prose typography. Section 03 uses `trace` from merged PR #333 (captured with candidate 5838acd). Its JSON fields were captured from the real CLI against an isolated synthetic Claude session and a real demo Git commit; the displayed `jq` projection is the command used. The read excerpt is copied from actual CLI output, with scope/session headers and surrounding messages omitted. No personal transcript data is used.

Section 02 shows actual `search "cursor timestamp"` output from the same isolated synthetic session, omitting the scope/session headers and self-exclusion footer. The extra command blocks in 01/02 are removed. Approved copy cuts shorten 02/03, combine sender acceptance, and move MCP into the 04 command table.
