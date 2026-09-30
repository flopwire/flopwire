# Launch-critical state-machine walks

Salvaged from the CASS-era launch gate (PR #2, `origin/launch/readiness`) with
the CASS gates edited out. The "current broken flow" walks describe the
segment-era code that B1 replaced; they stay as the failure cases the new code
must not reintroduce. B1c implements the deletion and backup walks on chunks:
a segment below maps to a chunk, a session to a conversation, and "search
hides S" to the request transaction deleting the conversation's message rows.
The quota and collector walks are constraints for B2/B3.

## Invariants

1. A tombstoned conversation is never searchable or raw-readable.
2. A successful deletion request is durably auditable before the API replies.
3. Physical deletion is idempotent and resumes after a crash.
4. A completed backup contains every raw object referenced by its active-corpus
   database snapshot, with matching size and digest.
5. Physical deletion cannot race an online backup's object copy.
6. An incomplete restore target never receives production traffic.
7. Concurrent accepted uploads cannot exceed their applicable quota boundary.
8. A collector never reports a fully successful pass when eligible data failed
   or paused.
9. Search never claims a byte range is searchable before it has been parsed
   into message rows; archived bytes with a missing baseline remain explicitly
   retrieval-pending.

## Deletion

### Current broken flow: object cleanup fails

```text
t=0  api      = {request: DELETE session S, response: open}
     postgres = {segments: [A, B], audit: [], tombstone: none}
     minio    = {A: present, B: present}
     search   = {S: visible}

t=1  postgres transaction deletes A and B, then commits
     postgres = {segments: [], audit: [], tombstone: none}
     minio    = {A: present, B: present}
     search   = {S: hidden only because metadata vanished}

t=2  RemoveObject(A) succeeds; RemoveObject(B) times out
     postgres = {segments: [], audit: [], tombstone: none}
     minio    = {A: absent, B: unknown/present}

t=3  store returns an error; API maps every error to 404 and does not audit
     api      = {response: 404 "session not found"}
     postgres = {segments: [], audit: []}
     minio    = {B: orphaned and no longer addressable from Postgres}
     ⚠ invariants 2 and 3 fail; the operator received a false result and no
       durable state exists from which cleanup can resume
```

### Fixed flow: queued deletion succeeds

Proposed durable state:

```text
session_tombstones = {session_id, requested_by, requested_at}
deletion_jobs = {
  id, session_id, state: queued|purging|retry_wait|complete|failed,
  attempts, next_attempt_at, last_error, requested_at, completed_at
}
```

```text
t=0  api      = {request: DELETE S, response: open}
     postgres = {segments: [A, B], tombstone: none, job: none, audit: []}
     minio    = {A: present, B: present}

t=1  one Postgres transaction inserts tombstone(S), job(J, queued), and
     audit(deletion.requested, J), then commits
     postgres = {segments: [A, B], tombstone: S, job: J/queued,
                 audit: [deletion.requested]}
     search/raw-read predicates exclude S immediately

t=2  api returns 202 {job: J, state: queued}
     ✓ invariants 1 and 2 hold before the response

t=3  worker leases J and acquires the global deletion-purge advisory lock
     postgres = {job: J/purging, attempts: 1}

t=4  worker calls idempotent RemoveObject(A), RemoveObject(B)
     minio    = {A: absent, B: absent}

t=5  one Postgres transaction deletes S's segment rows, marks J complete, and
     appends audit(deletion.complete, J), then commits
     postgres = {segments: [], tombstone: S, job: J/complete,
                 audit: [deletion.requested, deletion.complete]}

t=6  worker releases purge lock; API polling reports complete
     ✓ all deletion invariants hold
```

The tombstone remains indefinitely as the minimal non-secret record preventing
session-ID resurrection and proving the deletion boundary.

### Fixed flow: crash and retry

```text
t=0..3  same as the fixed success flow

t=4  RemoveObject(A) succeeds; process crashes before B
     postgres = {segments: [A, B], tombstone: S, job: J/purging}
     minio    = {A: absent, B: present}
     search   = {S: hidden by tombstone}

t=5  lease expires; restarted worker leases J, increments attempts, and
     reacquires the purge lock

t=6  RemoveObject(A) reports absent/success; RemoveObject(B) succeeds
     minio    = {A: absent, B: absent}

t=7  terminal Postgres transaction removes metadata, completes J, and audits
     ✓ retry reaches the same legal state as the no-crash flow
```

If object cleanup returns a retryable error, the worker records `retry_wait`, a
sanitized error class, exponential backoff, and an audit event. Exhausting the
automatic retry budget sets `failed` but preserves the tombstone and permits an
explicit admin retry.

## Online backup and restore

### Current broken flow: deletion races object enumeration

```text
t=0  postgres = {segments: [A, B]}
     minio    = {A: present, B: present}

t=1  pg_dump captures snapshot {segments: [A, B]}

t=2  admin deletion commits metadata removal for B and deletes object B
     postgres.live = {segments: [A]}
     minio         = {A: present, B: absent}

t=3  backup lists current MinIO objects and copies only A
     backup = {database references: [A, B], objects: [A]}

t=4  manifest hashes all files it happens to contain and verification passes
     ⚠ invariant 4 fails even though `backup-verify` reports success
```

### Fixed online snapshot lease

The backup command uses one dedicated Postgres connection for a session-level
shared advisory lock. Deletion workers require the corresponding exclusive
lock before physical object purge. Uploads do not acquire this lock.

```text
t=0  postgres = {active segments: [A, B], tombstones: [], uploads: enabled}
     minio    = {A: present, B: present}

t=1  backup acquires shared purge lock L

t=2  backup begins REPEATABLE READ, READ ONLY transaction T, exports snapshot X,
     and queries the exact active object inventory I=[A(size,sha), B(size,sha)]

t=3  pg_dump --snapshot X writes the database snapshot while T remains open

t=4  a new upload C commits
     postgres.live = {A, B, C}; minio = {A, B, C}
     snapshot X and inventory I remain {A, B}; C correctly belongs to the next
     backup

t=5  deletion request for B commits its tombstone and queued job. Search hides
     B immediately. Its worker blocks on exclusive lock L and cannot purge B.

t=6  pg_dump completes; T commits. Backup copies exactly I and verifies each
     byte count and digest against the inventory.

t=7  backup fsyncs files, writes manifest last through atomic rename, records
     backup.complete, and releases shared lock L
     backup = {database X, inventory [A,B], objects [A,B], state: complete}
     ✓ invariants 4 and 5 hold

t=8  deletion worker acquires exclusive L and purges B
```

An interrupted backup never creates the final manifest and is therefore not a
valid backup. The output directory records an incomplete state and can be
removed or resumed according to its manifest version. Production invocation
requires an explicit assertion that the destination is encrypted.

### Empty-target restore

```text
t=0  source backup = {verified database X, inventory I, objects I}
     target.db     = {schema/data: empty}
     target.minio  = {bucket: empty}
     traffic       = {points to old deployment}

t=1  restore verifies hashes, manifest version, target emptiness, and required
     encryption acknowledgement before writing

t=2  restore uploads and re-verifies every object in I
     target.minio = {objects: I}; target.db = empty

t=3  restore runs pg_restore without --clean into the empty database
     target.db = X; target.minio = I

t=4  validation checks every active segment reference, raw byte digest,
     canonical provenance sample, and expected aggregate counts

t=5  message rows are checked (or rebuilt from manifests); known-result and
     tombstone-negative queries pass

t=6  restore.complete is audited; operator explicitly switches traffic
     ✓ invariant 6 holds throughout because the partial target was never live
```

Any failure before t=6 invalidates the fresh target. V1 does not resume or
overwrite a partial restore; operators discard the target and start with new
empty stores.

## Quotas and collector state

### Current broken flow: concurrent overshoot

```text
policy   = {hard: 1000, backfill_soft: not modeled}
postgres = {usage: 850}
upload_1 = {lane: backfill, bytes: 100}
upload_2 = {lane: backfill, bytes: 100}

t=1  upload_1 reads usage=850; computes 850+100 <= 1000; accepts
t=2  upload_2 reads usage=850; computes 850+100 <= 1000; accepts
t=3  both transactions commit
     postgres = {usage: 1050}
     ⚠ invariant 7 fails

t=4  a live append arrives and is rejected after backfill consumed the hard cap
```

### Fixed serialized enforcement

Policy gains `live_reserve_percent`, default 10. The applicable boundaries are
`backfill_limit=hard*(100-reserve)/100` and `live_limit=hard`. A row lock on the
singleton policy serializes usage calculation and segment metadata commit.

```text
policy   = {hard: 1000, reserve: 10%, backfill_limit: 900}
postgres = {usage: 850}
upload_1 = {lane: backfill, bytes: 100}
upload_2 = {lane: backfill, bytes: 100}

t=1  upload_1 locks policy row, reads usage=850
     850+100 > 900 → reject as backfill_paused; unlock

t=2  upload_2 locks policy row, reads usage=850
     850+100 > 900 → reject as backfill_paused; unlock

t=3  live append L={bytes:100} locks policy row, reads usage=850
     850+100 <= 1000 → object write and metadata commit; unlock
     postgres = {usage: 950}
     ✓ backfill preserved the live reserve and hard quota did not overshoot
```

The object write occurs while the quota transaction is open. This reduces
throughput but preserves the V1 invariant on a single node. A later design may
replace serialization with durable byte reservations.

### Archived live tail with incomplete baseline

```text
enrollment baseline = {source F, size: 10 MB, uploaded coverage: none}
quota                = {backfill: paused, live space: available}

t=1  F appends 100 KB at offset 10 MB
     collector = {baseline_size: 10 MB, coverage: [], live_tail: [10MB,10.1MB]}

t=2  server accepts the raw live segment under the hard limit
     archive = {tail object: durable}
     session = {coverage: [10MB,10.1MB], retrieval: pending_baseline}
     messages = {none}; the parser is never given a sparse file

t=3  API and collector report archived=true, searchable=false,
     reason=retrieval_pending_baseline

t=4  quota later permits baseline chunks. Coverage becomes continuous
     [0,10.1MB] and the server parses the generation into message rows.

t=5  parse succeeds; session transitions retrieval=ready
     ✓ invariants 8 and 9 hold; the UI never claimed the tail was searchable
```

For mutable whole-document sources such as observed Gemini JSON, every content
hash is a complete source version. Initial content is backfill; a post-enrollment
rewrite is live and can index independently if it fits under the hard limit.
Same-size rewrites are detected by digest, not size.

### Collector pass result

Collector state records per source/version:

```text
{discovery, lane, coverage, archive_state, retrieval_state, paused_reason,
 last_attempt_at, next_retry_at, error_class}
```

One pass returns aggregate `complete`, `partial`, or `paused`, plus counts. A
one-shot CLI exits nonzero for `partial`; quota-only `paused` uses a distinct
documented exit code. Watch mode remains alive but changes health and logs only
sanitized identifiers and error classes.
