---
name: Flopwire editorial redesign
description: An editorial comparison prototype built around a large conversation spread.
colors:
  paper: "#f4f0e6"
  ink: "#161616"
  muted: "#5e5b54"
  orange: "#ed6038"
  rule: "#c9c3b6"
  evidence-surface: "#e9e3d7"
typography:
  display:
    fontFamily: "Plex Mono, monospace"
    fontSize: "clamp(38px, 6.3vw, 96px)"
    fontWeight: 700
    lineHeight: 1.09
    letterSpacing: "-.04em"
  body:
    fontFamily: "Plex Sans, sans-serif"
    fontSize: "18px"
    lineHeight: 1.55
---

# Design System: Flopwire editorial redesign

## Overview

Bold replaces the incumbent composition with an editorial rhythm: oversized type, a full-width black conversation spread, a large source quotation, and an orange compatibility spread. Alex's API constraint and Sam's response remain in reading order, with identity in a narrow desktop margin. This is a comparison prototype, not a selected production redesign.

The orange fw reconstruction anchors masthead and footer beside a bold lowercase Plex Mono wordmark. Shared asset limitations and completed review checks are recorded in [the comparison design document](../DESIGN.md).

## Colors

Warm cream paper and black ink carry the page. The compatibility section uses a broad orange fill; lighter orange links identify sources inside the dark conversation. Muted rules and warm evidence fills separate dense tool content.

## Typography

Bundled Plex Mono carries the headline, wordmark, section titles, conversation, quotes, commands, and labels. Bundled Plex Sans carries body explanations. The desktop display reaches 96px; the mobile headline uses a smaller fluid scale. The contrast between large conversation type and compact source metadata is deliberate.

## Layout

The page caps at 1600px with 5vw side padding. The headline spans the hero above introductory copy and actions. The conversation is one wide black spread rather than stepped windows. Discovery reads as a directory; retrieval pairs matching lines with their source; provenance puts a quotation beside the diff; sharing uses a policy ledger.

At 700px, identities, directory, retrieval panes, quotation, policy rows, and setup stack. Side padding becomes 22px. Code wraps except installation commands, which scroll within their own surface.

## Elevation & Depth

Large contrasting fills, thin rules, and typography create hierarchy. There are no shadows or entrance animations.

## Shapes

Rectangular spreads, evidence panes, and buttons dominate. The fw image retains its 512:356 proportion. The composition relies on type and space rather than rounded cards.

## Components

Primary actions use black fill and cream text; outline actions invert on hover. Keyboard focus uses a distinct dark orange outline, and a skip link reaches the content. The installation terminal is black with a bordered copy control. Copy includes a status announcement and selection fallback in code; browser checks passed mocked successful writes and command selection with a fallback announcement when clipboard writes reject. Successful OS clipboard writes remain unverified. Pagination, search, and diff examples retain their illustrative or synthetic labels.

## Do's and Don'ts

- Do preserve the broad conversation spread and editorial type hierarchy when extending this direction.
- Do retain installation commands, link destinations, and the existing privacy wording.
- Do keep all example evidence visibly illustrative.
- Don't introduce stepped hero windows or treat this comparison as the production choice.
- Don't describe the reconstructed identity as an exact original vector or confirmed font match.
