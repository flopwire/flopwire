# History coverage reporting

Coverage reports separate local collection, uploads, server parsing, and
policy holds. Each section reports an observed snapshot. Missing diagnostics
are unknown. Empty queues do not establish exhaustive discovery.

## Scope

Collection counts describe the current device's retained index. They do not
prove that every intended transcript folder is watched or that a file that
has not been discovered is absent.

Upload counts distinguish scheduled source checks from captured data awaiting
acknowledgment. A scheduled check can discover an unchanged file. Spool bytes
are local storage usage, not a measure of remaining upload bytes.

Server parsing describes the authenticated device's retained server sources.
It is not a receipt linking a local capture to an exact indexed revision.
Other devices contributing to shared retrieval remain outside this snapshot.

Policy holds describe history that collection rules deliberately withhold.
A busy or unavailable policy diagnostic does not mean that there are no holds.
Lightweight retrieval reports can expose durable historical mapping evidence
without claiming to know current sharing eligibility. Ordinary status includes
detailed Cowork holds when its existing diagnostic is available.

Lost generations and shortened capture prefixes are separate retained facts.
Their counts can overlap. Known retained server copies describe local policy
bookkeeping, not the server's complete inventory.

Untracked server sources are active native sources with no observed applied
parser. They can also be pending. Companions and tombstones do not count as
untracked native sources. Do not add these counts as disjoint totals.

## State trace

The following trace uses one transcript and one intentionally held Cowork
session. Each diagnostic can observe a different instant.

| Step | Collector | Upload ledger | Server parser | Report implication |
|---|---|---|---|---|
| 0 | File grew from 100 to 200 bytes; one check scheduled | Previous 100-byte capture acknowledged | Requested 7, parsed 7 | Scheduled checks remain; previously captured uploads and parsing are caught up |
| 1 | Capture running | New 200-byte capture has 100 bytes awaiting acknowledgment | Requested 7, parsed 7 | Upload remains pending although parsing currently has no backlog |
| 2 | Capture complete; check queue empty | New capture fully acknowledged | Requested 8, parsed 7 | Upload caught up; server parsing pending |
| 3 | No scheduled checks | Capture fully acknowledged | Requested 8, parsed 8 | Both snapshots caught up; discovery still unproven and Cowork still held |
| 4 | Optional ledger read reaches its deadline | Ledger diagnostic unavailable | Parser snapshot available | Upload totals unknown; preserve available scheduler and parser facts |
| 5 | Policy lock busy | No eligible Cowork upload | Parser snapshot available | Hold counts unknown; do not report zero holds |
| 6 | Retrieval succeeds while diagnostics are running | Diagnostic not ready | Older server lacks coverage endpoint | Return retrieval results; coverage unknown; no local fallback |
| 7 | No source checks remain | An old unacknowledged generation lost its chunks | Parser snapshot available | Report lost captures separately; zero pending upload work is not successful delivery |

No step permits an overall claim that shared history is complete.

## Compatibility

The server endpoint is additive and requires a device-bound credential with
read permission. It returns metadata for that device only. Old servers and
unavailable agents produce unknown diagnostics. Search results keep their
original backend scope, query semantics, and error behavior.
