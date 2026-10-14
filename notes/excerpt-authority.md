# Message reference authority and delivery

This investigation covers transcript excerpts attached to messages delivered
through device hooks. It leaves storage defaults, diagnostics, the redaction
command's multi-target result, and cloud delivery unchanged.

## Contract

A standalone device resolves references from its local transcript index. A
server-connected device resolves them through the receiving device's authorized
`GET /v1/read`, regardless of whether it keeps a local message index. Missing,
denied, ambiguous, or timed-out reads produce a reference address without an
excerpt. They never select a different corpus or fall back to local text.

The receiving device may spend at most one second preparing a batch of
references before importing new messages into its inbox. The hook still reads
only local state. An excerpt is optional; its failure cannot acknowledge,
delete, or consume a message.

Prepared excerpts are snapshots valid for at most 30 seconds from the start of
the batch read. The cache holds at most 128 entries of at most 640 text bytes
each, plus keys and metadata. It is memory-only. Each entry belongs to an
envelope ID, reference address, and receiving credential. A restart discards
excerpts and retains the existing durable inbox and receipt bookkeeping.

An invalidation increments an epoch and clears prepared text. A read can publish
its result only if its captured epoch and credential still match. Local
redaction requests and observed path-policy changes invalidate before and after
their operation. Repinning or rechecking credentials and failed message polls
also invalidate. An observed authorization denial (HTTP 401 or 403) clears
all snapshots and discards earlier successes in the same batch. The current
read API returns 403 for credential-level read-scope denial; invisible
individual addresses return 404. Repeated credential denial therefore keeps
all excerpts unavailable while ordinary message delivery continues. Still-valid
snapshots are reused without another read or an expiry extension.
Expired entries are removed before a later poll attempts their references again.
Missing, empty, or failed results leave those references without snapshots. A
batch deadline does not delete still-valid entries it did not read.

A server-side redaction or permission change on another device is not an
instantaneous local event. Until this device observes it, an already prepared
snapshot may be served within the 30-second window. Expiry is checked when the
hook takes the excerpt. Refetch begins only after expiry, when another poll
returns the envelope. Between expiry and the next successful read, the hook
prints the reference address without an excerpt. Text already inserted into an
agent's context cannot be recalled. Strict authorization at the instant of hook delivery would require a
different protocol and timing contract.

## Raw reproduction

The baseline is commit `007c29a`. The connected control-socket regression uses
synthetic transcripts, a real HTTP bus client, and an authenticated test read
endpoint. Its expected text is specified literally:

```
user: Synthetic authority excerpt with one precise fact.
```

| Fixture | Baseline result | Required result |
|---|---|---|
| Connected, full index | Local text; authenticated reader never called | Authorized shared text |
| Connected, sync-only | Message and ref, no excerpt | Same authorized shared text |
| Connected, ref only on another device | Message and ref, no excerpt | Same authorized shared text |
| Connected, read denied, local row present | Local text still displayed | Message and ref, no excerpt |

The first storage-only probe widened the local lookup budget to ten seconds.
The sync-only failure remained, excluding the hook deadline as its cause. The
connected regression then established the request-authority mismatch. A denied
synthetic read is a contract test; it does not establish exploitation of a live
server permission bug.

## Current execution

For the same delivered envelope `M`, let `R` name its transcript reference.

| Step | Server | Full device | Minimal device |
|---|---|---|---|
| 0 | `R=text`, `M=queued` | `index[R]=text`, `inbox={}` | `index[R]=absent`, `inbox={}` |
| 1: poll | Returns `M` | `inbox[M]=queued` | `inbox[M]=queued` |
| 2: pending | No read request | `M=leased`, excerpt=`index[R]` | `M=leased`, excerpt=absent |
| 3: confirm | Delivery receipt follows | `M=delivered` | `M=delivered` |

The divergence occurs at step 2. The hook's local-index access chooses both
data location and effective read authority. Giving the minimal device a larger
lookup budget cannot provide a row it intentionally does not store. Full
indexing also fails when `R` exists only on another device.

