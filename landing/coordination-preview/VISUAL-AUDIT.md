# Homepage spacing audit

Audited both complete directions, including the review page. Preserved the existing typography, colors, logo, copy, section order, and tool output.

The reading order is: product pitch, agent exchange, device progression, retrieval, source context, architecture, permissions, sharing rules, measurements, setup. This remains the same on small screens. The issue was spacing and column behavior across that sequence.

## Findings and corrections

| Priority | Area | Evidence before | Correction |
| --- | --- | --- | --- |
| P1 | Connected sessions hero | The example caption immediately followed the hero grid. The grid aligned its title to the bottom of the longer copy column. | Align the columns at the top. Add 48px between hero and example, 40px on mobile. |
| P1 | Device progression | Terminal tops varied with paragraph, note, and command lengths. Three columns persisted at tablet widths. | Align corresponding rows with a CSS subgrid. Stack below 1000px. |
| P1 | Section introductions | Editorial permissions and benchmark content immediately followed the introduction. Other sections had 30px or 36px gaps. | Use 32px between introductions and content, 24px on mobile. |
| P1 | Architecture on mobile | The title and description could touch after the heading layout collapsed. | Use an explicit single-column grid with a 16px gap. |
| P2 | Page rhythm | Section intervals mixed 100px, 68px, and 66px. Colored sections used a separate spacing rule. | Use 80px section intervals, 64px at tablet widths, and 56px on mobile. Apply the same interval before the architecture panel. |
| P2 | Source context | Diff padding and margin accumulated differently between the directions. | Remove the nested diff margin. Keep 24px between mobile diff and source context. |
| P2 | Terminal legibility | Mobile command/output text was 11px. | Increase to 12px with 1.75 line height. Retain the complete captured output. |
| P2 | Setup | Instruction panel inherited default preformatted-text margins. Split columns stayed narrow at tablet widths. | Reset the margin. Stack below 1000px. Keep the copy target at least 44px tall. |
| P2 | Review page | The direction description still described an obsolete side-by-side example. | Describe the current sequence. |

## Scope review

No sections, controls, or repeated information were added. Existing examples already cover the product use cases. The correction uses `layout.css` to define page rhythm across both visual directions. `examples.css` continues to distinguish terminal output, diffs, and agent instructions.

## Verification

Reviewed complete before/after screenshots at 1440px, 768px, and 390px. Checked both directions at 320px, 390px, 768px, 1024px, and 1440px for page overflow, section presence, desktop terminal alignment, and mobile architecture spacing. All nine main sections remain present. The benchmark table scrolls within its own region.

Checked images, internal anchors, clipboard success/reset/fallback, and review-page direction and viewport controls. The pages are static and need no build step.

The layout detector flags left padding beside horizontal separators in the stacked device stages. These are full-width section rules, not enclosing cards; text remains aligned with the page gutter. This is an intentional result. The earlier full detector's palette, display-leading, and CLI/table dash findings are unchanged.

Screenshots were generated outside the repository to avoid committing large image artifacts. Runtime messaging behavior and comparative performance were not re-tested in this visual audit.
