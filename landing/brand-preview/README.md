# Homepage brand previews

Compare three complete homepage directions before selecting the production design.

- `focused/` applies the fw identity to the existing composition.
- `bold/` uses an editorial headline and conversation spread.
- `explore/` shows the constraint and its source moving between sessions.

Serve the landing directory. Open `/brand-preview/`.

```sh
python3 -m http.server 8994 --bind 127.0.0.1 --directory landing
```

Use the direction buttons to switch the preview. Use Desktop or Mobile to change the iframe width. Use Open full page to inspect the complete page.

The supplied raster was reconstructed as `assets/fw.svg` and `assets/fw-black.svg`. These are approximate paths. The wordmark uses the bundled IBM Plex Mono. Replace these assets with the original outlined vectors before final brand approval.

Verification covered 1440px and 390px renders, a 1280px overflow check, local assets, internal anchors, setup commands, clipboard success with a mock, clipboard failure with selection fallback, and comparison controls. The independent finish review cleared all three as comparison prototypes. The focused version retains inherited 11px metadata; the exploratory version uses a denser monospace treatment.

No direction is selected for production. The existing landing page remains the source for product claims and setup commands.
