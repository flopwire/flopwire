# Flopwire identity

The approved mark is concept D: one italic ƒ with the middle peak of a w-shaped crossbar aligned to its stem. Preserve the two open troughs, rounded terminals, and forward lean.

## Assets

- `brand-mark.svg`: primary vermilion mark, #d52e19.
- `brand-mark-ink.svg`: ink mark, #17191b, for monochrome use.
- `brand-mark-reverse.svg`: paper mark, #faf9f6, for dark backgrounds.
- `favicon.svg`: vermilion mark on a square paper field.

These files contain vector paths. The admin console imports the primary mark and favicon from this directory so both builds use the same assets.

## Placement

Pair the primary mark with the lowercase black wordmark. Keep IBM Plex Mono at weight 500 on the homepage. The admin console retains its existing system-font wordmark.

Use the mark at 40px high in the desktop homepage header, 34px on mobile, and 32px in the footer and admin console. Keep its aspect ratio. Allow at least one stem width of clear space around it. Use the square favicon asset for isolated icons at 16px and above.

Use a single flat color. Do not stretch, outline, rotate, or add shadows to the mark. Preserve the centered crossbar. Keep the existing thin connection lines in diagrams; they describe relationships rather than repeat the logo.

## Homepage language

Keep warm paper, black ink, and vermilion accents. Keep Plex Mono for the wordmark, hero, and machine output, with Plex Sans for explanations. The curved mark sits beside the squared demo windows and controls. Avoid spreading waves into separators or making every control rounded.

## Origin

Vector contour traced from the approved image-tool concept D in [logo exploration PR #88](https://github.com/flopwire/flopwire/pull/88). Potrace 1.16 used a 50% monochrome threshold, a 12-pixel speck threshold, and 0.4 curve optimization tolerance. The contour is preserved; the viewBox removes excess image margins. The favicon adds a paper field and square padding.

The exact generation prompts and source raster remain in the exploration PR. Treat these SVGs as the production assets. Inspect the master, color variants, and favicon together after any future geometry change.
