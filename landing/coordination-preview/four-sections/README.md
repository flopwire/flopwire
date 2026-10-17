# Four-section homepage preview

Serve `landing` and open `/coordination-preview/four-sections/`.

Uses the existing homepage stylesheet, fonts, logo, controls and example components. Production homepage files are unchanged. The four sections introduce talk, search, trace, and cross-device/team use. Section 04 combines device/team reach with architecture. Supporting sections cover privacy and the qualitative comparison from PR #308.

Examples are synthetic, not live agents. CLI syntax and data boundaries were checked against README.md, docs/search.md, docs/messaging.md and SECURITY.md at 8a725a5. Competitor docs and code were rechecked October 17: AgentsView 1440d77, Agent Mail Rust e069358, Entire 9c3b86e, and SpecStory f5e1c72. Source links and scope differences remain in the [pre-cut revision](https://github.com/flopwire/flopwire/blob/a51bc83/landing/coordination-preview/four-sections/index.html) and PR review record; the owner requested their removal from the page. Ratings cover supported public interfaces; no comparative runtime measurements were performed. Earlier research remains in PR #308.

Agent marks are bundled from Lobe Icons with their MIT license. Inline code uses a tinted background. All examples and notes are visible; there are no disclosure controls.

The hero introduces cross-harness reach; section 01 shows the only conversation as chat bubbles joined by a static Flopwire connector. Setup uses an embedded copy control with visible success feedback and a manual-copy fallback. Section 03 uses `trace` from PR #333 (candidate 5838acd). Its JSON fields were captured from the real CLI against an isolated synthetic Claude session and a real demo Git commit; the displayed `jq` projection is the command used. The read excerpt is copied from actual CLI output, with scope/session headers and surrounding messages omitted. No personal transcript data is used.
