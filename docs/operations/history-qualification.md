# History qualification before CASS retirement

Status: incomplete, October 8, 2026. Collection and coverage reporting are
deployed. This record does not authorize stopping CASS, removing integrations,
changing exclusions, or deleting archives and recovery keys.

## Shipped behavior

Both Macs collect ordinary native coding history and Claude Desktop Local Code.
Cowork collection is deployed on both Macs. Fresh Cowork acceptance is required
on the second Mac; the owner does not use Cowork on the first Mac. Ordinary
Claude chat and remote SSH/WSL collection are outside the selected scope. Cowork inherits the strictest rule anywhere within its selected
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

The October 8 Local Code app check on the first Mac created a session after
collector startup and appended a second synthetic prompt. Both prompts and
replies appeared in local and shared Claude retrieval, in the same session.
This establishes creation and append for that surface. It does not establish
app restart, collector restart, or hooks-disabled discovery. Both second-Mac
app sessions passed UI creation and append. Local Code markers appeared in
local and shared retrieval; Cowork collection remains under investigation.
Cross-Mac shared retrieval of the first-Mac Code markers passed. The owner
authorized app restart checks on the second Mac and deferred them on the first. Scoped fallback
and Cowork authorization passed synthetic integration tests. Synthetic
private staging tests used real PostgreSQL and MinIO; they do not establish
complete private-history coverage.

## Remaining coverage gates

- [ ] Finish fresh Local Code acceptance on both Macs and Cowork on the second
  Mac. First-Mac Local Code creation and append passed. Check provenance and raw
  evidence, plus intended shared eligibility or hold. First-Mac Cowork is unused.
- [ ] Verify append, a session created after collector startup, app restart,
  and collector restart. Check for duplicate live records and durable catch-up.
- [ ] Verify cross-Mac retrieval and hooks-disabled collection. Use isolated
  synthetic staging records for outages and destructive policy tests.
- [ ] Account for older missing Code files and the outside-root native source.
  Record exclusions and unavailable evidence without broadening roots silently.
- [x] Verify the selected CASS recovery exports through origin, original path,
  raw bytes, and indexed messages. See the bounded audit below. This does not
  prove all native history or providers outside the selected recovery set.
- [x] Compare eight selected native Claude/Codex records across both Macs with
  local and shared raw reads and extracted message identities and text. All
  matched. This sample excludes Devin and does not prove whole-history equality.
- [ ] Publish the aggregate acceptance result and remaining gaps for the owner
  to review before any CASS watcher or integration is removed.

## October 8 retained recovery audit

The read-only audit verified all **11,722 selected Claude recovery exports and
94,900 expected indexed messages**: 30 exports from the first Mac and 11,692
from the second. Manifest origin and original-path associations, export file
hashes, raw source reconstruction, and message identities were checked.

Of these exports, 11,715 match original bytes exactly. Seven contain only
length-preserving redactions. All indexed message text matches the archived
post-redaction text. The second-Mac chunk-backed subset contains 136 sources;
all 328 committed objects passed BLAKE3 address and decoded-size checks.
Provisional-tail generations need not have their completion flag set; this
proof used acknowledged extents and reconstructed bytes.

Thirty first-Mac exports have ambiguous conversation-level origin metadata
because imports with a shared session ID coalesced. Their source and individual
message provenance and bytes were verified independently. Conversation origin
or a shared session ID alone remains insufficient proof.

The owner chose to drop **103 older supported CASS records** from recovery
scope: 61 Devin, 21 Codex, and 21 Code-app records without proven shared
original-path association. This is an accepted coverage gap, not a successful
migration claim. No additional import or snapshot deletion is required by that
decision. Retained snapshots remain intact.

The owner also chose to drop **180 CASS records from other providers** from
migration scope. They remain outside selected native collection and recovery
scope. Retained snapshots stay intact; this decision does not claim migration. The selected export proof and eight native
samples do not establish exhaustive discovery, all native message equality, or
a complete backup restore. CASS retirement remains pending live acceptance and
owner review of the remaining boundaries.

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
