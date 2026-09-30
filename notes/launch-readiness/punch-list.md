# Launch readiness punch list

Status: open. Salvaged from the CASS-era launch gate (PR #2,
`origin/launch/readiness`, 3ec5124) with the CASS gates edited out. Retrieval
is now specified by `notes/local-search/README.md`; where this list and that
spec disagree, the spec wins. A production launch with real company traces is
not approved until every P0 is closed and every P1 is either closed or
explicitly removed from the MVP contract.

Items the B1 server PRs closed on the fresh schema are marked **(B1)**; see
`README.md` in this directory for the mapping.

## Confirmed implementation decisions

- Ship the full production MVP, with attachment scope narrowed to content
  embedded in the transcript files Flopwire's parsers read.
- Use queued deletion with an immediate tombstone, no undo window, idempotent
  object cleanup, retries, and durable request/terminal audit events.
- Use an online backup snapshot lease. Uploads continue; deletion purges wait.
- Require operator-declared encrypted backup destinations. Schedule backups
  externally. Restore only into empty Postgres and MinIO targets. RPO/RTO are
  operator-defined and measured rather than promised globally.
- Normalize Git project identity from `upstream`, then `origin`, then a
  deterministic remaining remote. Permit a local override. Leave repositories
  without a remote unassigned rather than guessing from folder names.
- Preserve both source-native message time (`messages.ts`) and server capture
  time (`generations.captured_at`); search time filters default to message
  time.
- Reserve a configurable share of quota for live appends so historical
  backfill pauses first (to be redesigned for chunk uploads in B2/B3).
- Rotate device credentials with immediate server-side replacement using a
  two-phase prepare/local-save/commit protocol so a lost response cannot strand
  the collector.
- Fail closed when a security-sensitive read or mutation cannot durably record
  its audit event.
- Retain every audit event indefinitely in V1, including searches, raw reads,
  administrative reads, failed authentication, and mutations. Measure and
  expose audit growth rather than silently pruning it.
- Retain full search text and complete result-ID lists in permanent search
  audit events. Raw-read events retain target identifiers and outcomes, not a
  duplicate copy of raw trace bytes. Document this privacy and growth cost.
- Require a TLS reverse proxy and enforce built-in token-bucket limits using a
  trusted client IP plus account/invite identifier.
- Remove network bootstrap. Create the first administrator only with a local
  server CLI that reads the password without argv or environment exposure,
  transactionally refuses after the first human admin, and audits its outcome.
- Publish tagged source only. CI must still prove macOS/Linux compilation and
  amd64/arm64 container builds.
- Officially support the bundled MinIO configuration in V1. Treat AWS and other
  S3-compatible services as unverified/best effort until provider tests exist.
- Do not require encryption for live Postgres or MinIO volumes in V1.
  Warn that host disks, Docker volumes, snapshots, and direct database access
  expose raw traces and full audit queries. Backup destinations remain subject
  to the separate encrypted-destination acknowledgement.
- Use a representative source-stratified local corpus during iteration, then a
  full local corpus import. Retain only aggregate timings, sizes, counts,
  and sanitized error classes.
- Measure performance first, then set the release SLO before the final full
  corpus/30-user gate.
- Do not add password recovery in V1. Document the canonical-user lockout and
  manual recovery risk explicitly.
- Treat the organization's administrator policy as the authorization boundary
  for collection. Do not add a member-side consent prompt. State the
  unredacted, team-visible, indefinite archive and permanent full-query audit
  boundary in public and administrator documentation.

## P0 — production launch blockers

### Truthful readiness and container boot (B1)

- Probe Postgres and object storage in readiness instead of returning hard-coded
  `ready` values.
- Separate liveness from readiness.
- Make the Compose health check call the readiness endpoint.
- Complete a production Docker image build and boot the exact Compose stack.

Acceptance evidence:

- Dependency failure tests for Postgres and object storage.
- A fresh `docker compose up --build` reaches ready and serves the console.
- A stopped dependency makes readiness fail with the correct cause.

### Safe, attributable deletion (B1)

- Replace the current database-first/object-second partial failure path.
- Preserve a durable deletion request or tombstone before destructive work.
- Make object cleanup retryable and idempotent.
- Return distinct not-found, accepted/in-progress, complete, and failed states.
- Audit the request and every terminal outcome even when cleanup fails.
- Ensure deleted conversations cannot appear in search while cleanup is
  pending, and cannot be resurrected by a re-upload.

Acceptance evidence:

- Hand-computed failure traces for every database/object ordering.
- Fault-injection tests for object-store timeout, partial object deletion,
  process crash, and retry.
- End-to-end deletion followed by a message-row rebuild and negative search
  (needs B3).

### Consistent backup and proven restore (B1)

- Define and enforce a write-quiescing or snapshot protocol across Postgres and
  raw objects.
- Verify that every chunk in the database snapshot has its exact object in
  the manifest.
- Make interrupted backup and restore states explicit.
- Restore into empty targets without mutating the active deployment.
- Record backup and restore outcomes, not only successful backup creation.

