# History qualification before CASS retirement

Status: incomplete, October 8, 2026. Collection and coverage reporting are
deployed. This record does not authorize stopping CASS, removing integrations,
changing exclusions, or deleting archives and recovery keys.

## Shipped behavior

Both Macs collect ordinary native coding history, Claude Desktop Local Code,
and Cowork. Ordinary Claude chat and remote SSH/WSL collection are outside the
selected scope. Cowork inherits the strictest rule anywhere within its selected
Mac folders. Unknown mapping holds sharing. Previously captured unknown mapping
remains restrictive even when current folders become known.

Collection, captured uploads, server parsing, and policy holds have separate
[coverage observations](../coverage-reporting.md). Unknown diagnostics are not
zero counts. An empty upload or parse queue does not prove discovery or exact
message equality.

## Observed evidence and limits

| Evidence | Result | What it does not prove |
| --- | --- | --- |
| October 7 retained-capture comparison across both Macs | 27,797 generations matched server sizes and manifest counts; no unexplained missing or partial acknowledgment | Raw object checksums, exhaustive discovery, or every message's semantics |
| October 7 recent retrieval sample | Twelve local-Mac sessions and one second-Mac session matched shared retrieval message counts | Equality outside the sample or equality of message text and order |
| October 7 current Desktop inventory | Twenty-two Local Code parent files and 75 Cowork native files indexed to observed sizes | App creation, append, or restart behavior on both Macs |
| October 7 missing older Code evidence | Twenty older metadata links had absent native files | Why the files are absent or whether retained archives can recover them |
| October 7 former one-time captures | 75 Cowork files have ongoing collection; 67 audit streams deliberately excluded; one older native transcript outside configured roots | Ongoing discovery for the outside-root transcript |
| October 8 live checks | Both collectors connected; no pending captured bytes or reported loss/truncation; both authenticated parse snapshots settled | Exhaustive history completeness or a single atomic cross-device snapshot |
| Cowork policy observations | Historical mapping holds remain deliberate; second Mac reported 44 held sessions | Permission to upload held evidence or complete policy facts when diagnostics are busy |

The current Local Code app probe established normal native CLI storage on one
Mac. Scoped fallback and Cowork authorization passed synthetic integration
tests. They still need fresh app-session acceptance on both Macs. Synthetic
private staging tests used real PostgreSQL and MinIO; they do not establish
complete private-history coverage.

## Remaining coverage gates

- [ ] Verify fresh Local Code and Cowork sessions on both Macs. Check local
  retrieval, intended shared eligibility or hold, provenance, and raw evidence.
- [ ] Verify append, a session created after collector startup, app restart,
  and collector restart. Check for duplicate live records and durable catch-up.
- [ ] Verify cross-Mac retrieval and hooks-disabled collection. Use isolated
  synthetic staging records for outages and destructive policy tests.
- [ ] Account for older missing Code files and the outside-root native source.
  Record exclusions and unavailable evidence without broadening roots silently.
- [ ] Compare retained CASS recovery evidence with native and shared history.
  Require archive origin and original-path proof before matching copies.
  A shared session ID alone is insufficient.
- [ ] Verify selected raw bytes and message text, order, and addresses against
  independently retained evidence. Record the sampling limits.
- [ ] Publish the aggregate acceptance result and remaining gaps for the owner
  to review before any CASS watcher or integration is removed.

Separate recovery-device reconciliation was previously deferred. Read-only
comparison does not authorize restriction changes, grants, imports, or deletion
on that device. Preserve the existing migration archives throughout the audit.

## Recovery and other deferred work

Off-host backup, independent key recovery, and complete production disaster
recovery remain deferred. The staging VM shares production's physical host.
Local deployment checkpoints do not protect against loss of that host. Follow
the [recovery plan](recovery.md) before claiming recovery is qualified or retiring
working keys and migration archives.

Parallel uploads remain a separate [qualification project](../parallel-sync.md).
Their throughput and shared-state safety gates cannot be inferred from the
successful serial rollout. Remote SSH/WSL collectors remain deferred.
