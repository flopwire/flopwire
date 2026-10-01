---
name: Flopwire focused refresh
description: Preserve the incumbent composition with the reconstructed fw identity.
colors:
  paper: "#faf9f6"
  ink: "#17191b"
  muted: "#60615f"
  line: "#cbc9c1"
  signal: "#df3717"
  signal-ink: "#bd2e13"
  soft: "#eeede8"
typography:
  display:
    fontFamily: "Plex, ui-monospace, monospace"
    fontSize: "clamp(42px, 3.9vw, 60px) at widths above 1100px"
    lineHeight: 1.1
    letterSpacing: "-.04em"
  body:
    fontFamily: "PlexSans, system-ui, sans-serif"
    fontSize: "17px"
    lineHeight: 1.7
  wordmark:
    fontFamily: "Plex, ui-monospace, monospace"
    fontWeight: 700
    letterSpacing: "-.035em"
rounded:
  route-label: "30px"
---

# Design System: Flopwire focused refresh

## Overview

Focused preserves the original landing composition, section order, stepped exchange, paper surface, and Plex Mono/Plex Sans pairing. The reconstructed orange fw ligature replaces the wire emblem in header and footer; the lowercase wordmark becomes bold. This remains one of three comparison prototypes, with no selected winner.

Shared asset limitations and completed review checks are recorded in [the comparison design document](../DESIGN.md). The mark is approximate and Plex Mono is not a confirmed reference-font match.

## Colors

Orange carries the mark, primary actions, connection lines, and focus rings. Its darker companion carries small orange text and hover states. Paper, ink, thin neutral rules, and soft code backgrounds retain the incumbent visual hierarchy.

## Typography

Plex Mono identifies the wordmark, headline, session screens, commands, and metadata. Plex Sans carries prose. Primary orange buttons use bold white type (19px). Inherited window chrome and some mobile metadata remain compact (11px); this is a density tradeoff in the preserved design.

## Layout

The page caps at 1440px with 56px desktop side padding. The hero splits copy and two offset session screens; the pill connector bridges the screens. Later sections alternate open prose and bordered evidence surfaces. Desktop story spacing is generous (130px at section starts).

Below 760px, the hero and provenance sections stack, side padding becomes 24px, and story spacing reduces. Installation commands scroll within their own container. Preserve the original stepped exchange rather than adopting either alternate hero.

## Elevation & Depth

Flat paper surfaces, borders, and soft code fills create hierarchy. Session screens have no drop shadow.

## Shapes

Most evidence panels and buttons are rectangular. The connecting annotation is a pill; avatars and window dots are circular. Header and footer marks preserve the 512:356 proportion.

## Components

Header and footer home links target `#top` and wrap decorative logo images in labeled links. Orange primary actions and ink outline actions have explicit hover and focus treatments. Skip navigation and visible source links support keyboard access. Source passages, discovery, search, and diffs retain their illustrative labels. The installation copy control includes status announcements and a selection fallback in code; browser checks passed mocked successful writes and command selection with a fallback announcement when clipboard writes reject. Successful OS clipboard writes remain unverified.

## Do's and Don'ts

- Do preserve the incumbent layout, product wording, installation commands, and privacy wording.
- Do keep synthetic workflow and retrieval examples labeled illustrative.
- Do retain the distinction between display orange and small-text orange.
- Don't describe the reconstructed mark or wordmark font as final brand assets.