Acceptance evidence:

- Hand-computed upload/delete races during backup.
- Real backup while data exists, manifest verification, restore into fresh
  Postgres/MinIO, raw byte comparison, and (after B3) known-result search.
- Fault-injection coverage for interrupted dump, object copy, and restore.

### Real provenance and required filters

- Derive project identity from a normalized Git remote when available.
- Define behavior for no-remote, multiple-remotes, worktrees, and rewritten
  remotes.
- Capture the relevant Git commit without trusting trace-embedded identity.
- Add author, project, source, and time filters consistently to HTTP, CLI, MCP,
  and the local index (spec §7.3, §8).
- Preserve original source paths without using machine-specific paths as the
  cross-machine project identity.

Acceptance evidence:

- Cross-machine fixtures for SSH/HTTPS remotes that normalize to one project.
- Filter parity tests for server search, local search, and MCP.

### Quota state machine

- Reserve capacity for active/live session uploads before pausing historical
  backfill.
- Make `live`, `backfill`, `paused`, and `retryable` explicit protocol states.
- Enforce quota atomically under concurrent uploads.
- Persist and surface the collector's paused reason and next action.
- Do not report collection success when eligible files failed.

Acceptance evidence:

- Hand-computed concurrent upload and quota-boundary traces.
- Concurrency tests that cannot overshoot the configured limit.
- E2E proof that backfill pauses while an allowed live append still succeeds.

## P1 — MVP contract gaps

### Trace-owned attachments

- Preserve embedded Claude/Codex images and attachment events because they are
  already bytes inside the raw indexed traces.
- Add sanitized embedded-media fixtures and prove byte-for-byte round trips.
- Do not discover or chase sidecar paths, arbitrary workspace paths, symlinks,
  URLs, or traversal paths in V1.
- Update the MVP contract so attachments mean only content the parsers
  extract from transcript files (spec §4.2 skips base64 and images).

### Credential lifecycle (B1)

- Add explicit device-token rotation without losing device-agent state.
- Preserve a recoverable login/enrollment path after revocation.
- Audit issuance, rotation, revocation, and failed use.
- Add token redaction tests for logs, diagnostics, and errors.

### Complete audit semantics (B1; search and raw-read events return with B3)

- Audit searches, raw reads, policy/status/member/device/audit-log reads, failed
  authentication, and all mutations.
- Preserve full search text and complete result-ID lists for forensic replay;
  preserve target IDs and outcomes for raw reads.
- Retain all audit events indefinitely and measure their storage growth.
- Fail closed for security-sensitive operations when audit persistence fails.

### Transactional identity workflows (B1)

- Replace the public first-caller bootstrap endpoint with a concurrency-safe
  local server CLI command.
- Make invite claim atomic across user creation, invite claim, credential
  issuance, and audit.
- Make device/service enrollment atomic across identity, device, credential,
  and audit creation.
- Add retry and concurrent-claim tests.

### Member offboarding

- Define whether offboarding disables all credentials while retaining
  attributed traces, queues a member-wide purge, or remains a manual sequence
  of device revocations.
- Make the selected workflow atomic and auditable. The decision is currently
  unresolved; do not claim complete member lifecycle support.

### Abuse resistance and deployment boundary (B1; endpoint concurrency limits return with B3)

- Add login/bootstrap/invite-claim rate limits.
- Add request-body and concurrency limits to expensive endpoints.
- Document required TLS/reverse-proxy headers and trusted-proxy behavior.
- Add baseline security headers to the console.

## P2 — release confidence

- Test every wave-1 harness source shape (Claude Code, Codex, Devin).
- Run macOS and Linux device-agent integration tests.
- Run a 30-user mixed workload: active append, backfill, search, audit, delete,
  backup, and message-row rebuild.
- Measure database, object, and message-row growth.
- Test device-agent restart, server restart, network loss, duplicate delivery,
  file truncation, and rewrite.
- Publish versioned checksums and installation artifacts for macOS and Linux.
- Run accessibility automation plus keyboard and screen-reader spot checks.
- Complete a clean-machine installation from the public README.

## Evidence discovered during planning

- Docker Desktop's configured credential helper hangs in this environment.
  Anonymous temporary Docker configuration (`DOCKER_CONFIG=$(mktemp -d)`)
  resolves the exact base-image tags.
- Purging `tar` from the runtime image breaks `dpkg`. A Trixie runtime with
  corrected package cleanup built and passed runtime smoke checks for arm64 and
  amd64.
- Gemini stores mutable whole JSON documents. Offset-only append collection can
  corrupt larger rewrites and misses same-size rewrites (spec §5.3 handles
  these as new generations).
- Observed Claude and Codex image attachments are embedded base64/data-URI
  structures inside trace files and are already preserved by raw chunk upload.

## Launch rule

Do not label a release production-ready while a P0 is open. If a P1 feature is
deferred, update `notes/mvp_scope.md`, `PRODUCT.md`, README claims, and tests in
the same review so the shipped contract remains truthful.
