---
name: Flopwire brand comparison
description: Three parallel homepage prototypes for review; no direction selected.
colors:
  paper: "#faf9f6"
  ink: "#17191b"
  orange: "#df3717"
  border: "#c9c8bf"
  preview-backdrop: "#e9e7df"
typography:
  display:
    fontFamily: "Plex, monospace"
    fontSize: "clamp(38px, 5vw, 70px)"
    fontWeight: 700
    lineHeight: 1.08
    letterSpacing: "-.035em"
  body:
    fontFamily: "Sans, Arial, sans-serif"
    fontSize: "16px"
    lineHeight: 1.6
rounded:
  control: "4px"
---

# Design System: Flopwire brand comparison

## Overview

This hub presents three finished comparison prototypes: [Focused refresh](focused/DESIGN.md), [Editorial redesign](bold/DESIGN.md), and [Connected sessions](explore/DESIGN.md). Each keeps the product story while testing a different composition. None is the chosen production direction.

The binding reference is the supplied raster: an orange fw ligature with a black lowercase monospace wordmark. Shared `assets/fw.svg` and `assets/fw-black.svg` are manually reconstructed approximations. Curves and color remain approximate. The bundled Plex Mono wordmark is not a confirmed match for the reference font; obtain original vectors and font information before treating the identity as final.

Settled desktop (1440px) and mobile (390px) screenshots for all three directions were inspected. Independent finish review accepted all three as comparison prototypes with no required fixes. Browser checks passed image loading, one h1 per page, internal anchors, and mocked successful clipboard writes. The pages also passed document overflow checks at 1280px. Clipboard rejection also passed: all three variants selected the installation commands and announced the fallback status. Successful OS clipboard writes were not verified. Hub direction selection, pressed states, desktop/mobile frame sizing, and overflow checks passed.

## Colors

Paper and ink frame the comparison. Orange identifies the shared mark and interaction focus. Each variant owns its palette; the hub does not normalize those differences.

## Typography

The hub pairs bundled Plex Mono for the heading and bold lowercase wordmark with bundled Plex Sans for explanatory text and controls. These are self-hosted font files, not a verified reference-font match.

## Layout

The hub centers within a 1584px container with 48px side padding. Below 700px, padding becomes 20px and the footer stacks. Direction and viewport controls wrap as needed.

The iframe uses a real 1440px desktop or 390px mobile layout width, with a 900px frame height. It scales down to fit the surrounding shell, which clips to the scaled frame. Open full page provides the complete unscaled page. Direction selection updates the query string; the initial mobile viewport follows the hub's narrow-screen breakpoint.

## Elevation & Depth

The hub is flat. A border and muted backdrop distinguish the preview from the review controls; there are no shadows.

## Shapes

Controls have small rounded corners. The preview frame is rectangular. Logo images preserve the source's 512:356 proportion, including small-size samples at 16, 24, and 32px wide.

## Components

Direction buttons and Desktop/Mobile buttons expose selection through `aria-pressed`. Selected controls use ink fill and paper text. Hover uses an orange border; keyboard focus uses an orange outline. The frame title, description, and full-page link follow the selected direction. SVG download delivers the approximate orange reconstruction.

A separate static copy is served through the local Tailscale preview setup for review; its HTTPS URL was fetched successfully. This does not select or replace the production homepage.

## Do's and Don'ts

- Do keep all three directions available for parallel review.
- Do preserve each direction's own layout and palette when comparing.
- Do retain the approximate-asset notice until original brand assets arrive.
- Don't infer a winner from the hub's default Focused selection.
- Don't treat illustrative sessions as real product activity or add privacy claims from visual documentation.
