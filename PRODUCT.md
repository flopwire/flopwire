# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Engineering teams of up to 30 people that use multiple coding agents and need
prior work to remain searchable across people, machines, and tools. Operators
also need to enroll members, enforce collection policy, audit access, and
recover the deployment without reading or repairing search internals.

## Product Purpose

flopwire turns local coding-agent traces into organization-wide, agent-readable
memory. It indexes each device's traces locally within about a second,
continuously archives eligible traces to the team server, preserves exact
provenance, and exposes local and team full-text search through a CLI and an
MCP server.

## Positioning

The durable raw archive is the product boundary. Message rows and search
indexes are derived and rebuildable from it. This separates team identity, sync, policy, and
operations from source-specific indexing internals.

## Operating Context

Members run the device agent on macOS or Linux. A self-hosted Linux deployment runs
the API, Postgres, S3-compatible storage, and an admin console.
Members search through the CLI or MCP. Administrators use the console and CLI
for invites, devices, policy, audit, deletion, quotas, backups, and health.

## Capabilities and Constraints

- One deployment represents one organization.
- Claude Code, Codex and Devin transcripts are read on the device, chunked,
  and parsed by Flopwire's own parsers on the device and on the server.
- Path rules (admin floors plus member rules) decide what is indexed and
  what leaves a device. Nothing is redacted.
- Raw traces are organization-visible, unredacted, and retained indefinitely.
- Admin and member are the human roles; service accounts are upload-only by
  default.
- Canonical authorship comes from the enrolled uploader, never embedded trace
  identity.
- The release target is production-ready OSS for 30 users, macOS and Linux
  clients, and single-node Docker Compose.
- Search is full-text over extracted messages and returns hits with provenance.
- The admin web surface does not provide corpus search in V1.

## Brand Commitments

The product name and CLI command are `flopwire`. The voice is concrete,
operational, and candid about security tradeoffs.

## Evidence on Hand

The product begins from the decision records in `notes/punch-list.md` and
`notes/local-search/README.md` (index: `notes/README.md`).
No customer logos, testimonials, benchmarks, or commercial claims exist and
must not be fabricated.

## Product Principles

1. Preserve evidence before deriving knowledge.
2. Make every result attributable and inspectable.
3. Keep indexes replaceable and recovery deterministic.
4. Prefer simple self-hosting over distributed machinery.
5. State privacy and operator trust boundaries plainly.

## Accessibility & Inclusion

The admin console must meet WCAG 2.2 AA for keyboard access, contrast, focus,
semantics, error identification, and reduced-motion preferences.