## Candidate execution

The poll loop prepares references from direct and claimable envelopes under one
batch deadline. Claimable messages still require the existing successful claim
before they enter the inbox. Preparation does not grant delivery ownership.
Preparing a claimable envelope consumes read budget even if this device later
loses the claim.

```
t0 server = {R: original}; inbox = {}; cache = {}; epoch = 4
t1 poll returns M; read begins with {epoch: 4, credential: C, start: t1}
t2 read returns authorized text
   cache[(M,R)] = {text, credential: C, expires: t1+30s}
   inbox[M] = queued
t3 hook takes M: inbox[M] = leased; excerpt = cache[(M,R)].text
t4 hook confirms: inbox[M] = delivered; receipt = owed
```

A read failure at `t2` leaves the cache empty and still imports `M`. No hook
performs the HTTP read. The existing lease, requeue, confirm, and read-receipt
transitions retain their meaning.

### Invalidation during a read

```
t0 read begins: captured epoch=4, credential=C
   cache={}; inbox[M]=queued
t1 local invalidation: epoch=5; cache={}
t2 old read returns original text, captured epoch=4
   4 != 5: discard result; cache={}; inbox[M]=queued
t3 hook takes M with its reference address and no excerpt
```

Credential replacement is checked the same way using the credential key.
Expiry uses the read's start time, so a delayed result cannot extend its own
freshness. A restart creates an empty cache and opens the unchanged inbox. If a
hook had leased a message before the restart, its existing lease expiry still
controls redelivery; refetching an excerpt neither confirms nor duplicates it.

## Executable acceptance criteria

- `TestControlPendingExcerptIndexModeContract`: independently specified text
  through the real control socket in full, sync-only, remote-only, and denied
  cases. The baseline fails; the candidate must pass.
- `TestReference*`: channel barriers force invalidation and credential changes
  during reads. A controlled clock exercises expiry. Tests inspect persisted
  inbox state through take, requeue, confirm, and reopen. Hook reads must not
  increment the network-reader call count. Cache entry and text bounds are
  asserted independently of the number of incoming refs.
- `TestReadReferenceUsesAuthorizedFocusAndAddressedLine`: the HTTP request uses
  the receiver's credential, requests bounded text, selects the focus rather
  than neighbors, and respects addressed lines and denied/missing results.
- `TestReferenceTextBoundsSourceBeforeNormalization`: standalone and shared
  paths use the same 640-byte source window, normalization, and empty-result
  handling. Long blank prefixes do not produce a role-only excerpt.
- `TestBusConnectFollowsARotationInFlight/reference`: a refused old credential
  retries only after a saved replacement exists.
- `TestTwoDeviceSync/zz-cross-device-ref-excerpts`: real separate device homes,
  indexes, credentials, agents, pinned TLS, PostgreSQL, and object storage.
  The sender-only transcript must appear in actual hook output on a full and a
  sync-only receiver. SQL proves that neither receiver has local rows for the
  referenced session; sync-only must have no message rows or search shards.
  The test records actual hook duration separately from enrichment time.

The Go race detector checks the changed concurrent paths. Race barriers use
channels; wall-clock sleeps do not choose which actor wins. Timeouts only bound
test failure or exercise the actual enrichment deadline. Routine run results
belong in the PR evidence, separate from this behavior contract.

## Remaining boundaries

This change does not make all local and shared operations conformant. It
establishes one authority boundary with executable evidence. It does not change
the default storage mode or authorize a migration of existing devices. Other
features should reuse the rule: choose request authority explicitly, then treat
local retention as storage policy. Any future persistent excerpt cache would
need its own deletion and recovery contract.

The one-second and 30-second values are product bounds, not measured universal
optima. Reusing valid entries lets later missing references use the read budget.
Stable poll ordering, slow or repeatedly failing earlier references, and the
128-entry bound can still prevent later references from receiving excerpts.
This cache provides no universal fairness guarantee. Successful local tests
establish the exercised schedules and workload;
they do not establish production tail latency or instantaneous revocation.
