# Dormant spool reference owner

The private `withReferences` API defines one process-local owner for an actual
Store and Spool pair. It is not wired into capture, upload, or startup sweep.
Current runtime operations still use the serial Syncer. This API does not enable
parallel workers or certify concurrent runtime cleanup.

The first valid callback binds the Spool to its Store wrapper. Later callbacks
must use that same wrapper. Nil arguments and a different wrapper fail before
the callback runs. A newly opened wrapper needs a new Spool owner after the old
owner stops. The collector's process-level index lock remains required.

Use the supplied scope for publication, capture commit, and conditional release.
Keep it through commit or abort. Pass it explicitly to nested work. Do not
reacquire reference ownership inside a callback or retain the scope after the
callback returns. This is a private lexical contract, not a runtime lease or
reference cache.

The lock order is the reference mutex, then SQLite or the file mutex. Reference
queries complete before file inspection and unlink. No SQLite transaction spans
spool IO. No file bookkeeping lock spans a reference query. Neither mutex may
span network transport. Chunk and tail reads return independent byte copies
under the existing file mutex; this API adds no reader pins.

Owned chunk publication validates the supplied hash and any existing file before
reuse. Conditional chunk release queries only the supplied hashes in bounded
batches. Orphan manifests and invalid generation metadata stop release. Audit
rows beyond a cut generation's retained entry count are not pending references.
Regular file type, size, and body hash must validate before unlink and accounting
changes. Symlinks, directories, FIFOs, and corrupt bodies retain their charge.
A missing file causes no accounting decrement. Tail release retains a currently
needed identical hash and validates the immutable or canonical bytes before
removal. Cancellation and query failure do not authorize deletion.

The tests hold the owner at actual publication and reference-check boundaries.
They check real SQLite references, charged files, commit or guard rejection, and
cleanup followed by republish. These are API-level proofs. Production wiring,
cross-process spool ownership, general reclamation, parallel capture, and
power-loss durability remain separate work.
