---
name: flopwire
description: A restrained control plane for attributable team memory.
colors:
  ref-ink: "#18201b"
  paper: "#f3f4ef"
  surface: "#ffffff"
  quiet-ink: "#667168"
  hairline: "#cbd1c9"
  healthy: "#176b45"
  healthy-soft: "#dbe9df"
  warning: "#9a5b0b"
  danger: "#a9362a"
typography:
  headline:
    fontFamily: "ui-sans-serif, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "clamp(1.625rem, 3vw, 2.5rem)"
    fontWeight: 650
    lineHeight: 1.08
    letterSpacing: "-0.025em"
  body:
    fontFamily: "ui-sans-serif, -apple-system, BlinkMacSystemFont, Segoe UI, sans-serif"
    fontSize: "0.9375rem"
    fontWeight: 400
    lineHeight: 1.55
  label:
    fontFamily: "ui-monospace, SFMono-Regular, Consolas, monospace"
    fontSize: "0.75rem"
    fontWeight: 550
    lineHeight: 1.3
    letterSpacing: "0.01em"
rounded:
  control: "6px"
  panel: "10px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "16px"
  lg: "24px"
  xl: "40px"
components:
  button-primary:
    backgroundColor: "{colors.ref-ink}"
    textColor: "{colors.surface}"
    rounded: "{rounded.control}"
    padding: "10px 14px"
  button-secondary:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ref-ink}"
    rounded: "{rounded.control}"
    padding: "9px 13px"
  input:
    backgroundColor: "{colors.surface}"
    textColor: "{colors.ref-ink}"
    rounded: "{rounded.control}"
    padding: "10px 12px"
---

# Design System: flopwire

## Overview

**Creative North Star: "The Ref Log"**

flopwire feels like an attributable operations record rather than a generic SaaS
dashboard. State flows in one direction, mutations remain legible as history,
and every health signal answers what changed, where, and who can act. The visual
language is quiet enough for sustained operational use under ordinary office
light.

The system refuses decorative data cards and terminal cosplay. Familiar
controls remain familiar. Character comes from the pipeline composition,
ref-like state labels, disciplined alignment, and the contrast between stable
paper surfaces and precise state color.

**Key Characteristics:**

- Health-first and causal, not metric-first.
- Flat, attributable, and operational.
- Restrained system typography with monospace reserved for identifiers and measurements.
- One visible ingest-to-index flow anchors the primary surface.

## Colors

Cool paper and green-black ink provide a low-glare working field. State colors
appear only where an operator must distinguish health, pressure, or failure.

### Primary

- **Ref Ink** (`#18201b`): primary text, selected navigation, and decisive actions.
- **Healthy** (`#176b45`): ready state and successful completion.

### Neutral

- **Paper** (`#f3f4ef`): application background.
- **Surface** (`#ffffff`): controls, tables, and focused operational regions.
- **Quiet Ink** (`#667168`): secondary text that still meets contrast requirements.
- **Hairline** (`#cbd1c9`): structural dividers and field borders.

**The State-Is-Meaning Rule.** Never use healthy, warning, or danger colors as
decoration. Each occurrence must describe current state or consequence.

## Typography

**Display Font:** operating-system sans serif
**Body Font:** operating-system sans serif
**Label/Mono Font:** operating-system monospace

**Character:** Native, fast, and unperformed. Prose reads like a runbook;
identifiers and measurements align like porcelain output.

### Hierarchy

- **Headline** (650, `clamp(1.625rem, 3vw, 2.5rem)`, 1.08): route and system state.
- **Title** (620, `1rem`, 1.3): section and action titles.
- **Body** (400, `0.9375rem`, 1.55): explanations and audit details, max `72ch`.
- **Label** (550, `0.75rem`, `0.01em`): identifiers, timestamps, measurements, and compact state.

**The Porcelain Rule.** Monospace marks machine-shaped data only. Navigation,
headings, descriptions, and buttons remain sans serif.

## Layout

The desktop shell uses a narrow persistent navigation rail and a fluid work
region capped at `1440px`. The health route begins with a horizontal relay from
collector to archive to index to retrieval. Each stage owns its current state,
queue depth, and last transition in place. Below it, dense operational lists
use aligned rows rather than card grids.

At widths below `760px`, navigation becomes a compact header and the pipeline
stacks vertically without changing stage order. Spacing follows 4, 8, 16, 24,
and 40-pixel steps.

## Elevation & Depth

The system is flat. Tonal changes and one-pixel hairlines establish grouping.
Shadows appear only for a temporary menu or dialog that must sit above active
work; routine panels never float.

**The Flat Record Rule.** Historical and operational content stays on the same
plane because depth must not imply that one record is less durable than another.

## Shapes

Controls use a restrained `6px` radius. Large operational regions use `10px`
only when they need a bounded background. Status chips may use a pill because
they are compact labels. Tables and timeline rows do not become rounded cards.

## Components

### Buttons

- **Shape:** compact control radius (`6px`).
- **Primary:** Ref Ink with white text and `10px 14px` padding.
- **Hover / Focus:** darken without movement; show a two-pixel healthy focus ring with offset.
- **Secondary:** white surface with a one-pixel hairline border.

### Chips

- **Style:** state-tinted background, dark state text, and no decorative icon.
- **State:** wording names the actual condition: Ready, Delayed, Paused, Failed.

### Cards / Containers

- **Corner Style:** `10px` only for large bounded work regions.
- **Background:** Surface on Paper.
- **Shadow Strategy:** none at rest.
- **Border:** one-pixel Hairline.
- **Internal Padding:** `16px` compact, `24px` standard.

### Inputs / Fields

- **Style:** white surface, hairline stroke, `6px` radius.
- **Focus:** Ref Ink border plus a two-pixel translucent Healthy ring.
- **Error / Disabled:** error copy states recovery; disabled fields retain legible text.

### Navigation

Navigation uses sans-serif labels and a quiet vertical rhythm. The active route
is Ref Ink on Healthy Soft with no colored side stripe. On small screens it
becomes a semantic header menu with the same route order.

### Pipeline Relay

The signature component shows Collector, Archive, Index, and Retrieval in
causal order. Threshold marks reveal queue pressure. State changes update in
place and the transfer log below explains each movement without animation being
required for comprehension.

## Do's and Don'ts

### Do:

- **Do** show health in causal order from collection through retrieval.
- **Do** attach actors, devices, and timestamps to every administrative mutation.
- **Do** use rows and timelines for repeated records.
- **Do** name the recovery action in every error state.

### Don't:

- **Don't** build the page from interchangeable metric cards.
- **Don't** use state color for decoration or brand emphasis.
- **Don't** use monospace as a costume for technical credibility.
- **Don't** hide destructive scope or consequences inside a generic confirmation.


## Shared brand mark

Use the approved centered ƒ/w mark from `landing/assets/brand-mark.svg` beside the existing lowercase wordmark. The admin console uses the mark at 32px high and imports the same asset as the homepage. Its favicon also uses the shared source. Vermilion on the brand mark identifies the product; operational colors retain their state meanings. Asset variants and usage rules are in `landing/assets/BRAND.md`.
